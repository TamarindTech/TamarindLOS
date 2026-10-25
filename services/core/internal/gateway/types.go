package gateway

import (
	"fmt"
	"net/http"
)

// Envelope carries the invocation metadata defined in Spec 03 §3.
type Envelope struct {
	IdempotencyKey       string `json:"idempotency_key,omitempty"`
	ExpectedStateVersion *int   `json:"expected_state_version,omitempty"`
	DryRun               bool   `json:"dry_run,omitempty"`
	Reason               string `json:"reason,omitempty"`
	OnBehalfOf           string `json:"on_behalf_of,omitempty"`
	TraceID              string `json:"trace_id,omitempty"`
	// RunID links the call to an open agent run (agent.run_start).
	RunID string `json:"run_id,omitempty"`
}

// Response is returned for every invocation, over REST and MCP alike.
type Response struct {
	Status            string     `json:"status"` // ok | approval_required | error
	Tool              string     `json:"tool"`
	Result            any        `json:"result,omitempty"`
	ApprovalRequestID string     `json:"approval_request_id,omitempty"`
	Error             *ToolError `json:"error,omitempty"`
	InvocationID      string     `json:"invocation_id,omitempty"`
	Replayed          bool       `json:"replayed,omitempty"`
}

// ToolError is a typed error (Spec 03 §5).
type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details any    `json:"details,omitempty"`
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

func newErr(code, format string, args ...any) *ToolError {
	return &ToolError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Error codes.
const (
	ErrValidation      = "VALIDATION_FAILED"
	ErrUnauthenticated = "UNAUTHENTICATED"
	ErrToolNotFound    = "TOOL_NOT_FOUND"
	ErrNotImplemented  = "NOT_IMPLEMENTED"
	ErrNotFound        = "NOT_FOUND"
	ErrStateConflict   = "STATE_CONFLICT"
	ErrPrecondition    = "PRECONDITION_FAILED"
	ErrConsentMissing  = "CONSENT_MISSING"
	ErrPolicyDenied    = "POLICY_DENIED"
	ErrMakerChecker    = "MAKER_CHECKER_VIOLATION"
	ErrIdempotency     = "IDEMPOTENCY_CONFLICT"
	ErrInternal        = "INTERNAL"
)

// HTTPStatus maps a response to an HTTP status code.
func (r *Response) HTTPStatus() int {
	switch r.Status {
	case "ok":
		return http.StatusOK
	case "approval_required":
		return http.StatusAccepted
	}
	switch r.Error.Code {
	case ErrValidation:
		return http.StatusBadRequest
	case ErrUnauthenticated:
		return http.StatusUnauthorized
	case ErrToolNotFound, ErrNotFound:
		return http.StatusNotFound
	case ErrPolicyDenied, ErrMakerChecker:
		return http.StatusForbidden
	case ErrStateConflict, ErrIdempotency:
		return http.StatusConflict
	case ErrPrecondition, ErrConsentMissing:
		return http.StatusUnprocessableEntity
	case ErrNotImplemented:
		return http.StatusNotImplemented
	default:
		return http.StatusInternalServerError
	}
}
