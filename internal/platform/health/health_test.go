package health_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/platform/health"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func okProbe(context.Context) error { return nil }

func failProbe(context.Context) error { return errors.New("connection refused") }

func TestNotReadyUntilMarked(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")

	report := registry.Ready(context.Background())
	assert.Equal(t, health.StatusDown, report.Status, "readiness must fail while starting up")

	registry.MarkReady()
	report = registry.Ready(context.Background())
	assert.Equal(t, health.StatusUp, report.Status)
}

func TestLivenessIgnoresDependencies(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.Register(health.Check{Name: "postgres", Probe: failProbe, Critical: true})

	// A dead database must not get the pod killed — only removed from the Service.
	assert.Equal(t, health.StatusUp, registry.Live().Status)
}

func TestCriticalFailureMakesServiceDown(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.MarkReady()
	registry.Register(health.Check{Name: "postgres", Probe: failProbe, Critical: true})

	report := registry.Ready(context.Background())
	assert.Equal(t, health.StatusDown, report.Status)
	require.Len(t, report.Checks, 1)
	assert.Equal(t, "connection refused", report.Checks[0].Error)
}

func TestNonCriticalFailureIsOnlyDegraded(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.MarkReady()
	registry.Register(health.Check{Name: "postgres", Probe: okProbe, Critical: true})
	registry.Register(health.Check{Name: "redis", Probe: failProbe, Critical: false})

	report := registry.Ready(context.Background())
	assert.Equal(t, health.StatusDegraded, report.Status,
		"losing a non-critical dependency must not take the service out of rotation")
}

func TestShuttingDownFailsReadiness(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.MarkReady()
	registry.Register(health.Check{Name: "postgres", Probe: okProbe, Critical: true})
	registry.MarkShuttingDown()

	report := registry.Ready(context.Background())
	assert.Equal(t, health.StatusDown, report.Status)
	require.Len(t, report.Checks, 1)
	assert.Equal(t, "lifecycle", report.Checks[0].Name)
}

func TestCheckTimeout(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.MarkReady()
	registry.Register(health.Check{
		Name:    "slow",
		Timeout: 20 * time.Millisecond,
		Probe: func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
				return nil
			}
		},
		Critical: true,
	})

	report := registry.Ready(context.Background())
	assert.Equal(t, health.StatusDown, report.Status)
	assert.Contains(t, report.Checks[0].Error, "deadline exceeded")
}

func TestReadyHandlerStatusCodes(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.Register(health.Check{Name: "postgres", Probe: okProbe, Critical: true})

	rec := httptest.NewRecorder()
	registry.ReadyHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code, "not ready yet")

	registry.MarkReady()
	rec = httptest.NewRecorder()
	registry.ReadyHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
	assert.Contains(t, rec.Body.String(), `"status":"UP"`)
}

func TestDegradedStillServesTraffic(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.MarkReady()
	registry.Register(health.Check{Name: "redis", Probe: failProbe, Critical: false})

	rec := httptest.NewRecorder()
	registry.ReadyHandler()(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"status":"DEGRADED"`)
}

func TestLiveHandler(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.2.3")
	rec := httptest.NewRecorder()
	registry.LiveHandler()(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"service":"wallet"`)
	assert.Contains(t, rec.Body.String(), `"version":"1.2.3"`)
}

func TestChecksRunConcurrently(t *testing.T) {
	registry := health.NewRegistry("wallet", "1.0.0")
	registry.MarkReady()
	for i := 0; i < 5; i++ {
		registry.Register(health.Check{
			Name: "slow-but-ok",
			Probe: func(context.Context) error {
				time.Sleep(50 * time.Millisecond)
				return nil
			},
			Critical: true,
		})
	}

	start := time.Now()
	report := registry.Ready(context.Background())
	elapsed := time.Since(start)

	assert.Equal(t, health.StatusUp, report.Status)
	assert.Less(t, elapsed, 200*time.Millisecond,
		"five 50ms checks run in parallel should finish well under their serial total")
}
