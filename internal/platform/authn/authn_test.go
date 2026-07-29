package authn_test

import (
	"context"
	"testing"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/platform/authn"
	"github.com/MS-Arcadia/api-gateway/internal/platform/errs"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testSecret = "a-test-secret-that-is-long-enough-for-hs256"

func newPair(t *testing.T) (*authn.Issuer, *authn.Verifier) {
	t.Helper()
	issuer, err := authn.NewIssuer(authn.IssuerConfig{
		Secret:   testSecret,
		Issuer:   "arcadia-auth",
		Audience: "arcadia",
		TTL:      15 * time.Minute,
	})
	require.NoError(t, err)

	verifier, err := authn.NewVerifier(authn.VerifierConfig{
		Algorithm: authn.AlgHS256,
		Secret:    testSecret,
		Issuer:    "arcadia-auth",
		Audience:  "arcadia",
	})
	require.NoError(t, err)
	return issuer, verifier
}

func TestIssueAndVerify(t *testing.T) {
	issuer, verifier := newPair(t)

	token, err := issuer.Issue("user-1", authn.RoleBasicUser, "jti-1", time.Now(), "wallet:read")
	require.NoError(t, err)

	principal, err := verifier.Verify(token)
	require.NoError(t, err)
	assert.Equal(t, "user-1", principal.UserID)
	assert.Equal(t, authn.RoleBasicUser, principal.Role)
	assert.Equal(t, "jti-1", principal.TokenID)
	assert.True(t, principal.HasScope("wallet:read"))
	assert.False(t, principal.HasScope("wallet:write"))
}

func TestVerifyRejectsExpiredToken(t *testing.T) {
	issuer, verifier := newPair(t)

	// Issued far enough in the past that even the default leeway cannot save it.
	token, err := issuer.Issue("user-1", authn.RoleBasicUser, "jti-1", time.Now().Add(-2*time.Hour))
	require.NoError(t, err)

	_, err = verifier.Verify(token)
	require.Error(t, err)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
	assert.Equal(t, "TOKEN_EXPIRED", errs.ReasonOf(err))
}

func TestVerifyRejectsWrongSecret(t *testing.T) {
	issuer, _ := newPair(t)
	token, err := issuer.Issue("user-1", authn.RoleAdmin, "jti-1", time.Now())
	require.NoError(t, err)

	other, err := authn.NewVerifier(authn.VerifierConfig{
		Algorithm: authn.AlgHS256,
		Secret:    "a-completely-different-secret-of-sufficient-size",
	})
	require.NoError(t, err)

	_, err = other.Verify(token)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

func TestVerifyRejectsUnsignedToken(t *testing.T) {
	// The classic "alg: none" downgrade. WithValidMethods must reject it.
	claims := authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "attacker",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: string(authn.RoleAdmin),
	}
	unsigned, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)

	_, verifier := newPair(t)
	_, err = verifier.Verify(unsigned)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

func TestVerifyRejectsRefreshToken(t *testing.T) {
	claims := authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Issuer:    "arcadia-auth",
			Audience:  jwt.ClaimStrings{"arcadia"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role:      string(authn.RoleBasicUser),
		TokenType: "refresh",
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	require.NoError(t, err)

	_, verifier := newPair(t)
	_, err = verifier.Verify(signed)
	require.Error(t, err)
	// REFRESH_TOKEN_USED, not WRONG_TOKEN_TYPE. The Python services already answered this way for
	// the same condition and these two did not, so a client branching on `reason` saw a different
	// answer depending on which service it happened to ask. WRONG_TOKEN_TYPE now means "the token
	// does not say what it is", which is a different mistake with a different remedy.
	assert.Equal(t, "REFRESH_TOKEN_USED", errs.ReasonOf(err))
}

// TestVerifyRejectsATokenWithNoType covers the hole this check was widened to close.
//
// The claim used to be checked only when present, so a token that said nothing about itself was
// taken to be an access token. The auth service spelled it `type` instead of `typ`, which meant
// `typ` arrived empty — and its seven-day refresh tokens, carrying a full role, were accepted on
// every endpoint of every service.
func TestVerifyRejectsATokenWithNoType(t *testing.T) {
	claims := authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Issuer:    "arcadia-auth",
			Audience:  jwt.ClaimStrings{"arcadia"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(7 * 24 * time.Hour)),
		},
		Role: string(authn.RoleAdmin),
		// TokenType deliberately absent.
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	require.NoError(t, err)

	_, verifier := newPair(t)
	_, err = verifier.Verify(signed)
	require.Error(t, err, "a token that does not declare its type must not be usable")
	assert.Equal(t, "WRONG_TOKEN_TYPE", errs.ReasonOf(err))
}

func TestVerifyRejectsUnknownRole(t *testing.T) {
	claims := authn.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-1",
			Issuer:    "arcadia-auth",
			Audience:  jwt.ClaimStrings{"arcadia"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role:      "SUPER_DUPER_ADMIN",
		TokenType: "access",
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testSecret))
	require.NoError(t, err)

	_, verifier := newPair(t)
	_, err = verifier.Verify(signed)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

func TestVerifyRejectsWrongIssuerAndAudience(t *testing.T) {
	wrongIssuer, err := authn.NewIssuer(authn.IssuerConfig{
		Secret: testSecret, Issuer: "evil", Audience: "arcadia",
	})
	require.NoError(t, err)
	token, err := wrongIssuer.Issue("user-1", authn.RoleBasicUser, "jti", time.Now())
	require.NoError(t, err)

	_, verifier := newPair(t)
	_, err = verifier.Verify(token)
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

func TestVerifyRejectsEmptyToken(t *testing.T) {
	_, verifier := newPair(t)
	_, err := verifier.Verify("   ")
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}

func TestNewVerifierValidatesConfig(t *testing.T) {
	_, err := authn.NewVerifier(authn.VerifierConfig{Algorithm: authn.AlgHS256, Secret: "short"})
	assert.Error(t, err, "a short HMAC secret must be refused")

	_, err = authn.NewVerifier(authn.VerifierConfig{Algorithm: authn.AlgRS256})
	assert.Error(t, err, "RS256 without a public key must be refused")

	_, err = authn.NewVerifier(authn.VerifierConfig{Algorithm: "ES512", Secret: testSecret})
	assert.Error(t, err, "an unsupported algorithm must be refused")

	_, err = authn.NewVerifier(authn.VerifierConfig{Algorithm: authn.AlgRS256, PublicKeyPEM: "not pem"})
	assert.Error(t, err)
}

func TestNewIssuerRejectsShortSecret(t *testing.T) {
	_, err := authn.NewIssuer(authn.IssuerConfig{Secret: "tiny"})
	assert.Error(t, err)
}

func TestIssueRejectsBadInput(t *testing.T) {
	issuer, _ := newPair(t)
	_, err := issuer.Issue("", authn.RoleBasicUser, "jti", time.Now())
	assert.Error(t, err)

	_, err = issuer.Issue("user-1", authn.Role("NOPE"), "jti", time.Now())
	assert.Error(t, err)
}

func TestBearerToken(t *testing.T) {
	token, err := authn.BearerToken("Bearer abc.def.ghi")
	require.NoError(t, err)
	assert.Equal(t, "abc.def.ghi", token)

	token, err = authn.BearerToken("bearer abc.def.ghi")
	require.NoError(t, err, "the scheme is case-insensitive per RFC 7235")
	assert.Equal(t, "abc.def.ghi", token)

	for _, header := range []string{"", "abc.def.ghi", "Basic dXNlcjpwYXNz", "Bearer "} {
		_, err := authn.BearerToken(header)
		assert.Error(t, err, "header %q must be rejected", header)
	}
}

func TestRoleParsing(t *testing.T) {
	role, ok := authn.ParseRole(" support ")
	assert.True(t, ok)
	assert.Equal(t, authn.RoleSupport, role)

	_, ok = authn.ParseRole("wizard")
	assert.False(t, ok)
}

func TestRequirePrincipal(t *testing.T) {
	_, err := authn.RequirePrincipal(context.Background())
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))

	ctx := authn.WithPrincipal(context.Background(), authn.Principal{UserID: "u-1", Role: authn.RoleBasicUser})
	principal, err := authn.RequirePrincipal(ctx)
	require.NoError(t, err)
	assert.Equal(t, "u-1", principal.UserID)
}

func TestRequireRole(t *testing.T) {
	ctx := authn.WithPrincipal(context.Background(), authn.Principal{UserID: "u-1", Role: authn.RoleBasicUser})

	_, err := authn.RequireRole(ctx, authn.RoleAdmin)
	assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))

	_, err = authn.RequireRole(ctx, authn.RoleBasicUser, authn.RoleAdmin)
	assert.NoError(t, err)
}

func TestRequireStaff(t *testing.T) {
	for _, role := range []authn.Role{authn.RoleSupport, authn.RoleAdmin} {
		ctx := authn.WithPrincipal(context.Background(), authn.Principal{UserID: "s-1", Role: role})
		_, err := authn.RequireStaff(ctx)
		assert.NoError(t, err, "role %s must count as staff", role)
	}
	for _, role := range []authn.Role{authn.RoleBasicUser, authn.RoleDeveloper, authn.RoleService} {
		ctx := authn.WithPrincipal(context.Background(), authn.Principal{UserID: "u-1", Role: role})
		_, err := authn.RequireStaff(ctx)
		assert.Error(t, err, "role %s must not count as staff", role)
	}
}

func TestRequireSelfOrStaff(t *testing.T) {
	tests := []struct {
		name    string
		caller  authn.Principal
		target  string
		allowed bool
	}{
		{"own wallet", authn.Principal{UserID: "u-1", Role: authn.RoleBasicUser}, "u-1", true},
		{"someone else's wallet", authn.Principal{UserID: "u-1", Role: authn.RoleBasicUser}, "u-2", false},
		{"support reads anybody", authn.Principal{UserID: "s-1", Role: authn.RoleSupport}, "u-2", true},
		{"admin reads anybody", authn.Principal{UserID: "a-1", Role: authn.RoleAdmin}, "u-2", true},
		{"service acts for anybody", authn.Principal{UserID: "store", Role: authn.RoleService}, "u-2", true},
		{"developer cannot snoop", authn.Principal{UserID: "d-1", Role: authn.RoleDeveloper}, "u-2", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := authn.WithPrincipal(context.Background(), tc.caller)
			_, err := authn.RequireSelfOrStaff(ctx, tc.target)
			if tc.allowed {
				assert.NoError(t, err)
			} else {
				assert.Equal(t, errs.CodePermissionDenied, errs.CodeOf(err))
			}
		})
	}
}

func TestRequireSelfOrStaffAnonymous(t *testing.T) {
	_, err := authn.RequireSelfOrStaff(context.Background(), "u-1")
	assert.Equal(t, errs.CodeUnauthenticated, errs.CodeOf(err))
}
