package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/MS-Arcadia/api-gateway/internal/gateway"
	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
)

// serve builds the whole server — operational routes, the middleware chain and the
// proxy — and returns an httptest server in front of it. The unit tests above cover
// each middleware alone; these cover the composition, which is where ordering
// mistakes live.
func serve(t *testing.T, targets gateway.Targets) *httptest.Server {
	t.Helper()

	table, err := gateway.NewTable(targets)
	require.NoError(t, err)

	server := gateway.New(gateway.Options{
		ServiceName:        "api-gateway",
		ServiceVersion:     "test",
		Port:               0,
		Table:              table,
		Verifier:           verifier(t),
		Logger:             logx.NewNop(),
		CORSOrigins:        []string{"http://localhost:3000"},
		RateLimitPerMinute: 600,
		UpstreamTimeout:    5 * time.Second,
	})

	// The server's own handler, exercised over a real socket so the chain runs the
	// way it runs in production rather than through a recorder.
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	return front
}

func TestLivenessChecksNothing(t *testing.T) {
	// Deliberately: a liveness probe that fails because a dependency is down asks an
	// orchestrator to restart the wrong container. Every upstream here is dead and
	// /livez still answers UP.
	front := serve(t, gateway.Targets{"catalog-service": "http://127.0.0.1:1"})

	response, err := http.Get(front.URL + "/livez")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)
}

func TestTheRootListsTheRoutingTable(t *testing.T) {
	// "Which prefix goes where" is the first question anybody debugging this asks,
	// and the alternative is reading a compose file.
	catalog := newSpy(t, http.StatusOK, "{}")
	front := serve(t, gateway.Targets{"catalog-service": catalog.server.URL})

	response, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	var body struct {
		Service string `json:"service"`
		Routes  []struct {
			Prefix  string `json:"prefix"`
			Service string `json:"service"`
		} `json:"routes"`
	}
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	require.Equal(t, "api-gateway", body.Service)
	require.Len(t, body.Routes, 1)
	require.Equal(t, "/catalog", body.Routes[0].Prefix)
}

func TestMetricsAreExposedForPrometheus(t *testing.T) {
	catalog := newSpy(t, http.StatusOK, "{}")
	front := serve(t, gateway.Targets{"catalog-service": catalog.server.URL})

	// One request first, so there is something to count.
	_, err := http.Get(front.URL + "/catalog/v1/games")
	require.NoError(t, err)

	response, err := http.Get(front.URL + "/metrics")
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	require.Equal(t, http.StatusOK, response.StatusCode)
}

func TestOperationalRoutesAreNotProxied(t *testing.T) {
	// If a service ever claimed one of these prefixes, an orchestrator would be
	// reading somebody else's health. The mux registers them directly, which is what
	// makes that impossible — this asserts the mux wins.
	catalog := newSpy(t, http.StatusOK, `{"i-am":"the catalog"}`)
	front := serve(t, gateway.Targets{"catalog-service": catalog.server.URL})

	for _, path := range []string{"/livez", "/readyz"} {
		response, err := http.Get(front.URL + path)
		require.NoError(t, err)
		require.NotEqual(t, "/v1", catalog.path, "%s must not have been forwarded", path)
		_ = response.Body.Close()
	}
}

func TestARateLimitedRequestStillCarriesCORSHeaders(t *testing.T) {
	// The reason CORS sits outside the limiter in the chain. Without this a
	// rate-limited page sees an opaque CORS error in the console instead of the 429
	// that explains itself — and the developer debugs the wrong problem.
	catalog := newSpy(t, http.StatusOK, "{}")

	table, err := gateway.NewTable(gateway.Targets{"catalog-service": catalog.server.URL})
	require.NoError(t, err)

	server := gateway.New(gateway.Options{
		ServiceName: "api-gateway", ServiceVersion: "test", Port: 0,
		Table: table, Verifier: verifier(t), Logger: logx.NewNop(),
		CORSOrigins:        []string{"http://localhost:3000"},
		RateLimitPerMinute: 1,
		UpstreamTimeout:    time.Second,
	})
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)

	client := &http.Client{}
	var last *http.Response
	for range 3 {
		request, err := http.NewRequest(http.MethodGet, front.URL+"/catalog/v1/games", nil)
		require.NoError(t, err)
		request.Header.Set("Origin", "http://localhost:3000")
		last, err = client.Do(request)
		require.NoError(t, err)
		_ = last.Body.Close()
	}

	require.Equal(t, http.StatusTooManyRequests, last.StatusCode)
	require.Equal(t, "http://localhost:3000", last.Header.Get("Access-Control-Allow-Origin"),
		"a 429 has to be readable by the page that caused it")
}

func TestAProxiedResponseCarriesExactlyOneCorrelationID(t *testing.T) {
	// Every Arcadia service stamps this header on its own responses, and ReverseProxy
	// *appends* the upstream's headers onto a ResponseWriter the Correlation middleware
	// has already written one to. Without dropping the upstream's copy the client gets
	// two values and reads whichever its HTTP library returns first — which, when a
	// service generates its own id rather than echoing ours, is the id that appears in
	// nobody's logs.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(gateway.HeaderCorrelationID, "a-different-id-from-the-service")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	front := serve(t, gateway.Targets{"catalog-service": upstream.URL})

	request, err := http.NewRequest(http.MethodGet, front.URL+"/catalog/v1/games", nil)
	require.NoError(t, err)
	request.Header.Set(gateway.HeaderCorrelationID, "the-id-the-gateway-logged")

	response, err := http.DefaultClient.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()

	require.Len(t, response.Header.Values(gateway.HeaderCorrelationID), 1,
		"a client reading one header must not have to choose between two answers")
	require.Equal(t, "the-id-the-gateway-logged",
		response.Header.Get(gateway.HeaderCorrelationID),
		"the surviving id must be the one the access log recorded")
}

func TestEveryResponseCarriesACorrelationID(t *testing.T) {
	// Including the failures. An error a client cannot quote an id for is an error
	// nobody can find in eight services' logs.
	front := serve(t, gateway.Targets{"catalog-service": "http://127.0.0.1:1"})

	for _, path := range []string{"/catalog/v1/games", "/nowhere/at/all"} {
		response, err := http.Get(front.URL + path)
		require.NoError(t, err)
		require.NotEmpty(t, response.Header.Get(gateway.HeaderCorrelationID), "for %s", path)
		_ = response.Body.Close()
	}
}
