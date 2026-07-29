package errs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCCode maps a platform code onto a gRPC status code.
func GRPCCode(code Code) codes.Code {
	switch code {
	case CodeInvalidArgument:
		return codes.InvalidArgument
	case CodeUnauthenticated:
		return codes.Unauthenticated
	case CodePermissionDenied:
		return codes.PermissionDenied
	case CodeNotFound:
		return codes.NotFound
	case CodeAlreadyExists:
		return codes.AlreadyExists
	case CodeConflict:
		// gRPC has no dedicated "conflict"; Aborted is the canonical mapping for
		// a state-based rejection that a retry will not fix on its own.
		return codes.Aborted
	case CodeFailedPrecondition:
		return codes.FailedPrecondition
	case CodeResourceExhausted:
		return codes.ResourceExhausted
	case CodeAborted:
		return codes.Aborted
	case CodeUnavailable:
		return codes.Unavailable
	case CodeDeadlineExceeded:
		return codes.DeadlineExceeded
	default:
		return codes.Internal
	}
}

// HTTPStatus maps a platform code onto an HTTP status code.
func HTTPStatus(code Code) int {
	switch code {
	case CodeInvalidArgument:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodePermissionDenied:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeAlreadyExists, CodeConflict, CodeAborted:
		return http.StatusConflict
	case CodeFailedPrecondition:
		return http.StatusUnprocessableEntity
	case CodeResourceExhausted:
		return http.StatusTooManyRequests
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

// ToGRPC converts any error into a gRPC status error. Internal failures are
// deliberately flattened to a generic message so that implementation details
// never leak across the wire; the real cause stays in the logs.
func ToGRPC(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}

	var domain *Error
	if !errors.As(err, &domain) {
		return status.Error(codes.Internal, "internal error")
	}
	if domain.Code == CodeInternal {
		return status.Error(codes.Internal, "internal error")
	}

	msg := domain.Message
	if domain.Reason != "" {
		msg = domain.Reason + ": " + msg
	}
	return status.Error(GRPCCode(domain.Code), msg)
}

// Problem is an RFC 7807-flavoured error body used by the REST adapters.
type Problem struct {
	// Type is a stable URI-ish identifier, e.g. "urn:arcadia:error:CONFLICT".
	Type string `json:"type"`
	// Title is the platform code.
	Title string `json:"title"`
	// Status is the HTTP status code, repeated in the body for convenience.
	Status int `json:"status"`
	// Detail is the human-readable message.
	Detail string `json:"detail"`
	// Reason is the fine-grained discriminator, when present.
	Reason string `json:"reason,omitempty"`
	// Details carries safe structured context.
	Details map[string]any `json:"details,omitempty"`
	// TraceID lets a user quote one identifier to support.
	TraceID string `json:"trace_id,omitempty"`
}

// ToProblem converts an error into a Problem and its HTTP status code.
func ToProblem(err error, traceID string) (int, Problem) {
	if err == nil {
		return http.StatusOK, Problem{}
	}

	var domain *Error
	if !errors.As(err, &domain) {
		switch {
		case errors.Is(err, context.Canceled):
			domain = &Error{Code: CodeDeadlineExceeded, Message: "request canceled"}
		case errors.Is(err, context.DeadlineExceeded):
			domain = &Error{Code: CodeDeadlineExceeded, Message: "request deadline exceeded"}
		default:
			domain = &Error{Code: CodeInternal, Message: "internal error"}
		}
	}

	detail := domain.Message
	details := domain.Details
	if domain.Code == CodeInternal {
		detail = "internal error"
		details = nil
	}

	st := HTTPStatus(domain.Code)
	return st, Problem{
		Type:    "urn:arcadia:error:" + string(domain.Code),
		Title:   string(domain.Code),
		Status:  st,
		Detail:  detail,
		Reason:  domain.Reason,
		Details: details,
		TraceID: traceID,
	}
}

// WriteProblem renders an error as application/problem+json.
func WriteProblem(w http.ResponseWriter, err error, traceID string) {
	st, problem := ToProblem(err, traceID)
	w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
	w.WriteHeader(st)
	// A failure to encode here means the client is already gone; there is
	// nothing useful left to do with the error.
	_ = json.NewEncoder(w).Encode(problem)
}

// FromGRPC converts a gRPC status error received from a downstream service back
// into a platform error, so that a failure at the edge of one service keeps its
// meaning as it propagates through the next.
func FromGRPC(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return Internal("unexpected transport error").WithCause(err)
	}
	code := CodeInternal
	switch st.Code() {
	case codes.InvalidArgument:
		code = CodeInvalidArgument
	case codes.Unauthenticated:
		code = CodeUnauthenticated
	case codes.PermissionDenied:
		code = CodePermissionDenied
	case codes.NotFound:
		code = CodeNotFound
	case codes.AlreadyExists:
		code = CodeAlreadyExists
	case codes.Aborted:
		code = CodeConflict
	case codes.FailedPrecondition:
		code = CodeFailedPrecondition
	case codes.ResourceExhausted:
		code = CodeResourceExhausted
	case codes.Unavailable:
		code = CodeUnavailable
	case codes.DeadlineExceeded, codes.Canceled:
		code = CodeDeadlineExceeded
	}
	return (&Error{Code: code, Message: st.Message()}).WithCause(err)
}
