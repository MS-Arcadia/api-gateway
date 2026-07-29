package errs_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/MS-Arcadia/api-gateway/internal/platform/errs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestCodeOf(t *testing.T) {
	assert.Equal(t, errs.Code(""), errs.CodeOf(nil))
	assert.Equal(t, errs.CodeNotFound, errs.CodeOf(errs.NotFound("nope")))
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(errors.New("raw")))
}

func TestWrapPreservesCode(t *testing.T) {
	base := errs.FailedPrecondition("insufficient funds").WithReason("INSUFFICIENT_FUNDS")
	wrapped := errs.Wrap(base, "debit wallet %s", "w-1")

	assert.Equal(t, errs.CodeFailedPrecondition, errs.CodeOf(wrapped))
	assert.Equal(t, "INSUFFICIENT_FUNDS", errs.ReasonOf(wrapped))
	assert.Contains(t, wrapped.Error(), "debit wallet w-1")
	assert.ErrorIs(t, wrapped, base)
}

func TestWrapRawErrorBecomesInternal(t *testing.T) {
	cause := errors.New("connection reset")
	wrapped := errs.Wrap(cause, "load wallet")
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(wrapped))
	assert.ErrorIs(t, wrapped, cause)
}

func TestWrapNilIsNil(t *testing.T) {
	assert.Nil(t, errs.Wrap(nil, "anything"))
}

func TestDetailsAndCause(t *testing.T) {
	cause := fmt.Errorf("boom")
	err := errs.InvalidArgument("bad amount").
		WithReason("AMOUNT_NOT_POSITIVE").
		WithDetail("field", "amount").
		WithCause(cause)

	assert.Equal(t, "AMOUNT_NOT_POSITIVE", err.Reason)
	assert.Equal(t, "amount", err.Details["field"])
	assert.ErrorIs(t, err, cause)
}

func TestIsRetryable(t *testing.T) {
	retryable := []error{
		errs.Unavailable("psp down"),
		errs.DeadlineExceeded("slow"),
		errs.Aborted("version conflict"),
		errs.Internal("bug"),
		errors.New("unclassified"),
	}
	for _, err := range retryable {
		assert.True(t, errs.IsRetryable(err), "%v should be retryable", err)
	}

	permanent := []error{
		errs.InvalidArgument("bad"),
		errs.NotFound("gone"),
		errs.Conflict("already used"),
		errs.FailedPrecondition("insufficient funds"),
		errs.PermissionDenied("nope"),
		errs.AlreadyExists("dup"),
		errs.ResourceExhausted("slow down"),
	}
	for _, err := range permanent {
		assert.False(t, errs.IsRetryable(err), "%v should not be retryable", err)
	}
}

func TestToGRPCMapping(t *testing.T) {
	tests := []struct {
		err  error
		want codes.Code
	}{
		{errs.InvalidArgument("x"), codes.InvalidArgument},
		{errs.Unauthenticated("x"), codes.Unauthenticated},
		{errs.PermissionDenied("x"), codes.PermissionDenied},
		{errs.NotFound("x"), codes.NotFound},
		{errs.AlreadyExists("x"), codes.AlreadyExists},
		{errs.Conflict("x"), codes.Aborted},
		{errs.FailedPrecondition("x"), codes.FailedPrecondition},
		{errs.ResourceExhausted("x"), codes.ResourceExhausted},
		{errs.Unavailable("x"), codes.Unavailable},
		{errs.DeadlineExceeded("x"), codes.DeadlineExceeded},
		{errs.Internal("x"), codes.Internal},
		{errors.New("raw"), codes.Internal},
	}
	for _, tc := range tests {
		got := status.Code(errs.ToGRPC(tc.err))
		assert.Equal(t, tc.want, got, "for %v", tc.err)
	}
	assert.Nil(t, errs.ToGRPC(nil))
}

func TestToGRPCHidesInternalDetail(t *testing.T) {
	err := errs.Internal("failed to reach postgres at 10.0.0.5:5432")
	st, ok := status.FromError(errs.ToGRPC(err))
	require.True(t, ok)
	assert.Equal(t, "internal error", st.Message())
	assert.NotContains(t, st.Message(), "10.0.0.5")
}

func TestToGRPCIncludesReason(t *testing.T) {
	err := errs.FailedPrecondition("balance is 100, needed 500").WithReason("INSUFFICIENT_FUNDS")
	st, ok := status.FromError(errs.ToGRPC(err))
	require.True(t, ok)
	assert.Contains(t, st.Message(), "INSUFFICIENT_FUNDS")
}

func TestHTTPStatusMapping(t *testing.T) {
	tests := map[errs.Code]int{
		errs.CodeInvalidArgument:    http.StatusBadRequest,
		errs.CodeUnauthenticated:    http.StatusUnauthorized,
		errs.CodePermissionDenied:   http.StatusForbidden,
		errs.CodeNotFound:           http.StatusNotFound,
		errs.CodeConflict:           http.StatusConflict,
		errs.CodeAlreadyExists:      http.StatusConflict,
		errs.CodeFailedPrecondition: http.StatusUnprocessableEntity,
		errs.CodeResourceExhausted:  http.StatusTooManyRequests,
		errs.CodeUnavailable:        http.StatusServiceUnavailable,
		errs.CodeDeadlineExceeded:   http.StatusGatewayTimeout,
		errs.CodeInternal:           http.StatusInternalServerError,
	}
	for code, want := range tests {
		assert.Equal(t, want, errs.HTTPStatus(code), "for %s", code)
	}
}

func TestToProblem(t *testing.T) {
	err := errs.Conflict("gift card already used").
		WithReason("GIFT_CARD_ALREADY_USED").
		WithDetail("code", "ABC-123")

	st, problem := errs.ToProblem(err, "trace-1")
	assert.Equal(t, http.StatusConflict, st)
	assert.Equal(t, "urn:arcadia:error:CONFLICT", problem.Type)
	assert.Equal(t, "CONFLICT", problem.Title)
	assert.Equal(t, "GIFT_CARD_ALREADY_USED", problem.Reason)
	assert.Equal(t, "ABC-123", problem.Details["code"])
	assert.Equal(t, "trace-1", problem.TraceID)
}

func TestToProblemRedactsInternal(t *testing.T) {
	err := errs.Internal("dsn=postgres://user:pw@host/db").WithDetail("dsn", "secret")
	st, problem := errs.ToProblem(err, "trace-2")
	assert.Equal(t, http.StatusInternalServerError, st)
	assert.Equal(t, "internal error", problem.Detail)
	assert.Nil(t, problem.Details)
}

func TestFromGRPCRoundTrip(t *testing.T) {
	original := errs.FailedPrecondition("insufficient funds")
	transported := errs.ToGRPC(original)
	back := errs.FromGRPC(transported)
	assert.Equal(t, errs.CodeFailedPrecondition, errs.CodeOf(back))
}

func TestFromGRPCNil(t *testing.T) {
	assert.Nil(t, errs.FromGRPC(nil))
}
