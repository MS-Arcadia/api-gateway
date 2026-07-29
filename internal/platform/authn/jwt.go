package authn

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/platform/errs"
	"github.com/golang-jwt/jwt/v5"
)

// Algorithm is the signing algorithm a verifier accepts.
type Algorithm string

const (
	// AlgHS256 uses a shared secret. Simple to operate, but every service that can
	// verify a token can also mint one.
	AlgHS256 Algorithm = "HS256"
	// AlgRS256 uses an asymmetric key pair: Auth holds the private key and signs,
	// everyone else holds only the public key and verifies. This is the production
	// choice.
	AlgRS256 Algorithm = "RS256"
)

// VerifierConfig configures token verification.
type VerifierConfig struct {
	Algorithm Algorithm
	// Secret is the HMAC key, required for HS256.
	Secret string
	// PublicKeyPEM is the RSA public key, required for RS256.
	PublicKeyPEM string
	// Issuer, when set, must match the token's iss claim.
	Issuer string
	// Audience, when set, must appear in the token's aud claim.
	Audience string
	// Leeway tolerates small clock skew between services.
	Leeway time.Duration
}

// Claims is Arcadia's access-token body.
type Claims struct {
	jwt.RegisteredClaims
	// Role is the caller's single role.
	Role string `json:"role"`
	// Email is informational.
	Email string `json:"email,omitempty"`
	// Scopes carries fine-grained grants for service tokens.
	Scopes []string `json:"scopes,omitempty"`
	// TokenType distinguishes an access token from a refresh token. A refresh
	// token must never be accepted as a credential for an API call.
	TokenType string `json:"typ,omitempty"`
}

// Verifier validates access tokens.
type Verifier struct {
	parser    *jwt.Parser
	algorithm Algorithm
	hmacKey   []byte
	rsaKey    *rsa.PublicKey
}

// NewVerifier builds a Verifier from cfg.
func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if cfg.Algorithm == "" {
		cfg.Algorithm = AlgHS256
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = 30 * time.Second
	}

	opts := []jwt.ParserOption{
		// Pinning the accepted algorithm is what prevents the classic "alg: none"
		// and HS/RS confusion attacks, where an attacker re-signs a token with the
		// public key treated as an HMAC secret.
		jwt.WithValidMethods([]string{string(cfg.Algorithm)}),
		jwt.WithLeeway(cfg.Leeway),
		jwt.WithExpirationRequired(),
	}
	if cfg.Issuer != "" {
		opts = append(opts, jwt.WithIssuer(cfg.Issuer))
	}
	if cfg.Audience != "" {
		opts = append(opts, jwt.WithAudience(cfg.Audience))
	}

	verifier := &Verifier{
		parser:    jwt.NewParser(opts...),
		algorithm: cfg.Algorithm,
	}

	switch cfg.Algorithm {
	case AlgHS256:
		if cfg.Secret == "" {
			return nil, errors.New("authn: a secret is required for HS256")
		}
		if len(cfg.Secret) < 32 {
			return nil, errors.New("authn: the HS256 secret must be at least 32 bytes")
		}
		verifier.hmacKey = []byte(cfg.Secret)
	case AlgRS256:
		if cfg.PublicKeyPEM == "" {
			return nil, errors.New("authn: a public key is required for RS256")
		}
		key, err := parseRSAPublicKey(cfg.PublicKeyPEM)
		if err != nil {
			return nil, err
		}
		verifier.rsaKey = key
	default:
		return nil, fmt.Errorf("authn: unsupported algorithm %q", cfg.Algorithm)
	}

	return verifier, nil
}

func parseRSAPublicKey(pemData string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(pemData)))
	if block == nil {
		return nil, errors.New("authn: public key is not valid PEM")
	}

	if key, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		rsaKey, ok := key.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("authn: public key is not an RSA key")
		}
		return rsaKey, nil
	}
	// Fall back to the older PKCS#1 encoding.
	key, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("authn: parse public key: %w", err)
	}
	return key, nil
}

func (v *Verifier) keyFunc(token *jwt.Token) (any, error) {
	switch v.algorithm {
	case AlgHS256:
		return v.hmacKey, nil
	case AlgRS256:
		return v.rsaKey, nil
	default:
		return nil, fmt.Errorf("authn: unsupported algorithm %q", v.algorithm)
	}
}

// Verify parses and validates a bearer token, returning the Principal it names.
//
// Error messages stay generic on purpose: telling a caller whether a token was
// expired, forged or simply malformed hands an attacker a free oracle.
func (v *Verifier) Verify(tokenString string) (Principal, error) {
	if strings.TrimSpace(tokenString) == "" {
		return Principal{}, errs.Unauthenticated("no access token was provided")
	}

	claims := &Claims{}
	token, err := v.parser.ParseWithClaims(tokenString, claims, v.keyFunc)
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return Principal{}, errs.Unauthenticated("the access token has expired").
				WithReason("TOKEN_EXPIRED").WithCause(err)
		case errors.Is(err, jwt.ErrTokenNotValidYet):
			return Principal{}, errs.Unauthenticated("the access token is not valid yet").
				WithReason("TOKEN_NOT_YET_VALID").WithCause(err)
		default:
			return Principal{}, errs.Unauthenticated("the access token is invalid").
				WithReason("TOKEN_INVALID").WithCause(err)
		}
	}
	if !token.Valid {
		return Principal{}, errs.Unauthenticated("the access token is invalid").WithReason("TOKEN_INVALID")
	}

	// Required, not merely checked when present. This used to allow an empty TokenType through
	// on the grounds that an older token might not carry one — and the auth service spelled the
	// claim `type`, so `typ` arrived empty and its seven-day refresh tokens, carrying a full
	// role, were accepted on every endpoint. Demanding the claim closes that for any future
	// issuer as well, and every issuer on the platform already sets it.
	if !strings.EqualFold(claims.TokenType, "access") {
		reason := "WRONG_TOKEN_TYPE"
		if strings.EqualFold(claims.TokenType, "refresh") {
			reason = "REFRESH_TOKEN_USED"
		}
		return Principal{}, errs.Unauthenticated("only an access token may be used to call the API").
			WithReason(reason)
	}
	if claims.Subject == "" {
		return Principal{}, errs.Unauthenticated("the access token has no subject").
			WithReason("TOKEN_INVALID")
	}

	role, ok := ParseRole(claims.Role)
	if !ok {
		return Principal{}, errs.Unauthenticated("the access token carries an unknown role %q", claims.Role).
			WithReason("TOKEN_INVALID")
	}

	return Principal{
		UserID:  claims.Subject,
		Role:    role,
		Email:   claims.Email,
		TokenID: claims.ID,
		Scopes:  claims.Scopes,
	}, nil
}

// BearerToken extracts the credential from an Authorization header value.
func BearerToken(header string) (string, error) {
	if header == "" {
		return "", errs.Unauthenticated("the Authorization header is missing")
	}
	parts := strings.SplitN(strings.TrimSpace(header), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", errs.Unauthenticated("the Authorization header must be of the form 'Bearer <token>'")
	}
	token := strings.TrimSpace(parts[1])
	if token == "" {
		return "", errs.Unauthenticated("the Authorization header carries an empty token")
	}
	return token, nil
}

// Issuer mints tokens. Only the Auth service does this in production; the wallet
// and payment services use it in tests, and it is what lets the Postman
// collection produce a working token without standing Auth up.
type Issuer struct {
	algorithm Algorithm
	hmacKey   []byte
	issuer    string
	audience  string
	ttl       time.Duration
}

// IssuerConfig configures an Issuer.
type IssuerConfig struct {
	Secret   string
	Issuer   string
	Audience string
	TTL      time.Duration
}

// NewIssuer builds an HS256 Issuer.
func NewIssuer(cfg IssuerConfig) (*Issuer, error) {
	if len(cfg.Secret) < 32 {
		return nil, errors.New("authn: the HS256 secret must be at least 32 bytes")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 15 * time.Minute
	}
	return &Issuer{
		algorithm: AlgHS256,
		hmacKey:   []byte(cfg.Secret),
		issuer:    cfg.Issuer,
		audience:  cfg.Audience,
		ttl:       cfg.TTL,
	}, nil
}

// Issue mints an access token for the given user and role.
func (i *Issuer) Issue(userID string, role Role, tokenID string, now time.Time, scopes ...string) (string, error) {
	if userID == "" {
		return "", errors.New("authn: user id is required")
	}
	if !role.Valid() {
		return "", fmt.Errorf("authn: unknown role %q", role)
	}

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    i.issuer,
			ID:        tokenID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
		Role:      string(role),
		Scopes:    scopes,
		TokenType: "access",
	}
	if i.audience != "" {
		claims.Audience = jwt.ClaimStrings{i.audience}
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(i.hmacKey)
	if err != nil {
		return "", fmt.Errorf("authn: sign token: %w", err)
	}
	return signed, nil
}
