package gateway_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/require"

	"github.com/MS-Arcadia/api-gateway/internal/gateway"
	"github.com/MS-Arcadia/api-gateway/internal/platform/authn"
	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
)

const secret = "a-test-only-jwt-secret-at-least-32-chars"

func verifier(t *testing.T) *authn.Verifier {
	t.Helper()
	built, err := authn.NewVerifier(authn.VerifierConfig{
		Algorithm: authn.AlgHS256,
		Secret:    secret,
		Issuer:    "arcadia-auth",
		Audience:  "arcadia",
	})
	require.NoError(t, err)
	return built
}

// token mints one the way the auth service does. Every claim here is required by
// all seven services, which is why the helper sets them all rather than the
// minimum that would parse.
func token(t *testing.T, mutate func(jwt.MapClaims)) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub":    "11111111-1111-4111-8111-111111111111",
		"role":   "BASIC_USER",
		"typ":    "access",
		"scopes": []string{},
		"iss":    "arcadia-auth",
		"aud":    "arcadia",
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
		"iat":    time.Now().Unix(),
	}
	if mutate != nil {
		mutate(claims)
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	require.NoError(t, err)
	return signed
}

// reached records whether the request got past the middleware.
func reached() (http.Handler, *bool) {
	got := false
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = true
		w.WriteHeader(http.StatusOK)
	}), &got
}

// --- the token check, and the decision behind it -------------------------

func TestARequestWithNoTokenIsPassedThrough(t *testing.T) {
	// **The most important test in this package.**
	//
	// The obvious design is for the gateway to require authentication, which needs a
	// list of public routes — login, register, browsing the catalogue. That list is a
	// second copy of knowledge the services already hold, and two lists that must
	// agree drift: this platform lost a notification exactly that way, when a
	// translator existed and its router registration did not.
	//
	// So absence of a token is not the gateway's business. The service decides, and
	// there is no route table here to fall out of step.
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil))

	require.True(t, *got, "a request with no Authorization header must reach the service")
	require.Equal(t, http.StatusOK, recorder.Code)
}

func TestAValidTokenIsPassedThrough(t *testing.T) {
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer "+token(t, nil))

	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.True(t, *got)
}

func TestGarbageDiesAtTheEdge(t *testing.T) {
	cases := map[string]string{
		"not a jwt at all":  "Bearer not-a-jwt",
		"no bearer scheme":  "Basic dXNlcjpwYXNz",
		"bearer but empty":  "Bearer ",
		"missing the space": "Bearer",
	}

	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			next, got := reached()
			handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

			request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
			request.Header.Set("Authorization", header)

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			require.False(t, *got, "a malformed credential should not cost a service anything")
			require.Equal(t, http.StatusUnauthorized, recorder.Code)
		})
	}
}

func TestARefreshTokenIsRefusedHereToo(t *testing.T) {
	// The hole that was live across this whole platform: the auth service spelled the
	// claim `type` while every verifier read `typ`, so `typ` arrived empty and a check
	// that only refused `typ == "refresh"` treated an absent one as an access token.
	// Seven-day tokens carrying a full role worked on every endpoint.
	//
	// The gateway is a second layer against exactly that, not a replacement for it.
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer "+token(t, func(c jwt.MapClaims) {
		c["typ"] = "refresh"
		c["exp"] = time.Now().Add(7 * 24 * time.Hour).Unix()
		c["role"] = "ADMIN"
	}))

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.False(t, *got)
	require.Equal(t, http.StatusUnauthorized, recorder.Code)
}

func TestATokenThatDoesNotDeclareItsTypeIsRefused(t *testing.T) {
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer "+token(t, func(c jwt.MapClaims) {
		delete(c, "typ")
		// What the real token looked like: the claim spelled the other way, and a
		// full role behind it.
		c["type"] = "refresh"
		c["role"] = "ADMIN"
	}))

	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.False(t, *got, "an absent typ must not be read as an access token")
}

func TestATokenFromAnotherIssuerIsRefused(t *testing.T) {
	// iss and aud are not decoration. Before they were aligned, a token from a
	// completely different issuer was accepted by three of the five services.
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer "+token(t, func(c jwt.MapClaims) {
		c["iss"] = "somebody-elses-auth"
	}))

	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.False(t, *got)
}

func TestAnExpiredTokenIsRefused(t *testing.T) {
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer "+token(t, func(c jwt.MapClaims) {
		c["exp"] = time.Now().Add(-time.Hour).Unix()
	}))

	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.False(t, *got)
}

func TestATokenSignedWithAnotherKeyIsRefused(t *testing.T) {
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	forged, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"sub": "attacker", "role": "ADMIN", "typ": "access",
		"iss": "arcadia-auth", "aud": "arcadia",
		"exp": time.Now().Add(time.Hour).Unix(),
	}).SignedString([]byte("a-different-secret-that-is-also-32-chars"))
	require.NoError(t, err)

	request := httptest.NewRequest(http.MethodGet, "/wallet/v1/wallets/me", nil)
	request.Header.Set("Authorization", "Bearer "+forged)

	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.False(t, *got)
}

func TestTheGatewayDoesNotEnforceRoles(t *testing.T) {
	// A BASIC_USER reaching an admin path is passed through, and that is correct: the
	// role rules for an endpoint live with the endpoint. Copying them here would be a
	// second list to keep in step, and the catalog answers 403 for this anyway.
	next, got := reached()
	handler := gateway.VerifyToken(verifier(t), logx.NewNop())(next)

	request := httptest.NewRequest(http.MethodPost, "/auth/v1/admin/users/x/ban", nil)
	request.Header.Set("Authorization", "Bearer "+token(t, func(c jwt.MapClaims) {
		c["role"] = "BASIC_USER"
	}))

	handler.ServeHTTP(httptest.NewRecorder(), request)
	require.True(t, *got, "authorisation is the service's decision, not the gateway's")
}

// --- CORS ----------------------------------------------------------------

func TestAPreflightFromAnAllowedOriginIsAnswered(t *testing.T) {
	next, got := reached()
	handler := gateway.CORS([]string{"http://localhost:3000"})(next)

	request := httptest.NewRequest(http.MethodOptions, "/catalog/v1/games", nil)
	request.Header.Set("Origin", "http://localhost:3000")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.False(t, *got, "a preflight must not reach a service")
	require.Equal(t, "http://localhost:3000", recorder.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, recorder.Header().Get("Access-Control-Allow-Headers"), "Authorization")
}

func TestAPreflightFromAnUnknownOriginIsRefused(t *testing.T) {
	next, _ := reached()
	handler := gateway.CORS([]string{"http://localhost:3000"})(next)

	request := httptest.NewRequest(http.MethodOptions, "/catalog/v1/games", nil)
	request.Header.Set("Origin", "https://evil.example")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusForbidden, recorder.Code)
	require.Empty(t, recorder.Header().Get("Access-Control-Allow-Origin"))

	// And it says so in the platform's error shape. The browser never shows this body
	// to the page, but curl and the access log do — and a bodiless 403 does not say
	// which of the two possible things went wrong.
	var problem struct {
		Reason string `json:"reason"`
		Detail string `json:"detail"`
	}
	require.NoError(t, json.NewDecoder(recorder.Body).Decode(&problem))
	require.Equal(t, "ORIGIN_NOT_ALLOWED", problem.Reason)
	require.Contains(t, problem.Detail, "https://evil.example")
}

func TestTheOriginIsEchoedNotWildcarded(t *testing.T) {
	// `*` plus a bearer token means any page on the internet can call this API with a
	// phished token and the browser will allow it.
	next, _ := reached()
	handler := gateway.CORS([]string{"http://localhost:3000", "https://arcadia.example"})(next)

	request := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
	request.Header.Set("Origin", "https://arcadia.example")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.Equal(t, "https://arcadia.example", recorder.Header().Get("Access-Control-Allow-Origin"))
	require.Contains(t, recorder.Header().Values("Vary"), "Origin",
		"without Vary a cache could serve one origin's response to another")
}

// --- rate limiting -------------------------------------------------------

func TestRequestsUnderTheLimitPass(t *testing.T) {
	next, _ := reached()
	handler := gateway.NewRateLimit(5).Middleware(next)

	for i := range 5 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
		request.RemoteAddr = "203.0.113.7:1234"
		handler.ServeHTTP(recorder, request)
		require.Equal(t, http.StatusOK, recorder.Code, "request %d should pass", i+1)
	}
}

func TestTheRequestOverTheLimitIs429WithRetryAfter(t *testing.T) {
	next, _ := reached()
	handler := gateway.NewRateLimit(3).Middleware(next)

	var last *httptest.ResponseRecorder
	for range 4 {
		last = httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
		request.RemoteAddr = "203.0.113.7:1234"
		handler.ServeHTTP(last, request)
	}

	require.Equal(t, http.StatusTooManyRequests, last.Code)
	require.NotEmpty(t, last.Header().Get("Retry-After"),
		"a 429 without Retry-After tells a client to guess")

	var problem map[string]any
	require.NoError(t, json.Unmarshal(last.Body.Bytes(), &problem))
	require.Equal(t, "RATE_LIMITED", problem["reason"])
}

func TestTheLimitIsPerClientNotGlobal(t *testing.T) {
	// A shared counter would let one noisy client lock everybody else out, which is
	// the denial of service the limiter exists to prevent rather than to cause.
	next, _ := reached()
	handler := gateway.NewRateLimit(2).Middleware(next)

	for range 2 {
		request := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
		request.RemoteAddr = "203.0.113.7:1234"
		handler.ServeHTTP(httptest.NewRecorder(), request)
	}

	recorder := httptest.NewRecorder()
	other := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
	other.RemoteAddr = "198.51.100.9:5678"
	handler.ServeHTTP(recorder, other)

	require.Equal(t, http.StatusOK, recorder.Code, "a second client has its own bucket")
}

func TestPreflightsAreNotRateLimited(t *testing.T) {
	// The browser sends these, not the page. Counting them would have an application
	// trip the limit by being used normally.
	next, _ := reached()
	handler := gateway.NewRateLimit(1).Middleware(next)

	for range 5 {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodOptions, "/catalog/v1/games", nil)
		request.RemoteAddr = "203.0.113.7:1234"
		handler.ServeHTTP(recorder, request)
		require.NotEqual(t, http.StatusTooManyRequests, recorder.Code)
	}
}

func TestABucketRefillsOverTime(t *testing.T) {
	// The rate is chosen so draining is fast *relative to* refilling. At 600 a minute
	// a token returns every 100ms, so 600 requests in a few milliseconds put back
	// about 0.05 of one and the bucket really does empty.
	//
	// The first version of this test used 60000/minute — one token per millisecond —
	// and the drain loop itself took 80ms, so the bucket refilled faster than the
	// loop emptied it and the "blocked" assertion got a 200. That was the test being
	// wrong about the arithmetic, not the limiter.
	next, _ := reached()
	handler := gateway.NewRateLimit(600).Middleware(next)

	send := func() int {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
		request.RemoteAddr = "203.0.113.7:1234"
		handler.ServeHTTP(recorder, request)
		return recorder.Code
	}

	for range 600 {
		send()
	}
	require.Equal(t, http.StatusTooManyRequests, send(), "the bucket should be empty")

	time.Sleep(150 * time.Millisecond)
	require.Equal(t, http.StatusOK, send(), "a token should have refilled")
}

// --- correlation ---------------------------------------------------------

func TestACorrelationIDIsGeneratedAndEchoed(t *testing.T) {
	next, _ := reached()
	handler := gateway.Correlation(next)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil))

	id := recorder.Header().Get(gateway.HeaderCorrelationID)
	require.NotEmpty(t, id, "a client needs an id it can quote in a bug report")
	require.Len(t, id, 36, "a UUID")
}

func TestACallersCorrelationIDIsKept(t *testing.T) {
	// Generating a new one would break the chain: the whole point is that one id
	// covers the gateway's line and all the services' lines for the same request.
	next, _ := reached()
	handler := gateway.Correlation(next)

	request := httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil)
	request.Header.Set(gateway.HeaderCorrelationID, "3a196148-ccb4-4c0f-a2fb-4666eb51e45a")

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	require.Equal(t, "3a196148-ccb4-4c0f-a2fb-4666eb51e45a",
		recorder.Header().Get(gateway.HeaderCorrelationID))
}

func TestTheCorrelationIDTravelsToTheUpstream(t *testing.T) {
	catalog := newSpy(t, http.StatusOK, "{}")
	proxy := proxyTo(t, gateway.Targets{"catalog-service": catalog.server.URL})
	handler := gateway.Chain(proxy, gateway.Correlation)

	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/catalog/v1/games", nil))

	require.NotEmpty(t, catalog.correlationID,
		"the service has to receive the id to stamp it on its own log lines")
}
