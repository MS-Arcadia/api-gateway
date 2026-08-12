// Package gateway routes one public origin onto the platform's services.
package gateway

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// Upstream is one service behind the gateway.
type Upstream struct {
	// Name is what appears in logs and metrics. It matches the service's own
	// service_name, so a correlation id can be followed across both.
	Name string
	// Prefix is the public path this service answers, including the leading slash
	// and no trailing one: "/catalog".
	Prefix string
	// Target is the internal base URL, "http://catalog-service:8082".
	Target *url.URL
	// StripPrefix removes Prefix before forwarding.
	//
	// True for every service here, and the reason is worth stating: the services
	// were built before the gateway and each mounts its own routes at "/v1/...".
	// Rewriting their routers to expect "/catalog/v1/..." would make seven services
	// depend on being behind a gateway, which is exactly the coupling a gateway is
	// supposed to prevent. So the prefix is a gateway-side concern and is removed
	// on the way through.
	StripPrefix bool
}

// Table is the complete routing table, longest prefix first.
type Table struct {
	upstreams []Upstream
}

// Targets is the set of internal base URLs, keyed by service name.
type Targets map[string]string

// The public prefixes. These are not invented here — the frontend already writes
// every path this way in lib/api-paths.ts, and choosing them there before this
// existed is what makes the switch from its mock to this gateway one environment
// variable rather than a rewrite of every call site.
const (
	PrefixAuth            = "/auth"
	PrefixCatalog         = "/catalog"
	PrefixOrders          = "/orders"
	PrefixWallet          = "/wallet"
	PrefixPayment         = "/payment"
	PrefixMedia           = "/media"
	PrefixNotifications   = "/notifications"
	PrefixMarketplace     = "/marketplace"
	PrefixReviews         = "/reviews"
	PrefixFestivals       = "/festivals"
	PrefixCommunity       = "/community"
	PrefixRecommendations = "/recommendations"
)

// prefixFor maps a service name to its public prefix. Kept beside the constants
// so adding a service is one edit rather than two that can disagree.
var prefixFor = map[string]string{
	"auth-profile-service":   PrefixAuth,
	"catalog-service":        PrefixCatalog,
	"order-service":          PrefixOrders,
	"wallet-service":         PrefixWallet,
	"payment-service":        PrefixPayment,
	"media-service":          PrefixMedia,
	"notification-service":   PrefixNotifications,
	"marketplace-service":    PrefixMarketplace,
	"review-service":         PrefixReviews,
	"festival-service":       PrefixFestivals,
	"community-service":      PrefixCommunity,
	"recommendation-service": PrefixRecommendations,
}

// NewTable builds a routing table from service name to base URL.
//
// A service missing from targets is an error rather than a silently absent route:
// a gateway that starts with six of seven upstreams answers 404 for a seventh of
// the platform, and it does so looking healthy.
func NewTable(targets Targets) (*Table, error) {
	if len(targets) == 0 {
		return nil, fmt.Errorf("gateway: no upstreams configured")
	}

	upstreams := make([]Upstream, 0, len(targets))
	for name, raw := range targets {
		prefix, ok := prefixFor[name]
		if !ok {
			return nil, fmt.Errorf("gateway: no public prefix is defined for %q", name)
		}
		target, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("gateway: %s has an unparseable target %q: %w", name, raw, err)
		}
		if target.Scheme == "" || target.Host == "" {
			return nil, fmt.Errorf("gateway: %s needs an absolute target, got %q", name, raw)
		}
		upstreams = append(upstreams, Upstream{
			Name:        name,
			Prefix:      prefix,
			Target:      target,
			StripPrefix: true,
		})
	}

	// Longest prefix first. None of the current prefixes is a prefix of another, so
	// today the order is cosmetic — but "/media" and "/media-admin" would not be,
	// and a table that only works because of what happens to be in it is a trap for
	// whoever adds the eighth service.
	sort.Slice(upstreams, func(i, j int) bool {
		return len(upstreams[i].Prefix) > len(upstreams[j].Prefix)
	})

	return &Table{upstreams: upstreams}, nil
}

// Lookup finds the upstream for a request path, and the path to forward.
func (t *Table) Lookup(path string) (Upstream, string, bool) {
	for _, upstream := range t.upstreams {
		if path != upstream.Prefix && !strings.HasPrefix(path, upstream.Prefix+"/") {
			continue
		}
		forwarded := path
		if upstream.StripPrefix {
			forwarded = strings.TrimPrefix(path, upstream.Prefix)
			// "/catalog" alone becomes "", which is not a valid request target.
			if forwarded == "" {
				forwarded = "/"
			}
		}
		return upstream, forwarded, true
	}
	return Upstream{}, "", false
}

// Upstreams returns the table in matching order, for health checks and logging.
func (t *Table) Upstreams() []Upstream {
	out := make([]Upstream, len(t.upstreams))
	copy(out, t.upstreams)
	return out
}

// Prefixes returns the public prefixes, for the root index.
func (t *Table) Prefixes() []string {
	out := make([]string, 0, len(t.upstreams))
	for _, upstream := range t.upstreams {
		out = append(out, upstream.Prefix)
	}
	sort.Strings(out)
	return out
}
