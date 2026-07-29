// Package errs defines the platform's error taxonomy.
//
// Domain and application code never returns transport-specific errors. It
// returns an *Error carrying a semantic Code, and the inbound adapters translate
// that code into a gRPC status or an HTTP problem document. This is what keeps
// the dependency rule intact: the domain has no idea gRPC exists.
package errs

import (
	"errors"
	"fmt"
)

// Code classifies a failure independently of any transport.
type Code string

// The complete set of codes. Adding one requires updating the gRPC and HTTP
// mappings in this package.
const (
	// CodeInvalidArgument — the request is malformed or violates a field rule.
	CodeInvalidArgument Code = "INVALID_ARGUMENT"
	// CodeUnauthenticated — no valid credentials were presented.
	CodeUnauthenticated Code = "UNAUTHENTICATED"
	// CodePermissionDenied — authenticated, but the role is not allowed.
	CodePermissionDenied Code = "PERMISSION_DENIED"
	// CodeNotFound — the addressed resource does not exist.
	CodeNotFound Code = "NOT_FOUND"
	// CodeAlreadyExists — creating the resource would violate uniqueness.
	CodeAlreadyExists Code = "ALREADY_EXISTS"
	// CodeConflict — the request contradicts the resource's current state, e.g.
	// redeeming a gift card that is already USED.
	CodeConflict Code = "CONFLICT"
	// CodeFailedPrecondition — a business rule blocks the operation, e.g.
	// insufficient funds or a frozen wallet.
	CodeFailedPrecondition Code = "FAILED_PRECONDITION"
	// CodeResourceExhausted — a rate limit or quota was hit.
	CodeResourceExhausted Code = "RESOURCE_EXHAUSTED"
	// CodeAborted — an optimistic-concurrency conflict; retrying may succeed.
	CodeAborted Code = "ABORTED"
	// CodeUnavailable — a downstream dependency is unreachable.
	CodeUnavailable Code = "UNAVAILABLE"
	// CodeDeadlineExceeded — the operation timed out.
	CodeDeadlineExceeded Code = "DEADLINE_EXCEEDED"
	// CodeInternal — a bug or an unexpected failure. Details are never exposed.
	CodeInternal Code = "INTERNAL"
)

// Error is a domain-level error with a machine-readable code, a message that is
// safe to show a caller, and optional structured details.
type Error struct {
	Code    Code
	Message string
	// Reason is a stable, fine-grained discriminator inside a Code, e.g.
	// "INSUFFICIENT_FUNDS" or "GIFT_CARD_ALREADY_USED". Clients may branch on it.
	Reason string
	// Details carries safe-to-expose context such as the offending field.
	Details map[string]any
	// cause is wrapped for logging but never rendered to a caller.
	cause error
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap exposes the wrapped cause to errors.Is / errors.As.
func (e *Error) Unwrap() error { return e.cause }

// WithReason attaches a fine-grained reason discriminator.
func (e *Error) WithReason(reason string) *Error {
	e.Reason = reason
	return e
}

// WithDetail attaches one key/value pair of safe context.
func (e *Error) WithDetail(key string, value any) *Error {
	if e.Details == nil {
		e.Details = make(map[string]any, 2)
	}
	e.Details[key] = value
	return e
}

// WithCause wraps an underlying error for logs and traces.
func (e *Error) WithCause(cause error) *Error {
	e.cause = cause
	return e
}

func newError(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Constructors, one per code.

// InvalidArgument reports a malformed request.
func InvalidArgument(format string, args ...any) *Error {
	return newError(CodeInvalidArgument, format, args...)
}

// Unauthenticated reports missing or invalid credentials.
func Unauthenticated(format string, args ...any) *Error {
	return newError(CodeUnauthenticated, format, args...)
}

// PermissionDenied reports an RBAC rejection.
func PermissionDenied(format string, args ...any) *Error {
	return newError(CodePermissionDenied, format, args...)
}

// NotFound reports a missing resource.
func NotFound(format string, args ...any) *Error {
	return newError(CodeNotFound, format, args...)
}

// AlreadyExists reports a uniqueness violation.
func AlreadyExists(format string, args ...any) *Error {
	return newError(CodeAlreadyExists, format, args...)
}

// Conflict reports a state contradiction.
func Conflict(format string, args ...any) *Error {
	return newError(CodeConflict, format, args...)
}

// FailedPrecondition reports a business-rule rejection.
func FailedPrecondition(format string, args ...any) *Error {
	return newError(CodeFailedPrecondition, format, args...)
}

// ResourceExhausted reports a rate-limit or quota rejection.
func ResourceExhausted(format string, args ...any) *Error {
	return newError(CodeResourceExhausted, format, args...)
}

// Aborted reports a concurrency conflict that a retry may resolve.
func Aborted(format string, args ...any) *Error {
	return newError(CodeAborted, format, args...)
}

// Unavailable reports an unreachable dependency.
func Unavailable(format string, args ...any) *Error {
	return newError(CodeUnavailable, format, args...)
}

// DeadlineExceeded reports a timeout.
func DeadlineExceeded(format string, args ...any) *Error {
	return newError(CodeDeadlineExceeded, format, args...)
}

// Internal reports an unexpected failure. The cause is logged, never returned.
func Internal(format string, args ...any) *Error {
	return newError(CodeInternal, format, args...)
}

// Wrap converts an arbitrary error into an *Error, preserving an existing code
// when there is one and defaulting to CodeInternal otherwise.
func Wrap(err error, format string, args ...any) *Error {
	if err == nil {
		return nil
	}
	msg := fmt.Sprintf(format, args...)
	var domain *Error
	if errors.As(err, &domain) {
		return &Error{
			Code:    domain.Code,
			Message: msg,
			Reason:  domain.Reason,
			Details: domain.Details,
			cause:   err,
		}
	}
	return &Error{Code: CodeInternal, Message: msg, cause: err}
}

// CodeOf extracts the code from an error, returning CodeInternal for anything
// that is not a platform error and "" for a nil error.
func CodeOf(err error) Code {
	if err == nil {
		return ""
	}
	var domain *Error
	if errors.As(err, &domain) {
		return domain.Code
	}
	return CodeInternal
}

// ReasonOf extracts the fine-grained reason, or "" when there is none.
func ReasonOf(err error) string {
	var domain *Error
	if errors.As(err, &domain) {
		return domain.Reason
	}
	return ""
}

// Is reports whether err carries the given code.
func Is(err error, code Code) bool { return CodeOf(err) == code }

// IsRetryable reports whether a caller (or a Kafka consumer) should retry rather
// than route the message to the dead-letter queue. Business rejections are
// permanent; infrastructure failures are not.
func IsRetryable(err error) bool {
	switch CodeOf(err) {
	case CodeUnavailable, CodeDeadlineExceeded, CodeAborted, CodeInternal:
		return true
	default:
		return false
	}
}
