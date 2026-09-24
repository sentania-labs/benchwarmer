package api

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/sentania-labs/benchwarmer/internal/state"
)

// Error codes. Clients switch on these, never on message text.
const (
	CodeUnauthorized      = "unauthorized"
	CodeInvalidToken      = "invalid_token"
	CodeForbidden         = "forbidden"
	CodeNotFound          = "not_found"
	CodeMethodNotAllowed  = "method_not_allowed"
	CodeBadRequest        = "bad_request"
	CodeBodyTooLarge      = "body_too_large"
	CodeInvalidConfig     = "invalid_config"
	CodeInvalidMode       = "invalid_mode"
	CodeInternal          = "internal_error"
	CodeRejected          = "rejected"
	CodeWorkerUnavailable = "worker_unavailable"
)

// Response headers set by the inference proxy.
const (
	HeaderCondition = "X-Benchwarmer-Condition"
	HeaderRequestID = "X-Benchwarmer-Request-Id"
)

// RequestError lets a Backend refuse a request with a specific status and
// code (for example, a mode change the controller will not accept). Its
// message is shown to the client, so it must not contain secrets. Any other
// Backend error becomes a 500 with a generic message.
type RequestError struct {
	Status  int
	Code    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// Rejectf builds a 409 RequestError with code "rejected".
func Rejectf(format string, a ...any) *RequestError {
	return &RequestError{Status: http.StatusConflict, Code: CodeRejected, Message: fmt.Sprintf(format, a...)}
}

// ActionResponse is the 202 body of POST /api/v1/drain and /api/v1/reload.
type ActionResponse struct {
	Accepted bool   `json:"accepted"`
	Action   string `json:"action"`
	Reason   string `json:"reason"`
}

// ActionRequest is the optional body of POST /api/v1/drain and
// /api/v1/reload.
type ActionRequest struct {
	Reason string `json:"reason,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, Error{Error: msg, Code: code})
}

// WriteUnavailable writes the inference proxy's 503: an Error with code
// worker_unavailable, the user-visible condition, the reason, and (when
// known) when to retry, mirrored in the Retry-After header in seconds.
func WriteUnavailable(w http.ResponseWriter, now time.Time, cond state.Condition, reason string, retryAt *time.Time) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(HeaderCondition, string(cond))
	if retryAt != nil && retryAt.After(now) {
		w.Header().Set("Retry-After", fmt.Sprint(int64(math.Ceil(retryAt.Sub(now).Seconds()))))
	}
	writeJSON(w, http.StatusServiceUnavailable, Error{
		Error: "worker unavailable: " + reason, Code: CodeWorkerUnavailable,
		Condition: cond, Reason: reason, RetryAfter: retryAt,
	})
}
