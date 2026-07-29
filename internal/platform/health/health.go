// Package health implements liveness and readiness probes.
//
// The two are kept distinct because conflating them causes outages. Liveness answers
// "is this process wedged?" — a failure means restart me. Readiness answers "should
// traffic come here right now?" — a failure means stop sending requests, but leave the
// process alone. A database blip must fail readiness, never liveness: restarting would
// not fix Postgres and would throw away in-flight work.
//
// Docker's HEALTHCHECK calls /readyz (through the binary's own `healthcheck`
// subcommand); /livez is there for whatever supervises the container next.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Status is the outcome of a check.
type Status string

const (
	// StatusUp — the dependency is healthy.
	StatusUp Status = "UP"
	// StatusDown — the dependency is unusable.
	StatusDown Status = "DOWN"
	// StatusDegraded — usable but impaired. Readiness still passes.
	StatusDegraded Status = "DEGRADED"
)

// CheckFunc probes one dependency.
type CheckFunc func(ctx context.Context) error

// Check is a named probe.
type Check struct {
	// Name identifies the dependency in the probe response.
	Name string
	// Probe performs the check.
	Probe CheckFunc
	// Critical marks a dependency the service cannot serve without. A failing
	// non-critical check reports DEGRADED and readiness still passes, which is the
	// bulkhead tactic in practice: Redis being down must not stop wallet reads.
	Critical bool
	// Timeout bounds this check.
	Timeout time.Duration
}

// Result is one check's outcome.
type Result struct {
	Name       string `json:"name"`
	Status     Status `json:"status"`
	Error      string `json:"error,omitempty"`
	DurationMs int64  `json:"duration_ms"`
	Critical   bool   `json:"critical"`
}

// Report is the aggregated probe response.
type Report struct {
	Status  Status   `json:"status"`
	Service string   `json:"service"`
	Version string   `json:"version"`
	Checks  []Result `json:"checks,omitempty"`
	// UptimeSeconds helps distinguish "never started properly" from "just crashed".
	UptimeSeconds int64 `json:"uptime_seconds"`
}

// Registry holds the service's checks.
type Registry struct {
	mu      sync.RWMutex
	checks  []Check
	service string
	version string
	started time.Time
	// ready is flipped on once boot completes, so that the readiness probe fails
	// while migrations are still running.
	ready bool
	// shuttingDown makes readiness fail immediately on SIGTERM, draining traffic
	// before the process actually stops accepting connections.
	shuttingDown bool
}

// NewRegistry returns an empty Registry.
func NewRegistry(service, version string) *Registry {
	return &Registry{service: service, version: version, started: time.Now()}
}

// Register adds a check.
func (r *Registry) Register(check Check) {
	if check.Timeout <= 0 {
		check.Timeout = 2 * time.Second
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checks = append(r.checks, check)
}

// MarkReady declares the service ready to serve.
func (r *Registry) MarkReady() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = true
}

// MarkShuttingDown makes readiness fail so the load balancer drains this pod.
func (r *Registry) MarkShuttingDown() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.shuttingDown = true
}

// Live reports process liveness. It intentionally probes nothing: if this handler
// can answer at all, the process is alive.
func (r *Registry) Live() Report {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return Report{
		Status:        StatusUp,
		Service:       r.service,
		Version:       r.version,
		UptimeSeconds: int64(time.Since(r.started).Seconds()),
	}
}

// Ready runs every check and aggregates the result.
func (r *Registry) Ready(ctx context.Context) Report {
	r.mu.RLock()
	checks := make([]Check, len(r.checks))
	copy(checks, r.checks)
	ready, shuttingDown := r.ready, r.shuttingDown
	r.mu.RUnlock()

	report := Report{
		Status:        StatusUp,
		Service:       r.service,
		Version:       r.version,
		UptimeSeconds: int64(time.Since(r.started).Seconds()),
		Checks:        make([]Result, 0, len(checks)),
	}

	if shuttingDown {
		report.Status = StatusDown
		report.Checks = append(report.Checks, Result{
			Name: "lifecycle", Status: StatusDown, Error: "service is shutting down", Critical: true,
		})
		return report
	}
	if !ready {
		report.Status = StatusDown
		report.Checks = append(report.Checks, Result{
			Name: "lifecycle", Status: StatusDown, Error: "service is still starting up", Critical: true,
		})
		return report
	}

	// Checks run concurrently so that total probe time is the slowest check, not
	// the sum of all of them.
	results := make([]Result, len(checks))
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Add(1)
		go func(i int, check Check) {
			defer wg.Done()
			results[i] = runCheck(ctx, check)
		}(i, check)
	}
	wg.Wait()

	for _, result := range results {
		report.Checks = append(report.Checks, result)
		switch {
		case result.Status == StatusDown && result.Critical:
			report.Status = StatusDown
		case result.Status == StatusDown && report.Status == StatusUp:
			report.Status = StatusDegraded
		}
	}
	return report
}

func runCheck(ctx context.Context, check Check) Result {
	checkCtx, cancel := context.WithTimeout(ctx, check.Timeout)
	defer cancel()

	start := time.Now()
	err := check.Probe(checkCtx)
	result := Result{
		Name:       check.Name,
		Status:     StatusUp,
		DurationMs: time.Since(start).Milliseconds(),
		Critical:   check.Critical,
	}
	if err != nil {
		result.Status = StatusDown
		result.Error = err.Error()
	}
	return result
}

// LiveHandler serves the liveness probe.
func (r *Registry) LiveHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.Live())
	}
}

// ReadyHandler serves the readiness probe. A DEGRADED service still returns 200:
// it can serve, just not at full capability.
func (r *Registry) ReadyHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		report := r.Ready(req.Context())
		status := http.StatusOK
		if report.Status == StatusDown {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, report)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Probe responses must never be cached by an intermediary.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
