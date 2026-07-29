package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/MS-Arcadia/api-gateway/internal/platform/authn"
	"github.com/MS-Arcadia/api-gateway/internal/platform/health"
)

// Options configures the gateway's HTTP server.
type Options struct {
	ServiceName    string
	ServiceVersion string
	Port           int

	Table    *Table
	Verifier *authn.Verifier
	Logger   *slog.Logger

	CORSOrigins        []string
	RateLimitPerMinute int
	UpstreamTimeout    time.Duration
}

// Server is the gateway's public surface.
type Server struct {
	http      *http.Server
	health    *health.Registry
	rateLimit *RateLimit
	stopSweep func()
	logger    *slog.Logger
}

// New builds the server: operational routes, then everything else through the
// middleware chain and into the proxy.
func New(opts Options) *Server {
	proxy := NewProxy(opts.Table, opts.Logger, opts.UpstreamTimeout)
	limiter := NewRateLimit(opts.RateLimitPerMinute)
	probes := health.NewRegistry(opts.ServiceName, opts.ServiceVersion)

	// Readiness asks each upstream whether *it* is ready.
	//
	// Non-critical on purpose. A gateway that reports itself unready because one of
	// seven services is down would take the whole platform offline over a single
	// failure — the opposite of the bulkhead the architecture document asks for. So
	// a dead upstream degrades readiness and the gateway keeps serving the other
	// six, answering 503 for the one that is gone.
	for _, upstream := range opts.Table.Upstreams() {
		up := upstream
		probes.Register(health.Check{
			Name:     up.Name,
			Critical: false,
			Timeout:  2 * time.Second,
			Probe: func(ctx context.Context) error {
				return probeUpstream(ctx, up)
			},
		})
	}

	mux := http.NewServeMux()

	// Operational routes are registered directly and are *not* proxied, which is why
	// no upstream may claim these prefixes. `/livez` deliberately checks nothing: it
	// answers whether this process is alive, and a liveness probe that fails because
	// a dependency is down asks an orchestrator to restart the wrong container.
	mux.Handle("GET /livez", probes.LiveHandler())
	mux.Handle("GET /readyz", probes.ReadyHandler())
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /{$}", indexHandler(opts.Table, opts.ServiceName, opts.ServiceVersion))

	// Everything else goes through the chain. Order matters and is the design:
	//
	//   Correlation  first, so every line below — including a rate-limit refusal —
	//                carries the same id.
	//   AccessLog    next, so it times the whole thing including the refusals.
	//   CORS         before the limiter, so a browser gets usable headers even on a
	//                429; without this a rate-limited page sees an opaque CORS error
	//                instead of the 429 that explains itself.
	//   RateLimit    before the token check, so flooding with garbage tokens costs
	//                the gateway a map lookup rather than a signature verification.
	//   VerifyToken  last, closest to the proxy.
	mux.Handle("/", Chain(proxy,
		Correlation,
		AccessLog(opts.Logger),
		CORS(opts.CORSOrigins),
		limiter.Middleware,
		VerifyToken(opts.Verifier, opts.Logger),
	))

	return &Server{
		http: &http.Server{
			Addr:    fmt.Sprintf(":%d", opts.Port),
			Handler: mux,
			// Generous, because the media service streams multi-gigabyte uploads
			// through here. A 30-second write timeout would cut a real download.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		health:    probes,
		rateLimit: limiter,
		logger:    opts.Logger,
	}
}

// Handler is the composed handler: operational routes, the middleware chain and the
// proxy. Exposed so a test can exercise the whole composition without binding a
// port — ordering mistakes in the chain only show up once it is assembled.
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Start serves until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	// Buckets for addresses nobody has used in an hour, swept every ten minutes.
	// Without this the map grows with every address that has ever called — a slow
	// leak with a very long fuse.
	s.stopSweep = s.rateLimit.Sweep(10*time.Minute, time.Hour)
	s.health.MarkReady()

	errc := make(chan error, 1)
	go func() {
		s.logger.InfoContext(ctx, "gateway listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return s.Shutdown()
	}
}

// Shutdown drains in-flight requests, then stops.
func (s *Server) Shutdown() error {
	// Unready before draining, so a load balancer stops sending new work while the
	// requests already in flight finish.
	s.health.MarkShuttingDown()
	if s.stopSweep != nil {
		s.stopSweep()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return s.http.Shutdown(ctx)
}

// probeUpstream asks a service's own readiness endpoint.
//
// `/readyz` rather than a TCP dial: a listening port says the process started, and
// every service on this platform opens its port before it has a database.
func probeUpstream(ctx context.Context, up Upstream) error {
	target := up.Target.String() + "/readyz"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s is unreachable: %w", up.Name, err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d from /readyz", up.Name, response.StatusCode)
	}
	return nil
}

// indexHandler answers the root with the routing table.
//
// Not decoration: "which prefix goes where" is the first question anybody debugging
// this asks, and the alternative is reading a compose file to find out.
func indexHandler(table *Table, name, version string) http.HandlerFunc {
	type route struct {
		Prefix  string `json:"prefix"`
		Service string `json:"service"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		routes := make([]route, 0, len(table.Upstreams()))
		for _, up := range table.Upstreams() {
			routes = append(routes, route{Prefix: up.Prefix, Service: up.Name})
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service": name,
			"version": version,
			"routes":  routes,
		})
	}
}
