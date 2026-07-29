// Package authn verifies Arcadia access tokens and enforces role-based access.
//
// The Auth service issues the tokens; every other service verifies them locally.
// No network call is made on the request path, which is what keeps a wallet read
// inside its latency budget and stops Auth from becoming a single point of
// failure for the whole platform.
package authn

import (
	"context"
	"strings"

	"github.com/MS-Arcadia/api-gateway/internal/platform/errs"
)

// Role is one of the four roles defined in the requirements. A user holds
// exactly one.
type Role string

const (
	// RoleBasicUser is an ordinary customer.
	RoleBasicUser Role = "BASIC_USER"
	// RoleDeveloper publishes games and receives revenue.
	RoleDeveloper Role = "DEVELOPER"
	// RoleSupport moderates the platform and issues gift cards.
	RoleSupport Role = "SUPPORT"
	// RoleAdmin administers the platform.
	RoleAdmin Role = "ADMIN"
	// RoleService identifies another Arcadia service calling over the internal
	// network — the Store service driving the purchase saga, for instance.
	RoleService Role = "SERVICE"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool {
	switch r {
	case RoleBasicUser, RoleDeveloper, RoleSupport, RoleAdmin, RoleService:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (r Role) String() string { return string(r) }

// ParseRole normalises a role from a token claim.
func ParseRole(raw string) (Role, bool) {
	role := Role(strings.ToUpper(strings.TrimSpace(raw)))
	return role, role.Valid()
}

// Principal is the authenticated caller.
type Principal struct {
	// UserID is the subject of the token.
	UserID string
	// Role is the caller's single role.
	Role Role
	// Email is informational and may be empty.
	Email string
	// TokenID is the token's jti, used for revocation checks and audit trails.
	TokenID string
	// Scopes carries optional fine-grained grants for service-to-service tokens.
	Scopes []string
}

// IsService reports whether the caller is another Arcadia service.
func (p Principal) IsService() bool { return p.Role == RoleService }

// HasRole reports whether the principal holds any of the given roles.
func (p Principal) HasRole(roles ...Role) bool {
	for _, role := range roles {
		if p.Role == role {
			return true
		}
	}
	return false
}

// HasScope reports whether the principal was granted a scope.
func (p Principal) HasScope(scope string) bool {
	for _, s := range p.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// IsStaff reports whether the caller may act on other users' data.
func (p Principal) IsStaff() bool { return p.HasRole(RoleSupport, RoleAdmin) }

type principalContextKey struct{}

// WithPrincipal stores the authenticated caller on the context.
func WithPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFrom returns the authenticated caller, or false when the request is
// anonymous.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	principal, ok := ctx.Value(principalContextKey{}).(Principal)
	return principal, ok
}

// RequirePrincipal returns the caller or an unauthenticated error.
func RequirePrincipal(ctx context.Context) (Principal, error) {
	principal, ok := PrincipalFrom(ctx)
	if !ok || principal.UserID == "" {
		return Principal{}, errs.Unauthenticated("authentication is required")
	}
	return principal, nil
}

// RequireRole returns the caller if they hold one of roles, and a
// permission-denied error otherwise.
func RequireRole(ctx context.Context, roles ...Role) (Principal, error) {
	principal, err := RequirePrincipal(ctx)
	if err != nil {
		return Principal{}, err
	}
	if !principal.HasRole(roles...) {
		names := make([]string, 0, len(roles))
		for _, role := range roles {
			names = append(names, role.String())
		}
		return Principal{}, errs.PermissionDenied("this operation requires one of the roles: %s",
			strings.Join(names, ", ")).WithDetail("required_roles", names)
	}
	return principal, nil
}

// RequireStaff returns the caller if they are Support or Admin.
func RequireStaff(ctx context.Context) (Principal, error) {
	return RequireRole(ctx, RoleSupport, RoleAdmin)
}

// RequireSelfOrStaff authorises access to targetUserID.
//
// Users reach their own wallet; Support and Admin reach anybody's; another
// service reaches anybody's because it is acting on a user's behalf inside a
// saga. Everything else is denied. Getting this rule wrong once is how one
// customer ends up reading another customer's ledger.
func RequireSelfOrStaff(ctx context.Context, targetUserID string) (Principal, error) {
	principal, err := RequirePrincipal(ctx)
	if err != nil {
		return Principal{}, err
	}
	if principal.UserID == targetUserID || principal.IsStaff() || principal.IsService() {
		return principal, nil
	}
	return Principal{}, errs.PermissionDenied("you may only access your own wallet")
}
