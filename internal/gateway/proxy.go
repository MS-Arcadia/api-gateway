package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/platform/errs"
	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
)

type ctxKey string

const ctxForwardedPath ctxKey = "gateway.forwarded-path"

// Proxy forwards a request to the upstream that owns its prefix.
type Proxy struct {
	table   *Table
	proxies map[string]*httputil.ReverseProxy
	logger  *slog.Logger
}

// NewProxy builds one reverse proxy per upstream over a shared transport.
//
// One `ReverseProxy` each rather than a single instance with a switching director:
// the per-upstream error handler is what turns a dead service into a 503 naming
// which service, and one shared director would lose that. They share a transport,
// so connection pooling is platform-wide rather than per-service.
func NewProxy(table *Table, logger *slog.Logger, timeout time.Duration) *Proxy {
	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		// The services speak plain HTTP inside the compose network, so there is no
		// TLS handshake to amortise — but keeping connections alive still matters:
		// without it every request pays a fresh TCP handshake to the same host.
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	proxies := make(map[string]*httputil.ReverseProxy, len(table.Upstreams()))
	for _, upstream := range table.Upstreams() {
		up := upstream
		proxies[up.Name] = &httputil.ReverseProxy{
			Transport: transport,
			Rewrite: func(r *httputil.ProxyRequest) {
				r.SetURL(up.Target)
				// SetURL joins the target's path with the inbound one. The path to
				// forward was already decided by the routing table, so it is set
				// outright rather than joined.
				if forwarded, ok := r.In.Context().Value(ctxForwardedPath).(string); ok && forwarded != "" {
					r.Out.URL.Path = forwarded
					r.Out.URL.RawPath = ""
				}
				r.Out.Host = up.Target.Host
				// X-Forwarded-*, so a service logging a client address logs the
				// caller's rather than the gateway's.
				r.SetXForwarded()
				// The correlation id travels with the request. Every service already
				// reads this header and stamps it on its own lines, which is what
				// makes "show me everything for this purchase" answerable across
				// eight processes.
				if id := logx.CorrelationID(r.In.Context()); id != "" {
					r.Out.Header.Set(HeaderCorrelationID, id)
				}
			},
			ModifyResponse: func(response *http.Response) error {
				// The services set this header on their own responses too, and
				// ReverseProxy *appends* the upstream's headers onto a ResponseWriter
				// that the Correlation middleware has already written one to. The result
				// is a response carrying two X-Correlation-Id values, and a client
				// reading "the" correlation id gets whichever its HTTP library returns
				// first.
				//
				// Dropping the upstream's copy leaves the gateway's, which is the id the
				// access log recorded and therefore the one worth quoting in a bug
				// report. Setting it here instead would not work: it would be appended
				// to the middleware's, not replace it.
				response.Header.Del(HeaderCorrelationID)

				// A redirect must point somewhere the caller can actually reach.
				//
				// A service builds `Location` from the host it was addressed by, which is
				// its cluster name — so FastAPI's trailing-slash redirect on one review
				// route answered `http://review-service:8088/api/reviews/`. A browser on
				// https://arcadia.aptcodegen.online follows that to an address that does
				// not resolve, over a scheme the page will not allow, and posting a review
				// fails in a way that looks like nothing at all.
				//
				// Rewriting it back through this gateway keeps the public origin the only
				// address a client ever sees, and stops every 3xx from advertising an
				// internal hostname and port to whoever asked.
				if location := response.Header.Get("Location"); location != "" {
					if rewritten, ok := publicLocation(location, up); ok {
						response.Header.Set("Location", rewritten)
					}
				}
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				// A dead upstream is 503 and names the service, because "the store is
				// down" and "the wallet is down" need different people. 502 would be
				// defensible and less useful.
				if errors.Is(err, context.Canceled) {
					// The client hung up. Nothing to report and nobody to report it to.
					return
				}

				failure := errs.Unavailable("%s is not reachable", up.Name).
					WithReason("UPSTREAM_UNAVAILABLE").
					WithDetail("upstream", up.Name)
				if errors.Is(err, context.DeadlineExceeded) {
					failure = errs.DeadlineExceeded("%s did not answer in time", up.Name).
						WithReason("UPSTREAM_TIMEOUT").
						WithDetail("upstream", up.Name)
				}

				logger.WarnContext(r.Context(), "upstream failed",
					"upstream", up.Name,
					"path", r.URL.Path,
					"error", err.Error(),
				)
				observeUpstreamFailure(up.Name)
				errs.WriteProblem(w, failure, logx.CorrelationID(r.Context()))
			},
		}
	}

	return &Proxy{table: table, proxies: proxies, logger: logger}
}

// ServeHTTP routes one request.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upstream, forwarded, ok := p.table.Lookup(r.URL.Path)
	if !ok {
		// 404 rather than a default upstream. A gateway that guesses where an unknown
		// path belongs sends the request to a service that never asked for it, and
		// the resulting error then comes from the wrong place.
		errs.WriteProblem(w,
			errs.NotFound("no service is mapped to %s", r.URL.Path).WithReason("NO_ROUTE"),
			logx.CorrelationID(r.Context()))
		return
	}

	proxy := p.proxies[upstream.Name]
	if proxy == nil {
		errs.WriteProblem(w,
			errs.Internal("no proxy was built for %s", upstream.Name).WithReason("NO_PROXY"),
			logx.CorrelationID(r.Context()))
		return
	}

	ctx := context.WithValue(r.Context(), ctxForwardedPath, forwarded)
	proxy.ServeHTTP(w, r.WithContext(ctx))
}

// Table exposes the routing table, for the health checks and the root index.
func (p *Proxy) Table() *Table { return p.table }

// publicLocation turns an upstream's redirect target back into a public one.
//
// Only two forms are touched, and both are the service pointing at itself:
//
//	http://review-service:8088/api/reviews/  ->  /reviews/api/reviews/
//	/api/reviews/                            ->  /reviews/api/reviews/
//
// The result is relative on purpose. The gateway knows which prefix a service answers but
// not the scheme or hostname the caller reached it by — that is the ingress's business, and
// guessing it wrong is how a redirect lands on http from an https page. A relative Location
// is resolved by the client against the origin it actually used, which is always right.
//
// Anything else — a redirect to another site, an OAuth handoff — is returned untouched. A
// gateway that rewrote those would break them.
func publicLocation(location string, up Upstream) (string, bool) {
	target, err := url.Parse(location)
	if err != nil {
		return "", false
	}

	switch {
	case target.Host == up.Target.Host:
		// Absolute, and addressed to the upstream by its internal name.
	case target.Host == "" && strings.HasPrefix(target.Path, "/"):
		// Root-relative, so it is a path on the upstream by definition.
	default:
		return "", false
	}

	if !up.StripPrefix {
		return "", false
	}

	rewritten := *target
	rewritten.Scheme = ""
	rewritten.Host = ""
	rewritten.Path = up.Prefix + target.Path
	return rewritten.String(), true
}
