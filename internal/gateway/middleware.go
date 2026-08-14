package gateway

import (
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/platform/authn"
	"github.com/MS-Arcadia/api-gateway/internal/platform/errs"
	"github.com/MS-Arcadia/api-gateway/internal/platform/idgen"
	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
)

// Header names, matching what the services already read and write.
const (
	HeaderRequestID     = "X-Request-Id"
	HeaderCorrelationID = "X-Correlation-Id"
	HeaderRateLimit     = "X-RateLimit-Remaining"
	HeaderRetryAfter    = "Retry-After"
)

// Correlation gives every request an id and puts it on the way out.
//
// Generated here when the caller did not send one, which is the point of doing it
// at the edge: after this, one id covers the gateway's own log line and all eight
// services' lines for the same request. It is echoed back so a client can quote it
// in a bug report.
func Correlation(next http.Handler) http.Handler {
	// UUIDv7 rather than v4: the platform's ids sort by creation time, which makes a
	// log or a database index ordered by id also ordered by when it happened.
	ids := idgen.UUIDv7{}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		correlationID := r.Header.Get(HeaderCorrelationID)
		if correlationID == "" {
			correlationID = ids.NewID()
		}
		requestID := r.Header.Get(HeaderRequestID)
		if requestID == "" {
			requestID = ids.NewID()
		}

		w.Header().Set(HeaderCorrelationID, correlationID)
		w.Header().Set(HeaderRequestID, requestID)

		ctx := logx.WithCorrelationID(r.Context(), correlationID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AccessLog records one line per request, after it finishes.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			started := time.Now()

			// Put the configured logger on the context, so this line and anything
			// logged further down the chain carry the correlation id Correlation
			// just minted. Without it slog.InfoContext passes the context to a
			// handler that ignores it, and the id — propagated in the header,
			// returned to the caller, forwarded upstream — appeared in no log line
			// the gateway itself wrote.
			ctx := logx.WithLogger(r.Context(), logger)
			r = r.WithContext(ctx)

			recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(recorder, r)

			// The path is logged, the query string is not: a query can carry a search
			// term or an email, and an access log is the least protected place on the
			// platform.
			logx.FromContext(ctx).InfoContext(ctx, "request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.status,
				"duration_ms", time.Since(started).Milliseconds(),
			)
			observeRequest(r.URL.Path, r.Method, recorder.status, time.Since(started))
		})
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if s.written {
		return
	}
	s.status = status
	s.written = true
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	s.written = true
	return s.ResponseWriter.Write(b)
}

// CORS answers preflights and marks responses for the browser.
//
// An allow-list, not a wildcard, and not because the platform has cross-origin
// secrets to protect — the token lives in localStorage rather than a cookie, so
// `Allow-Credentials` is not needed. It is an allow-list because `*` plus a bearer
// token means any page on the internet can call this API with a token it has
// phished, and the browser will let it.
func CORS(allowed []string) func(http.Handler) http.Handler {
	allowAll := slices.Contains(allowed, "*")

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			permitted := origin != "" && (allowAll || slices.Contains(allowed, origin))

			if permitted {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				// Vary, because a cache that stored one origin's response and served
				// it to another would defeat the allow-list entirely.
				w.Header().Add("Vary", "Origin")
				// PUT is here because the platform serves it: review-service edits a
				// review with it and community-service sets a reaction with it. It was
				// missing, so the browser's preflight came back without it and every
				// reaction and review edit died as an opaque "CORS error" — a failure
				// that names neither the method nor this file.
				w.Header().Set("Access-Control-Allow-Methods",
					"GET, POST, PUT, PATCH, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers",
					strings.Join([]string{
						"Authorization", "Content-Type", "Idempotency-Key",
						HeaderCorrelationID, HeaderRequestID,
					}, ", "))
				w.Header().Set("Access-Control-Expose-Headers",
					strings.Join([]string{HeaderCorrelationID, HeaderRequestID, HeaderRateLimit}, ", "))
				w.Header().Set("Access-Control-Max-Age", "600")
			}

			if r.Method == http.MethodOptions {
				// A preflight from an origin that is not allowed gets 403 with a problem
				// document rather than 204-without-headers. The browser blocks it either
				// way and never shows the body to the page — but the body is what a
				// developer with curl, and the access log, actually get to read, and
				// "403" alone does not say which of the two things went wrong.
				if !permitted {
					errs.WriteProblem(w,
						errs.PermissionDenied("origin %q is not allowed", origin).
							WithReason("ORIGIN_NOT_ALLOWED"),
						logx.CorrelationID(r.Context()))
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit is a per-client token bucket.
//
// In memory, and that is a decision rather than a shortcut. With one gateway
// instance an in-memory bucket is exact and costs no network hop per request; with
// several it becomes per-instance and the effective limit multiplies. The trade is
// worth stating because the architecture document asks for a Redis sliding window,
// and Redis is already running: this is the right implementation for one instance
// and the wrong one for three.
//
// It is also a *different job* from the auth service's own limiter. That one counts
// registrations and logins per IP to make credential stuffing expensive. This one
// counts everything, to stop one client saturating eight services. Two mechanisms
// because they answer different questions — and collapsing them would mean either
// a browsing user tripping a login limit or a password guesser hiding inside a
// generous global one.
type RateLimit struct {
	capacity int
	refill   time.Duration

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// NewRateLimit allows `perMinute` requests per client, bursting to the same.
func NewRateLimit(perMinute int) *RateLimit {
	if perMinute <= 0 {
		perMinute = 600
	}
	return &RateLimit{
		capacity: perMinute,
		refill:   time.Minute / time.Duration(perMinute),
		buckets:  make(map[string]*bucket),
	}
}

// Middleware enforces the limit.
func (l *RateLimit) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Preflights are not requests a client controls the rate of — the browser
		// sends them — so counting them would have a page trip the limit by being
		// used normally.
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		client := clientKey(r)
		remaining, ok := l.take(client)
		w.Header().Set(HeaderRateLimit, strconv.Itoa(remaining))

		if !ok {
			w.Header().Set(HeaderRetryAfter, strconv.Itoa(int(l.refill.Seconds())+1))
			observeRateLimited()
			errs.WriteProblem(w,
				errs.ResourceExhausted("too many requests").WithReason("RATE_LIMITED"),
				logx.CorrelationID(r.Context()))
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (l *RateLimit) take(client string) (int, bool) {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, seen := l.buckets[client]
	if !seen {
		b = &bucket{tokens: float64(l.capacity), lastSeen: now}
		l.buckets[client] = b
	}

	// Refill for the time elapsed, capped at the bucket's size.
	elapsed := now.Sub(b.lastSeen)
	b.tokens = min(b.tokens+elapsed.Seconds()/l.refill.Seconds(), float64(l.capacity))
	b.lastSeen = now

	if b.tokens < 1 {
		return 0, false
	}
	b.tokens--
	return int(b.tokens), true
}

// Sweep drops buckets nobody has touched, so the map does not grow with every
// address that has ever called. Started once at boot; a gateway that leaked a
// bucket per client would be a slow memory leak with a very long fuse.
func (l *RateLimit) Sweep(every, idle time.Duration) func() {
	ticker := time.NewTicker(every)
	done := make(chan struct{})

	go func() {
		for {
			select {
			case <-ticker.C:
				cutoff := time.Now().Add(-idle)
				l.mu.Lock()
				for key, b := range l.buckets {
					if b.lastSeen.Before(cutoff) {
						delete(l.buckets, key)
					}
				}
				l.mu.Unlock()
			case <-done:
				ticker.Stop()
				return
			}
		}
	}()

	return func() { close(done) }
}

func clientKey(r *http.Request) string {
	// The gateway is the outermost hop in this deployment, so RemoteAddr is the
	// real peer. Trusting X-Forwarded-For here instead would let any caller pick
	// their own rate-limit bucket by sending a header.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// VerifyToken rejects a token that is malformed, expired, or not an access token —
// but does **not** require one.
//
// This is the important line in the whole gateway. The obvious design is for the
// edge to enforce authentication, which needs a list of which routes are public:
// login, register, browsing the catalogue. That list is a second copy of knowledge
// the services already hold, and two lists that must agree will drift — this
// platform lost a notification that way last week, when a translator existed and
// its router registration did not.
//
// So the gateway verifies a token *if one is present* and passes the absence
// through untouched. Garbage dies at the edge, the services stay the authority on
// what needs a credential, and there is no route table here to fall out of step.
//
// It is a second layer, never the only one: every service verifies again.
func VerifyToken(verifier *authn.Verifier, logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			header := r.Header.Get("Authorization")
			if header == "" {
				next.ServeHTTP(w, r)
				return
			}

			token, ok := bearer(header)
			if !ok {
				reject(w, r, errs.Unauthenticated("the Authorization header is not a bearer token").
					WithReason("TOKEN_INVALID"))
				return
			}

			principal, err := verifier.Verify(token)
			if err != nil {
				// The verifier's own reason codes are kept: they are the same ones the
				// services return, so a client cannot tell — and does not need to —
				// whether the refusal came from here or from behind here.
				logx.FromContext(r.Context()).DebugContext(r.Context(), "token refused at the edge",
					"path", r.URL.Path, "error", err.Error())
				observeTokenRejected()
				errs.WriteProblem(w, err, logx.CorrelationID(r.Context()))
				return
			}

			// The subject is logged, the token is not.
			logx.FromContext(r.Context()).DebugContext(r.Context(), "token accepted",
				"subject", principal.UserID, "role", string(principal.Role))
			next.ServeHTTP(w, r)
		})
	}
}

func bearer(header string) (string, bool) {
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	return strings.TrimSpace(header[len(prefix):]), true
}

func reject(w http.ResponseWriter, r *http.Request, err error) {
	errs.WriteProblem(w, err, logx.CorrelationID(r.Context()))
}

// Chain applies middleware in the order given, so the first is outermost.
func Chain(handler http.Handler, middleware ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middleware) - 1; i >= 0; i-- {
		handler = middleware[i](handler)
	}
	return handler
}
