// Command api-gateway is Arcadia's single public entry point.
//
// It routes one origin onto eleven services, and does four things on the way:
// correlation ids, CORS, rate limiting, and rejecting a malformed token before it
// costs a service anything.
//
// It deliberately does *not* do authorisation. Each service owns the role rules
// for its own endpoints, and a copy of those rules here would be a second list to
// keep in step — see the note on gateway.VerifyToken.
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MS-Arcadia/api-gateway/internal/config"
	"github.com/MS-Arcadia/api-gateway/internal/gateway"
	"github.com/MS-Arcadia/api-gateway/internal/platform/authn"
	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
)

func main() {
	// `-health-check` makes the binary probe itself and exit 0 or 1.
	//
	// It exists because the runtime image is `scratch`: there is no curl and no shell,
	// so Docker's HEALTHCHECK has nothing to run but this binary. The alternative is
	// shipping a shell into the image, which is exactly the attack surface scratch is
	// chosen to avoid.
	probe := flag.Bool("health-check", false, "probe the local gateway and exit")
	flag.Parse()

	if *probe {
		os.Exit(healthCheck())
	}

	if err := run(); err != nil {
		// Written to stderr rather than logged: this runs before the logger exists,
		// and a configuration failure has to be readable in `docker logs` whatever
		// the log format is set to.
		fmt.Fprintf(os.Stderr, "api-gateway: %v\n", err)
		os.Exit(1)
	}
}

// healthCheck asks the local gateway for /livez.
func healthCheck() int {
	port := os.Getenv("HTTP_PORT")
	if port == "" {
		port = "8090"
	}

	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://127.0.0.1:" + port + "/livez")
	if err != nil {
		fmt.Fprintf(os.Stderr, "health check: %v\n", err)
		return 1
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health check: /livez answered %d\n", response.StatusCode)
		return 1
	}
	return 0
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := logx.New(logx.Config{
		Level:       cfg.LogLevel,
		Format:      cfg.LogFormat,
		Service:     cfg.ServiceName,
		Version:     cfg.ServiceVersion,
		Environment: cfg.Environment,
	})

	table, err := gateway.NewTable(cfg.Upstreams)
	if err != nil {
		return err
	}

	verifier, err := authn.NewVerifier(authn.VerifierConfig{
		Algorithm:    authn.Algorithm(cfg.JWTAlgorithm),
		Secret:       cfg.JWTSecret,
		PublicKeyPEM: cfg.JWTPublicKey,
		Issuer:       cfg.JWTIssuer,
		Audience:     cfg.JWTAudience,
	})
	if err != nil {
		return err
	}

	server := gateway.New(gateway.Options{
		ServiceName:        cfg.ServiceName,
		ServiceVersion:     cfg.ServiceVersion,
		Port:               cfg.HTTPPort,
		Table:              table,
		Verifier:           verifier,
		Logger:             logger,
		CORSOrigins:        cfg.CORSOrigins,
		RateLimitPerMinute: cfg.RateLimitPerMinute,
		UpstreamTimeout:    cfg.UpstreamTimeout,
	})

	// SIGTERM is what an orchestrator sends; SIGINT is Ctrl-C. Both drain rather
	// than drop, because a request in flight here is a request in flight somewhere
	// behind here too.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	for _, upstream := range table.Upstreams() {
		logger.InfoContext(ctx, "route",
			"prefix", upstream.Prefix,
			"upstream", upstream.Name,
			"target", upstream.Target.String(),
		)
	}
	logger.InfoContext(ctx, "api-gateway starting",
		"environment", cfg.Environment,
		"port", cfg.HTTPPort,
		"cors_origins", cfg.CORSOrigins,
		"rate_limit_per_minute", cfg.RateLimitPerMinute,
	)

	if err := server.Start(ctx); err != nil {
		return err
	}

	logger.InfoContext(context.Background(), "api-gateway stopped")
	return nil
}
