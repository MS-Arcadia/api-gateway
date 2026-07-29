package gateway_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MS-Arcadia/api-gateway/internal/gateway"
)

func table(t *testing.T) *gateway.Table {
	t.Helper()
	built, err := gateway.NewTable(gateway.Targets{
		"auth-profile-service": "http://auth:8085",
		"catalog-service":      "http://catalog:8082",
		"order-service":        "http://order:8083",
		"wallet-service":       "http://wallet:8080",
		"payment-service":      "http://payment:8081",
		"media-service":        "http://media:8084",
		"notification-service": "http://notification:8086",
	})
	require.NoError(t, err)
	return built
}

// --- the contract with the frontend --------------------------------------
//
// These paths are not invented in the test: they are copied from the frontend's
// lib/api-paths.ts, which was written before this gateway existed. If the two ever
// disagree, the storefront 404s on every request — so the disagreement is worth a
// test rather than a discovery.

func TestTheFrontendsPathsRouteToTheRightServices(t *testing.T) {
	routes := table(t)

	cases := []struct {
		path      string
		service   string
		forwarded string
	}{
		{"/auth/v1/auth/login", "auth-profile-service", "/v1/auth/login"},
		{"/auth/v1/profile/abc", "auth-profile-service", "/v1/profile/abc"},
		{"/catalog/v1/games", "catalog-service", "/v1/games"},
		{"/catalog/v1/games/game-1/detail", "catalog-service", "/v1/games/game-1/detail"},
		{"/catalog/v1/review-queue", "catalog-service", "/v1/review-queue"},
		{"/orders/v1/orders", "order-service", "/v1/orders"},
		{"/orders/v1/instalment-orders", "order-service", "/v1/instalment-orders"},
		{"/wallet/v1/wallets/me", "wallet-service", "/v1/wallets/me"},
		{"/wallet/v1/wallets/me/ledger", "wallet-service", "/v1/wallets/me/ledger"},
		{"/notifications/v1/notifications", "notification-service", "/v1/notifications"},
		{"/notifications/v1/notifications/unread-count", "notification-service", "/v1/notifications/unread-count"},
		{"/media/v1/media/usage", "media-service", "/v1/media/usage"},
		{"/payment/v1/payments", "payment-service", "/v1/payments"},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			upstream, forwarded, ok := routes.Lookup(tc.path)
			require.True(t, ok, "no route for %s", tc.path)
			require.Equal(t, tc.service, upstream.Name)
			require.Equal(t, tc.forwarded, forwarded,
				"the prefix has to be stripped: the services mount their routes at /v1")
		})
	}
}

func TestThePrefixIsStrippedBecauseTheServicesNeverKnewAboutIt(t *testing.T) {
	// The whole reason StripPrefix exists. Every service was built before the
	// gateway and mounts its own routes at /v1/...; forwarding /catalog/v1/games
	// unchanged would 404 at the catalog, and rewriting seven routers to expect a
	// gateway prefix would make them depend on being behind one.
	routes := table(t)

	_, forwarded, ok := routes.Lookup("/catalog/v1/games")
	require.True(t, ok)
	require.Equal(t, "/v1/games", forwarded)
	require.NotContains(t, forwarded, "catalog")
}

func TestAPrefixOnItsOwnForwardsToRoot(t *testing.T) {
	// "/catalog" strips to "", which is not a valid request target — Go's http
	// client rejects it and the proxy would fail in a way that looks like the
	// upstream's fault.
	routes := table(t)

	_, forwarded, ok := routes.Lookup("/catalog")
	require.True(t, ok)
	require.Equal(t, "/", forwarded)
}

func TestAnUnknownPathHasNoRoute(t *testing.T) {
	routes := table(t)

	for _, path := range []string{"/", "/search/v1/query", "/festival/v1/festivals", "/catalogue/v1/games"} {
		_, _, ok := routes.Lookup(path)
		require.False(t, ok, "%s should have no route until its service exists", path)
	}
}

func TestAPrefixIsNotMatchedAsASubstring(t *testing.T) {
	// "/catalogue" starts with "/catalog" as a string. Matching on that would send
	// a future service's traffic to the catalog, and the error would come from the
	// wrong place entirely.
	routes := table(t)

	_, _, ok := routes.Lookup("/catalogue/v1/games")
	require.False(t, ok)
}

func TestLongerPrefixesAreMatchedFirst(t *testing.T) {
	// None of the current prefixes is a prefix of another, so this is about the
	// eighth service rather than about today: /media and /media-admin would collide,
	// and a table that only works because of what happens to be in it is a trap.
	routes := table(t)

	prefixes := routes.Upstreams()
	for i := 1; i < len(prefixes); i++ {
		require.GreaterOrEqual(t, len(prefixes[i-1].Prefix), len(prefixes[i].Prefix),
			"the table must be ordered longest prefix first")
	}
}

// --- configuration failures are loud -------------------------------------

func TestAServiceWithNoPrefixIsRefused(t *testing.T) {
	_, err := gateway.NewTable(gateway.Targets{"search-service": "http://search:8087"})
	require.ErrorContains(t, err, "no public prefix")
}

func TestARelativeTargetIsRefused(t *testing.T) {
	// A gateway that started with "catalog:8082" would proxy to a relative URL and
	// fail on every request, having reported itself healthy.
	_, err := gateway.NewTable(gateway.Targets{"catalog-service": "catalog:8082"})
	require.ErrorContains(t, err, "absolute target")
}

func TestAnEmptyTableIsRefused(t *testing.T) {
	_, err := gateway.NewTable(gateway.Targets{})
	require.ErrorContains(t, err, "no upstreams")
}

func TestOperationalPathsAreNotClaimedByAnyUpstream(t *testing.T) {
	// /livez, /readyz and /metrics are registered on the mux directly. If a service
	// ever claimed one of those prefixes, the gateway's own probes would be proxied
	// away and an orchestrator would be reading somebody else's health.
	routes := table(t)

	for _, path := range []string{"/livez", "/readyz", "/metrics"} {
		_, _, ok := routes.Lookup(path)
		require.False(t, ok, "%s must not be routable to an upstream", path)
	}
}
