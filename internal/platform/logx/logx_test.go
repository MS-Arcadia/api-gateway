package logx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/MS-Arcadia/api-gateway/internal/platform/logx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var record map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &record))
	return record
}

func TestNewStampsServiceAttributes(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.New(logx.Config{
		Service: "wallet", Version: "1.2.3", Environment: "local", Output: &buf,
	})
	logger.Info("hello")

	record := decode(t, &buf)
	assert.Equal(t, "wallet", record["service"])
	assert.Equal(t, "1.2.3", record["version"])
	assert.Equal(t, "local", record["env"])
	assert.Equal(t, "hello", record["msg"])
}

func TestRedactsSensitiveKeys(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.New(logx.Config{Service: "wallet", Output: &buf})
	logger.Info("redeem attempt",
		"code", "7K2M-9XQF-3B4T-VW8N",
		"authorization", "Bearer abc.def.ghi",
		"dsn", "postgres://u:p@h/db",
		"user_id", "u-1",
	)

	record := decode(t, &buf)
	assert.Equal(t, "[REDACTED]", record["code"])
	assert.Equal(t, "[REDACTED]", record["authorization"])
	assert.Equal(t, "[REDACTED]", record["dsn"])
	assert.Equal(t, "u-1", record["user_id"], "non-sensitive fields survive")
	assert.NotContains(t, buf.String(), "7K2M")
}

func TestLevelFiltering(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.New(logx.Config{Level: "warn", Output: &buf})
	logger.Debug("invisible")
	logger.Info("also invisible")
	assert.Empty(t, buf.String())

	logger.Warn("visible")
	assert.Contains(t, buf.String(), "visible")
}

func TestCorrelationIDFlowsThroughContext(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.New(logx.Config{Output: &buf})

	ctx := logx.WithLogger(context.Background(), logger)
	ctx = logx.WithCorrelationID(ctx, "corr-42")
	assert.Equal(t, "corr-42", logx.CorrelationID(ctx))

	logx.FromContext(ctx).Info("processing")
	assert.Equal(t, "corr-42", decode(t, &buf)["correlation_id"])
}

func TestWithCorrelationIDIgnoresEmpty(t *testing.T) {
	ctx := logx.WithCorrelationID(context.Background(), "")
	assert.Empty(t, logx.CorrelationID(ctx))
}

func TestFromContextNeverReturnsNil(t *testing.T) {
	assert.NotNil(t, logx.FromContext(context.Background()))
}

func TestTraceIDEmptyWithoutSpan(t *testing.T) {
	assert.Empty(t, logx.TraceID(context.Background()))
}

func TestNopLoggerWritesNothing(t *testing.T) {
	logger := logx.NewNop()
	// Nothing to assert on output; the contract is simply that it never panics
	// and never writes to stdout during a test run.
	logger.Error("dropped")
}

func TestTextFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := logx.New(logx.Config{Format: "text", Service: "wallet", Output: &buf})
	logger.Info("hello")
	assert.Contains(t, buf.String(), "service=wallet")
	assert.NotContains(t, buf.String(), "{")
}
