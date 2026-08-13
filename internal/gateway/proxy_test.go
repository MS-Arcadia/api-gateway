package gateway_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/MS-Arcadia/api-gateway/internal/gateway"
	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
)

// spy stands in for one service and records what actually arrived.
type spy struct {
	server *httptest.Server

	path          string
	host          string
	correlationID string
	forwardedFor  string
	authorization string
}

func newSpy(t *testing.T, status int, body string) *spy {
	t.Helper()
	s := &spy{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.path = r.URL.Path
		s.host = r.Host
		s.correlationID = r.Header.Get(gateway.HeaderCorrelationID)
		s.forwardedFor = r.Header.Get("X-Forwarded-For")
		s.authorization = r.Header.Get("Authorization")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.server.Close)
	return s
}

func proxyTo(t *testing.T, targets gateway.Targets) *gateway.Proxy {
	t.Helper()
	built, err := gateway.NewTable(targets)
	require.NoError(t, err)
	return gateway.NewProxy(built, logx.NewNop(), 5*time.Second)
}

func TestARequestReachesItsServiceWithThePrefixRemoved(t *testing.T) {
	catalog := newSpy(t, http.StatusOK, `{"items":[]}`)
	proxy := proxyTo(t, gateway.Targets{"catalog-service": catalog.server.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, `{"items":[]}`, recorder.Body.String())
	require.Equal(t, "/v1/games", catalog.path, "the service must see its own path")
}

func TestTheQueryStringSurvives(t *testing.T) {
	// The storefront's filters are all query parameters. Losing them would make the
	// catalogue ignore every search, silently.
	catalog := newSpy(t, http.StatusOK, "{}")
	proxy := proxyTo(t, gateway.Targets{"catalog-service": catalog.server.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder,
		httptest.NewRequest(http.MethodGet, "/catalog/v1/games?q=neon&limit=40", nil))

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "/v1/games", catalog.path)
}

func TestTheHostHeaderBecomesTheUpstreams(t *testing.T) {
	// Some frameworks build absolute URLs from Host. Forwarding the gateway's Host
	// would have a service generate links pointing at a path it does not serve.
	catalog := newSpy(t, http.StatusOK, "{}")
	proxy := proxyTo(t, gateway.Targets{"catalog-service": catalog.server.URL})

	proxy.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil))

	require.NotEqual(t, "example.com", catalog.host)
	require.Contains(t, catalog.server.URL, catalog.host)
}

func TestTheAuthorizationHeaderIsPassedThroughUntouched(t *testing.T) {
	// The gateway is not the authority on authorisation — the services are — so the
	// credential has to arrive intact. Stripping it here would make every
	// authenticated call fail with a 401 from the wrong layer.
	wallet := newSpy(t, http.StatusOK, "{}")
	proxy := proxyTo(t, gateway.Targets{"wallet-service": wallet.server.URL})

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer a.b.c")
	proxy.ServeHTTP(httptest.NewRecorder(), request)

	require.Equal(t, "Bearer a.b.c", wallet.authorization)
}

func TestTheClientAddressIsForwarded(t *testing.T) {
	// Without X-Forwarded-For every service sees the gateway as the client, so the
	// auth service's per-IP rate limit would count the whole platform as one caller.
	wallet := newSpy(t, http.StatusOK, "{}")
	proxy := proxyTo(t, gateway.Targets{"wallet-service": wallet.server.URL})

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.RemoteAddr = "203.0.113.7:54321"
	proxy.ServeHTTP(httptest.NewRecorder(), request)

	require.Equal(t, "203.0.113.7", wallet.forwardedFor)
}

func TestAnUnroutablePathIsAProblemDocumentNotAGuess(t *testing.T) {
	catalog := newSpy(t, http.StatusOK, "{}")
	proxy := proxyTo(t, gateway.Targets{"catalog-service": catalog.server.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/search/v1/query", nil))

	require.Equal(t, http.StatusNotFound, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/problem+json")

	var problem map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	require.Equal(t, "NO_ROUTE", problem["reason"])
}

func TestADeadUpstreamIs503AndNamesTheService(t *testing.T) {
	// "The store is down" and "the wallet is down" need different people, so the
	// answer says which. A bare 502 would be defensible and less useful.
	//
	// Port 1 on loopback: reserved, nothing listens, and the connection is refused
	// immediately rather than timing out.
	proxy := proxyTo(t, gateway.Targets{"wallet-service": "http://127.0.0.1:1"})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil))

	require.Equal(t, http.StatusServiceUnavailable, recorder.Code)

	var problem map[string]any
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &problem))
	require.Equal(t, "UPSTREAM_UNAVAILABLE", problem["reason"])
	require.Contains(t, problem["detail"], "wallet-service")
}

func TestAnUpstreamsStatusAndBodyAreNotRewritten(t *testing.T) {
	// A 409 from the order service — "you already own this game" — has to arrive as a
	// 409 with its own reason code. A gateway that normalised upstream errors would
	// destroy the client's error handling.
	order := newSpy(t, http.StatusConflict, `{"reason":"ALREADY_OWNED","status":409}`)
	proxy := proxyTo(t, gateway.Targets{"order-service": order.server.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/orders/v1/orders", nil))

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.JSONEq(t, `{"reason":"ALREADY_OWNED","status":409}`, recorder.Body.String())
}

// --- redirects ------------------------------------------------------------

// redirectingSpy answers 307 with whatever Location it is given, the way FastAPI's
// trailing-slash redirect does.
func redirectingSpy(t *testing.T, location func(baseURL string) string) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", location(server.URL))
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestARedirectToTheServiceItselfComesBackAsAPublicPath(t *testing.T) {
	// review-service declares one route as POST "/" under an /api/reviews prefix, so a
	// request without the trailing slash is answered with an absolute redirect built from
	// the host it was addressed by — its cluster name. A browser cannot follow that.
	review := redirectingSpy(t, func(base string) string { return base + "/api/reviews/" })
	proxy := proxyTo(t, gateway.Targets{"review-service": review.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/reviews/api/reviews", nil))

	require.Equal(t, http.StatusTemporaryRedirect, recorder.Code)
	require.Equal(t, "/reviews/api/reviews/", recorder.Header().Get("Location"),
		"the caller must be sent back through the gateway, never to an internal host")
}

func TestARelativeRedirectKeepsTheServicesPrefix(t *testing.T) {
	review := redirectingSpy(t, func(string) string { return "/api/reviews/" })
	proxy := proxyTo(t, gateway.Targets{"review-service": review.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/reviews/api/reviews", nil))

	require.Equal(t, "/reviews/api/reviews/", recorder.Header().Get("Location"))
}

func TestARedirectSomewhereElseIsLeftAlone(t *testing.T) {
	// An OAuth handoff or a link to another site is not ours to rewrite, and a gateway
	// that rewrote it would break it.
	elsewhere := "https://accounts.example.com/authorize?client_id=arcadia"
	review := redirectingSpy(t, func(string) string { return elsewhere })
	proxy := proxyTo(t, gateway.Targets{"review-service": review.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/reviews/api/reviews", nil))

	require.Equal(t, elsewhere, recorder.Header().Get("Location"))
}

func TestAQueryStringSurvivesTheRewrite(t *testing.T) {
	review := redirectingSpy(t, func(base string) string { return base + "/api/reviews/?page=2" })
	proxy := proxyTo(t, gateway.Targets{"review-service": review.URL})

	recorder := httptest.NewRecorder()
	proxy.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/reviews/api/reviews", nil))

	require.Equal(t, "/reviews/api/reviews/?page=2", recorder.Header().Get("Location"))
}
