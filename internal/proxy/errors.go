package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// containsAny checks if the string contains any of the substrings.
func containsAny(s string, substrs ...string) bool {
	for _, sub := range substrs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// writeHTTPError renders an httpError with its structured kind, falling back
// to invalid_request when the kind is unset. nil herr is a documented no-op.
// Non-error sentinels (e.g. the 204 cache-hit httpError) must be handled by
// the caller before calling this.
func writeHTTPError(w http.ResponseWriter, herr *httpError) {
	if herr == nil {
		return
	}
	kind := herr.Kind
	if kind == "" {
		kind = infra.ErrorKindInvalidRequest
	}
	writeStructuredError(w, herr.Code, kind, herr.Message)
}

// statusClientClosed is a non-standard sentinel status (nginx convention)
// for requests whose context was canceled mid-read: the client is gone and
// no error body should be rendered.
const statusClientClosed = 499

// errClientClosed is the shared sentinel for requests whose context was
// canceled mid-read; render sites must acknowledge it via
// renderBodyReadError/acknowledgeClientClosed instead of writing a body.
var errClientClosed = &httpError{Code: statusClientClosed, Message: "Request canceled"}

// acknowledgeClientClosed emits an empty statusClientClosed status with no
// body: the client is gone, but writing the status keeps access logs and
// status-code metrics honest instead of recording a phantom 200.
func acknowledgeClientClosed(w http.ResponseWriter) {
	w.WriteHeader(statusClientClosed)
}

// renderBodyReadError renders an error returned by readRequestBody (or an
// equivalent body-read helper): client-closed requests are acknowledged with
// the empty 499 status, everything else is rendered via writeHTTPError.
// Returns true when the caller must stop processing.
func renderBodyReadError(w http.ResponseWriter, herr *httpError) bool {
	if herr == nil {
		return false
	}
	if herr.Code == statusClientClosed {
		acknowledgeClientClosed(w)
		return true
	}
	writeHTTPError(w, herr)
	return true
}

// readRequestBody reads the (already MaxBytesReader-wrapped) request body,
// uniformly distinguishing client disconnects from genuine payload errors
// across all endpoints: a canceled context yields the 499 no-render
// sentinel, a MaxBytesError the 413/payload_too_large rendering, and any
// other transport error a 400/invalid_request. On a non-nil returned error
// the caller must pass it to renderBodyReadError and stop processing when
// it returns true. Extra attrs are attached to the failure log line (e.g.
// provider context); the "err" key is reserved and must not be passed in
// attrs.
func readRequestBody(r *http.Request, gw *gateway.NenyaGateway, logMsg string, attrs ...any) ([]byte, *httpError) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err == nil {
		return bodyBytes, nil
	}
	// Best-effort: background cancellation may land after ReadAll returns,
	// in which case the payload branch below runs — writing a 413 to an
	// already-disconnected client is harmless either way.
	if r.Context().Err() != nil {
		gw.Logger.Debug("request canceled mid-body-read", "err", err)
		return nil, errClientClosed
	}
	logAttrs := append([]any{"err", err}, attrs...)
	gw.Logger.Warn(logMsg, logAttrs...)
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return nil, &httpError{
			Code:    http.StatusRequestEntityTooLarge,
			Message: "Payload too large or malformed",
			Kind:    infra.ErrorKindPayloadTooLarge,
		}
	}
	return nil, &httpError{
		Code:    http.StatusBadRequest,
		Message: "Failed to read request body",
		Kind:    infra.ErrorKindInvalidRequest,
	}
}

// classifyError maps upstream responses to error kinds.
func classifyError(statusCode int, body []byte) infra.ErrorKind {
	switch {
	case statusCode == http.StatusTooManyRequests || statusCode == 529:
		// 529 is "Provider Overloaded" (non-standard, used by xAI and some LLM gateways)
		return infra.ErrorKindRateLimited
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return infra.ErrorKindAuthFailed
	case statusCode == http.StatusNotFound:
		return infra.ErrorKindModelNotFound
	case statusCode == http.StatusRequestEntityTooLarge:
		// Cross-check 413 body for context-overflow messages before classifying
		// as payload-too-large — some providers return 413 for context limits
		if inferErrorKind(body) == infra.ErrorKindContextExceeded {
			return infra.ErrorKindContextExceeded
		}
		return infra.ErrorKindPayloadTooLarge
	case statusCode == http.StatusBadRequest:
		return inferErrorKind(body)
	case statusCode >= 500:
		return classifyServerError(body)
	case statusCode == 0:
		return infra.ErrorKindNetworkError
	default:
		return infra.ErrorKindInvalidRequest
	}
}

// inferErrorKind tries to determine the error kind from the error body.
func inferErrorKind(body []byte) infra.ErrorKind {
	if len(body) == 0 {
		return infra.ErrorKindInvalidRequest
	}
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return infra.ErrorKindInvalidRequest
	}
	msg := strings.ToLower(parsed.Error.Message)
	switch {
	case containsAny(msg, "context_length", "context length", "max_tokens", "too many tokens",
		"exceeded model token", "maximum prompt length", "exceeds the available context",
		"model_context_window", "too large for model"):
		return infra.ErrorKindContextExceeded
	case containsAny(msg, "rate limit", "rate_limit"):
		return infra.ErrorKindRateLimited
	case containsAny(msg, "timeout", "timed out", "deadline exceeded"):
		return infra.ErrorKindProviderTimeout
	default:
		return infra.ErrorKindInvalidRequest
	}
}

// classifyServerError categorizes 5xx upstream responses.
func classifyServerError(body []byte) infra.ErrorKind {
	if len(body) > 0 && bytes.Contains(bytes.ToLower(body), []byte("timeout")) {
		return infra.ErrorKindProviderTimeout
	}
	return infra.ErrorKindProviderError
}

// writeStructuredError writes a structured error response to the HTTP writer.
func writeStructuredError(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg string) {
	writeStructuredErrorWithContext(w, statusCode, kind, msg, "", "")
}

// writeStructuredErrorWithContext writes a structured error with optional provider and model context.
func writeStructuredErrorWithContext(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg, provider, model string) {
	if w == nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	resp := infra.ErrorResponse{
		Error: infra.ErrorBody{
			Message: msg,
		},
		Kind: kind,
	}
	_ = json.NewEncoder(w).Encode(resp)
}
