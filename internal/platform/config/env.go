// Package config reads configuration from the process environment.
//
// Twelve-factor rules apply: configuration only ever arrives through the
// environment, secrets are never defaulted to a usable value, and a service
// refuses to boot when a required variable is missing rather than starting in a
// half-configured state.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Loader accumulates parse errors so that a caller can report every
// misconfigured variable at once instead of one boot failure per variable.
type Loader struct {
	errs []error
}

// NewLoader returns an empty Loader.
func NewLoader() *Loader { return &Loader{} }

// Err returns all accumulated problems, or nil when the configuration is sound.
func (l *Loader) Err() error {
	if len(l.errs) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(l.errs))
	for _, err := range l.errs {
		msgs = append(msgs, err.Error())
	}
	return fmt.Errorf("invalid configuration:\n  - %s", strings.Join(msgs, "\n  - "))
}

func (l *Loader) fail(key string, err error) {
	l.errs = append(l.errs, fmt.Errorf("%s: %w", key, err))
}

// String returns the value of key, or def when unset or empty.
func (l *Loader) String(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// MustString returns the value of key and records an error when it is absent.
func (l *Loader) MustString(key string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		l.fail(key, errRequired)
	}
	return v
}

// Int returns key parsed as an int, or def when unset.
func (l *Loader) Int(key string, def int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		l.fail(key, fmt.Errorf("expected an integer, got %q", raw))
		return def
	}
	return v
}

// Int64 returns key parsed as an int64, or def when unset.
func (l *Loader) Int64(key string, def int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		l.fail(key, fmt.Errorf("expected an integer, got %q", raw))
		return def
	}
	return v
}

// Bool accepts 1/t/true/yes/on and their negatives, case-insensitively.
func (l *Loader) Bool(key string, def bool) bool {
	raw := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if raw == "" {
		return def
	}
	switch raw {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	default:
		l.fail(key, fmt.Errorf("expected a boolean, got %q", raw))
		return def
	}
}

// Duration parses Go duration syntax, e.g. "250ms", "30s", "24h".
func (l *Loader) Duration(key string, def time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	v, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(key, fmt.Errorf("expected a duration such as 30s, got %q", raw))
		return def
	}
	return v
}

// Strings splits a comma-separated list, trimming blanks.
func (l *Loader) Strings(key string, def []string) []string {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return def
	}
	return out
}

// OneOf validates that the value is a member of allowed, comparing lowercase.
func (l *Loader) OneOf(key, def string, allowed ...string) string {
	raw := l.String(key, def)
	for _, a := range allowed {
		if strings.EqualFold(raw, a) {
			// The canonical spelling from the allowed list, not the caller's. Matching is
			// case-insensitive so that JWT_ALGORITHM=hs256 is accepted, but the value has to
			// come back as "HS256": consumers compare it against their own constants, and
			// returning the input verbatim made a lowercase environment variable fail at boot
			// with "unsupported algorithm".
			return a
		}
	}
	l.fail(key, fmt.Errorf("expected one of [%s], got %q", strings.Join(allowed, ", "), raw))
	return def
}

type requiredError struct{}

func (requiredError) Error() string { return "is required but was not set" }

var errRequired = requiredError{}
