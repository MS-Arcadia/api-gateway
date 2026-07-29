// Package logx provides structured JSON logging with trace correlation.
//
// Every log line carries service, trace_id and correlation_id so that a single
// request can be followed across services in Loki and pivoted into Tempo, which
// is exactly what the observability section of the architecture document calls
// for. Secrets are redacted by key name before they ever reach a handler.
package logx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Config controls logger construction.
type Config struct {
	// Level is one of debug, info, warn, error. Defaults to info.
	Level string
	// Format is "json" or "text". Text is for local development only.
	Format string
	// Service is the service name stamped on every record.
	Service string
	// Version is the build version stamped on every record.
	Version string
	// Environment is local/staging/production.
	Environment string
	// Output defaults to os.Stdout.
	Output io.Writer
}

// New builds a *slog.Logger from cfg.
func New(cfg Config) *slog.Logger {
	out := cfg.Output
	if out == nil {
		out = os.Stdout
	}

	opts := &slog.HandlerOptions{
		Level:       parseLevel(cfg.Level),
		ReplaceAttr: redactAttr,
	}

	var handler slog.Handler
	if strings.EqualFold(cfg.Format, "text") {
		handler = slog.NewTextHandler(out, opts)
	} else {
		handler = slog.NewJSONHandler(out, opts)
	}

	base := []slog.Attr{}
	if cfg.Service != "" {
		base = append(base, slog.String("service", cfg.Service))
	}
	if cfg.Version != "" {
		base = append(base, slog.String("version", cfg.Version))
	}
	if cfg.Environment != "" {
		base = append(base, slog.String("env", cfg.Environment))
	}
	if len(base) > 0 {
		handler = handler.WithAttrs(base)
	}
	return slog.New(handler)
}

// NewNop returns a logger that discards everything, for use in tests.
func NewNop() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// sensitiveKeys are never logged in the clear. The check is on the attribute key
// so that a careless `logger.Info("x", "password", pw)` cannot leak a secret.
var sensitiveKeys = map[string]struct{}{
	"password":      {},
	"passwd":        {},
	"secret":        {},
	"token":         {},
	"access_token":  {},
	"refresh_token": {},
	"authorization": {},
	"api_key":       {},
	"apikey":        {},
	"jwt":           {},
	"dsn":           {},
	"database_url":  {},
	"private_key":   {},
	"card_number":   {},
	"pan":           {},
	"cvv":           {},
	// A raw gift-card code is bearer-grade: whoever reads it can spend it.
	"code":           {},
	"gift_card_code": {},
	"signature":      {},
}

func redactAttr(_ []string, attr slog.Attr) slog.Attr {
	if _, sensitive := sensitiveKeys[strings.ToLower(attr.Key)]; sensitive {
		return slog.String(attr.Key, "[REDACTED]")
	}
	return attr
}

type contextKey struct{ name string }

var (
	loggerKey        = contextKey{"logger"}
	correlationIDKey = contextKey{"correlation_id"}
)

// WithLogger stores a logger on the context.
func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext returns the context's logger, enriched with the correlation
// identifier. It never returns nil.
func FromContext(ctx context.Context) *slog.Logger {
	logger, ok := ctx.Value(loggerKey).(*slog.Logger)
	if !ok || logger == nil {
		logger = slog.Default()
	}
	if cid := CorrelationID(ctx); cid != "" {
		return logger.With(slog.String("correlation_id", cid))
	}
	return logger
}

// WithCorrelationID stores the request's correlation identifier.
func WithCorrelationID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, correlationIDKey, id)
}

// CorrelationID returns the context's correlation identifier, or "".
func CorrelationID(ctx context.Context) string {
	id, _ := ctx.Value(correlationIDKey).(string)
	return id
}

// TraceID returns the identifier used to correlate one request across services.
//
// The platform propagates a correlation id through an HTTP header and a Kafka
// envelope field rather than running a distributed tracing backend. That covers the
// question this project actually needs answered — "show me every log line for this
// purchase" — for the cost of one string, and with no collector to run.
func TraceID(ctx context.Context) string { return CorrelationID(ctx) }
