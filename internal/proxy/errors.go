package proxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

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

// writePipelineRejection writes the structured client error for pipeline
// aborts and reports whether err was one: RejectError (policy rejection)
// renders 403; StrictError (fail-closed operational failure of a strict
// interceptor) renders 503. Other operational pipeline errors return
// false so callers keep their fail-open path. Rejection status stays 403;
// producers set Kind to a request-scoped kind (an empty Kind is
// defensively defaulted to internal_error).
func writePipelineRejection(gw *gateway.NenyaGateway, w http.ResponseWriter, err error) bool {
	var strict *pipeline.StrictError
	if errors.As(err, &strict) {
		gw.Logger.Error("pipeline aborted: strict interceptor failed", "err", err, "interceptor", strict.Interceptor)
		message := strict.Message
		if message == "" {
			message = pipeline.StrictAbortMessage
		}
		kind := strict.Kind
		if kind == "" {
			kind = infra.ErrorKindInternal
		}
		writeStructuredError(w, http.StatusServiceUnavailable, kind, message)
		return true
	}
	var reject *pipeline.RejectError
	if !errors.As(err, &reject) {
		return false
	}
	gw.Logger.Warn("request rejected by pipeline policy", "err", err)
	message := reject.Message
	if message == "" {
		message = "request rejected by pipeline policy"
	}
	kind := reject.Kind
	if kind == "" {
		kind = infra.ErrorKindInternal
	}
	writeStructuredError(w, http.StatusForbidden, kind, message)
	return true
}

func writeStructuredError(w http.ResponseWriter, statusCode int, kind infra.ErrorKind, msg string) {
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
