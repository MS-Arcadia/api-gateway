package gateway

import (
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The gateway's own metrics.
//
// Labelled by the public *prefix*, never by the full path. A histogram labelled
// with `/catalog/v1/games/{id}` where the id is real produces one time series per
// game — that is the classic cardinality explosion, and on a store it grows with
// the catalogue. The prefix is the useful grain anyway: "is the wallet slow" is a
// question about a service, not about one row.
var (
	requests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_requests_total",
		Help: "Requests handled, by public prefix, method and status class.",
	}, []string{"prefix", "method", "status"})

	latency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "gateway_request_duration_seconds",
		Help: "Time from accepting a request to finishing the response.",
		// Buckets chosen around the platform's own target: the architecture document
		// sets a p95 read latency of 300ms and alerts above 500ms, so the interesting
		// resolution is on either side of those.
		Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2, 5},
	}, []string{"prefix"})

	upstreamFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "gateway_upstream_failures_total",
		Help: "Requests that could not reach their upstream or timed out.",
	}, []string{"upstream"})

	rateLimited = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_rate_limited_total",
		Help: "Requests refused by the gateway's rate limiter.",
	})

	tokensRejected = promauto.NewCounter(prometheus.CounterOpts{
		Name: "gateway_tokens_rejected_total",
		Help: "Bearer tokens refused at the edge before reaching a service.",
	})
)

func observeRequest(path, method string, status int, took time.Duration) {
	prefix := prefixOf(path)
	requests.WithLabelValues(prefix, method, statusClass(status)).Inc()
	latency.WithLabelValues(prefix).Observe(took.Seconds())
}

func observeUpstreamFailure(upstream string) { upstreamFailures.WithLabelValues(upstream).Inc() }
func observeRateLimited()                    { rateLimited.Inc() }
func observeTokenRejected()                  { tokensRejected.Inc() }

// prefixOf reduces a path to its first segment, and only if it is one this gateway
// actually routes. Anything else collapses to "other", so a scanner probing random
// paths cannot create time series.
func prefixOf(path string) string {
	trimmed := strings.TrimPrefix(path, "/")
	first := trimmed
	if index := strings.Index(trimmed, "/"); index >= 0 {
		first = trimmed[:index]
	}
	switch "/" + first {
	case PrefixAuth, PrefixCatalog, PrefixOrders, PrefixWallet,
		PrefixPayment, PrefixMedia, PrefixNotifications:
		return "/" + first
	case "/livez", "/readyz", "/metrics", "/":
		return "/" + first
	default:
		return "other"
	}
}

// statusClass buckets a status into 2xx/4xx/5xx. The exact code is in the access
// log; a metric wants the class.
func statusClass(status int) string {
	return strconv.Itoa(status/100) + "xx"
}
