// Package metrics defines the Prometheus instrumentation shared by all services.
//
// The metric set is organised around the RED method (rate, errors, duration) for
// request-driven work, plus the domain-specific gauges the SLO table in the
// architecture document alerts on: saga state, outbox backlog, consumer lag and
// ledger drift.
package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Registry bundles a Prometheus registry with the platform's collectors.
type Registry struct {
	registry *prometheus.Registry
	service  string

	// RED metrics, one pair per transport.
	rpcRequests *prometheus.CounterVec
	rpcDuration *prometheus.HistogramVec
	rpcInFlight *prometheus.GaugeVec

	// Event pipeline.
	eventsConsumed     *prometheus.CounterVec
	eventDuration      *prometheus.HistogramVec
	eventsDeadLettered *prometheus.CounterVec
	consumerLag        *prometheus.GaugeVec

	// Outbox.
	outboxPublished *prometheus.CounterVec
	outboxFailed    *prometheus.CounterVec
	outboxBacklog   *prometheus.GaugeVec
	outboxOldestAge prometheus.Gauge

	// Domain metrics.
	moneyMoved       *prometheus.CounterVec
	walletOperations *prometheus.CounterVec
	ledgerMismatch   prometheus.Gauge
	idempotentHits   *prometheus.CounterVec
	rateLimitBlocks  *prometheus.CounterVec
	sagaSteps        *prometheus.CounterVec
	businessRules    *prometheus.CounterVec
}

// New builds a Registry for a service and registers the standard Go and process
// collectors alongside it.
func New(service string) *Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	labels := prometheus.Labels{"service": service}
	factory := prometheus.WrapRegistererWith(labels, registry)

	m := &Registry{registry: registry, service: service}

	m.rpcRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_rpc_requests_total",
		Help: "Total RPC requests handled, by transport, method and outcome.",
	}, []string{"transport", "method", "code"})

	m.rpcDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "arcadia_rpc_duration_seconds",
		Help: "RPC handler latency in seconds.",
		// Buckets straddle the 300ms p95 objective so that the SLO can be read
		// directly off the histogram.
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5, 5, 10},
	}, []string{"transport", "method"})

	m.rpcInFlight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "arcadia_rpc_in_flight",
		Help: "RPCs currently being handled.",
	}, []string{"transport"})

	m.eventsConsumed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_events_consumed_total",
		Help: "Kafka events consumed, by topic, type and outcome.",
	}, []string{"topic", "event_type", "outcome"})

	m.eventDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "arcadia_event_handling_duration_seconds",
		Help:    "Time spent handling one Kafka event.",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
	}, []string{"topic", "event_type"})

	m.eventsDeadLettered = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_events_dead_lettered_total",
		Help: "Events routed to a dead-letter topic. The SLO target is zero.",
	}, []string{"topic", "event_type"})

	m.consumerLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "arcadia_kafka_consumer_lag",
		Help: "Messages behind the partition high-water mark.",
	}, []string{"topic"})

	m.outboxPublished = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_outbox_published_total",
		Help: "Outbox messages successfully published.",
	}, []string{"topic"})

	m.outboxFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_outbox_publish_failures_total",
		Help: "Outbox publish attempts that failed.",
	}, []string{"topic"})

	m.outboxBacklog = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "arcadia_outbox_backlog",
		Help: "Outbox rows awaiting publication, by status.",
	}, []string{"status"})

	m.outboxOldestAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "arcadia_outbox_oldest_pending_age_seconds",
		Help: "Age of the oldest unpublished outbox row. A rising value means the dispatcher is stuck.",
	})

	m.moneyMoved = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_money_moved_minor_total",
		Help: "Cumulative money movement in minor currency units, by direction and reason.",
	}, []string{"direction", "reason", "currency"})

	m.walletOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_wallet_operations_total",
		Help: "Wallet operations attempted, by operation and outcome.",
	}, []string{"operation", "outcome"})

	m.ledgerMismatch = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "arcadia_ledger_mismatch_count",
		Help: "Wallets whose stored balance disagrees with their ledger. Anything above zero pages on-call.",
	})

	m.idempotentHits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_idempotent_replays_total",
		Help: "Requests short-circuited because their idempotency key had already been used.",
	}, []string{"operation"})

	m.rateLimitBlocks = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_rate_limit_blocks_total",
		Help: "Requests rejected by a rate limiter, by limiter name.",
	}, []string{"limiter"})

	m.sagaSteps = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_saga_steps_total",
		Help: "Saga steps executed, by saga, step and outcome.",
	}, []string{"saga", "step", "outcome"})

	m.businessRules = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "arcadia_business_rule_rejections_total",
		Help: "Operations rejected by a domain rule, by rule name.",
	}, []string{"rule"})

	factory.MustRegister(
		m.rpcRequests, m.rpcDuration, m.rpcInFlight,
		m.eventsConsumed, m.eventDuration, m.eventsDeadLettered, m.consumerLag,
		m.outboxPublished, m.outboxFailed, m.outboxBacklog, m.outboxOldestAge,
		m.moneyMoved, m.walletOperations, m.ledgerMismatch,
		m.idempotentHits, m.rateLimitBlocks, m.sagaSteps, m.businessRules,
	)
	return m
}

// Handler returns the /metrics HTTP handler.
func (m *Registry) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// Registerer exposes the underlying registry so that a service can add its own
// collectors.
func (m *Registry) Registerer() prometheus.Registerer { return m.registry }

// ObserveRPC records one handled request.
func (m *Registry) ObserveRPC(transport, method, code string, duration time.Duration) {
	m.rpcRequests.WithLabelValues(transport, method, code).Inc()
	m.rpcDuration.WithLabelValues(transport, method).Observe(duration.Seconds())
}

// IncInFlight and DecInFlight bracket a request.
func (m *Registry) IncInFlight(transport string) { m.rpcInFlight.WithLabelValues(transport).Inc() }

// DecInFlight decrements the in-flight gauge.
func (m *Registry) DecInFlight(transport string) { m.rpcInFlight.WithLabelValues(transport).Dec() }

// EventConsumed implements kafkax.ConsumerMetrics.
func (m *Registry) EventConsumed(topic, eventType string, duration time.Duration, err error) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.eventsConsumed.WithLabelValues(topic, eventType, outcome).Inc()
	m.eventDuration.WithLabelValues(topic, eventType).Observe(duration.Seconds())
}

// EventDeadLettered implements kafkax.ConsumerMetrics.
func (m *Registry) EventDeadLettered(topic, eventType, _ string) {
	m.eventsDeadLettered.WithLabelValues(topic, eventType).Inc()
}

// ConsumerLag implements kafkax.ConsumerMetrics.
func (m *Registry) ConsumerLag(topic string, lag int64) {
	m.consumerLag.WithLabelValues(topic).Set(float64(lag))
}

// OutboxPublished implements outbox.Metrics.
func (m *Registry) OutboxPublished(topic string, count int) {
	m.outboxPublished.WithLabelValues(topic).Add(float64(count))
}

// OutboxFailed implements outbox.Metrics.
func (m *Registry) OutboxFailed(topic string, count int) {
	m.outboxFailed.WithLabelValues(topic).Add(float64(count))
}

// OutboxBacklog implements outbox.Metrics.
func (m *Registry) OutboxBacklog(pending, failed int64, oldestAge time.Duration) {
	m.outboxBacklog.WithLabelValues("pending").Set(float64(pending))
	m.outboxBacklog.WithLabelValues("failed").Set(float64(failed))
	m.outboxOldestAge.Set(oldestAge.Seconds())
}

// MoneyMoved records a ledger movement for business dashboards.
func (m *Registry) MoneyMoved(direction, reason, currency string, amountMinor int64) {
	if amountMinor < 0 {
		amountMinor = -amountMinor
	}
	m.moneyMoved.WithLabelValues(direction, reason, currency).Add(float64(amountMinor))
}

// WalletOperation records the outcome of a wallet operation.
func (m *Registry) WalletOperation(operation, outcome string) {
	m.walletOperations.WithLabelValues(operation, outcome).Inc()
}

// LedgerMismatch publishes the reconciliation result.
func (m *Registry) LedgerMismatch(count int64) { m.ledgerMismatch.Set(float64(count)) }

// IdempotentReplay records a short-circuited duplicate request.
func (m *Registry) IdempotentReplay(operation string) {
	m.idempotentHits.WithLabelValues(operation).Inc()
}

// RateLimitBlock records a throttled request.
func (m *Registry) RateLimitBlock(limiter string) {
	m.rateLimitBlocks.WithLabelValues(limiter).Inc()
}

// SagaStep records one step of a distributed transaction.
func (m *Registry) SagaStep(saga, step, outcome string) {
	m.sagaSteps.WithLabelValues(saga, step, outcome).Inc()
}

// BusinessRuleRejection records a domain-rule rejection, e.g. INSUFFICIENT_FUNDS.
func (m *Registry) BusinessRuleRejection(rule string) {
	m.businessRules.WithLabelValues(rule).Inc()
}

// StatusLabel renders an HTTP status code as a metric label.
func StatusLabel(status int) string { return strconv.Itoa(status) }
