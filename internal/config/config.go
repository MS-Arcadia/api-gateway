// Package config reads the gateway's settings from the environment.
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/gateway"
	"github.com/MS-Arcadia/api-gateway/internal/platform/config"
)

// Config is everything the gateway needs to start.
type Config struct {
	ServiceName    string
	ServiceVersion string
	Environment    string
	HTTPPort       int
	LogLevel       string
	LogFormat      string

	// Upstreams maps service name to internal base URL.
	Upstreams gateway.Targets

	// CORSOrigins is the allow-list. A single "*" allows any origin, which is for
	// local development only — see the note on gateway.CORS for why.
	CORSOrigins []string

	RateLimitPerMinute int
	UpstreamTimeout    time.Duration

	JWTSecret    string
	JWTPublicKey string
	JWTAlgorithm string
	JWTIssuer    string
	JWTAudience  string
}

// Load reads the environment and validates it.
func Load() (Config, error) {
	env := config.NewLoader()

	cfg := Config{
		ServiceName:    env.String("SERVICE_NAME", "api-gateway"),
		ServiceVersion: env.String("SERVICE_VERSION", "dev"),
		Environment:    env.OneOf("ENVIRONMENT", "local", "local", "development", "staging", "production"),
		HTTPPort:       env.Int("HTTP_PORT", 8090),
		LogLevel:       env.OneOf("LOG_LEVEL", "info", "debug", "info", "warn", "error"),
		LogFormat:      env.OneOf("LOG_FORMAT", "json", "json", "text"),

		Upstreams: gateway.Targets{
			"auth-profile-service":   env.String("AUTH_SERVICE_URL", "http://auth-profile-service:8085"),
			"wallet-service":         env.String("WALLET_SERVICE_URL", "http://wallet-service:8080"),
			"payment-service":        env.String("PAYMENT_SERVICE_URL", "http://payment-service:8081"),
			"catalog-service":        env.String("CATALOG_SERVICE_URL", "http://catalog-service:8082"),
			"order-service":          env.String("ORDER_SERVICE_URL", "http://order-service:8083"),
			"media-service":          env.String("MEDIA_SERVICE_URL", "http://media-service:8084"),
			"notification-service":   env.String("NOTIFICATION_SERVICE_URL", "http://notification-service:8086"),
			"marketplace-service":    env.String("MARKETPLACE_SERVICE_URL", "http://marketplace-service:8087"),
			"review-service":         env.String("REVIEW_SERVICE_URL", "http://review-service:8088"),
			"festival-service":       env.String("FESTIVAL_SERVICE_URL", "http://festival-service:8089"),
			"community-service":      env.String("COMMUNITY_SERVICE_URL", "http://community-service:8091"),
			"recommendation-service": env.String("RECOMMENDATION_SERVICE_URL", "http://recommendation-service:8093"),
		},

		CORSOrigins:        env.Strings("CORS_ORIGINS", []string{"http://localhost:3000"}),
		RateLimitPerMinute: env.Int("RATE_LIMIT_PER_MINUTE", 600),
		UpstreamTimeout:    env.Duration("UPSTREAM_TIMEOUT", 30*time.Second),

		JWTSecret:    env.String("JWT_SECRET", ""),
		JWTPublicKey: env.String("JWT_PUBLIC_KEY", ""),
		JWTAlgorithm: env.OneOf("JWT_ALGORITHM", "HS256", "HS256", "RS256"),
		// Verified rather than optional, and defaulted to what every other service on
		// this platform requires. A token without these claims is rejected by all
		// seven of them, so a gateway that accepted one would pass through something
		// guaranteed to fail one hop later.
		JWTIssuer:   env.String("JWT_ISSUER", "arcadia-auth"),
		JWTAudience: env.String("JWT_AUDIENCE", "arcadia"),
	}

	// The loader collects parse failures rather than panicking on the first, so one
	// boot reports every malformed variable instead of one per restart.
	if err := env.Err(); err != nil {
		return Config{}, err
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.HTTPPort <= 0 || c.HTTPPort > 65535 {
		return fmt.Errorf("config: HTTP_PORT must be a valid port, got %d", c.HTTPPort)
	}

	switch c.JWTAlgorithm {
	case "HS256":
		if c.JWTSecret == "" {
			return fmt.Errorf("config: JWT_SECRET is required with HS256")
		}
		if len(c.JWTSecret) < 32 {
			// Checked at boot rather than on the first request, and the length matters:
			// an HMAC key shorter than its digest is the one mistake that weakens the
			// signature itself rather than merely the operations around it.
			return fmt.Errorf("config: JWT_SECRET must be at least 32 characters, got %d", len(c.JWTSecret))
		}
	case "RS256":
		if c.JWTPublicKey == "" {
			return fmt.Errorf("config: JWT_PUBLIC_KEY is required with RS256")
		}
	}

	if len(c.CORSOrigins) == 0 {
		return fmt.Errorf("config: CORS_ORIGINS is empty, so no browser could call this gateway")
	}

	if c.IsProduction() {
		// A wildcard origin plus a bearer token means any page anywhere can call this
		// API with a token it has phished. Fine on a laptop, never in production.
		for _, origin := range c.CORSOrigins {
			if origin == "*" {
				return fmt.Errorf(`config: CORS_ORIGINS cannot be "*" in production`)
			}
		}
		secret := strings.ToLower(c.JWTSecret)
		if strings.Contains(secret, "change-me") || strings.Contains(secret, "local-development") {
			return fmt.Errorf("config: JWT_SECRET is still a development placeholder")
		}
	}

	for name, target := range c.Upstreams {
		if target == "" {
			return fmt.Errorf("config: %s has no URL configured", name)
		}
	}

	return nil
}

// IsProduction reports whether this is a production deployment.
func (c Config) IsProduction() bool { return c.Environment == "production" }
