package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
)

const (
	maxErrorBodySize = 64 * 1024 // 64KB limit for error bodies
	maxParamLength   = 256       // 256 char limit for param/code fields
)

// ErrorType represents the type of error that occurred during request processing.
// Maps to upstream provider error types (provider_error, rate_limit_error, etc.).
type ErrorType string

const (
	// ErrorTypeProvider indicates an upstream provider error (5xx)
	ErrorTypeProvider ErrorType = "provider_error"
	// ErrorTypeRateLimit indicates a rate limit error (429)
	ErrorTypeRateLimit ErrorType = "rate_limit_error"
	// ErrorTypeQuotaExhausted indicates a quota exhaustion error (429)
	ErrorTypeQuotaExhausted ErrorType = "quota_exhausted_error"
	// ErrorTypeInvalidRequest indicates a client error (4xx)
	ErrorTypeInvalidRequest ErrorType = "invalid_request_error"
	// ErrorTypeAuthentication indicates an authentication error (401)
	ErrorTypeAuthentication ErrorType = "authentication_error"
	// ErrorTypeNotFound indicates a not found error (404)
	ErrorTypeNotFound ErrorType = "not_found_error"
	ErrorTypeGateway  ErrorType = "gateway_error"
	ErrorTypeBouncer  ErrorType = "bouncer_error"
)

// GatewayError represents an error that occurred in the gateway, with structured
// fields for logging, client responses, and debugging. The Err field contains
// the original error for debugging (not exposed to clients).
type GatewayError struct {
	Type       ErrorType `json:"type"`
	Message    string    `json:"message"`
	StatusCode int       `json:"status_code"`
	Provider   string    `json:"provider,omitempty"`
	Param      *string   `json:"param,omitempty"`
	Code       *string   `json:"code,omitempty"`
	// Original error for debugging (not exposed to clients)
	Err error `json:"-"`
}

// OpenAIErrorEnvelope is the OpenAI-compatible response envelope for errors.
// Clients expect errors in this format for OpenAI wire format.
type OpenAIErrorEnvelope struct {
	Error OpenAIErrorObject `json:"error"`
}

// OpenAIErrorObject is the public error object exposed to clients in OpenAI format.
// Contains type, message, optional param, code, and optional error_kind fields.
type OpenAIErrorObject struct {
	Type      ErrorType       `json:"type"`
	Message   string          `json:"message"`
	Param     *string         `json:"param,omitempty"`
	Code      *string         `json:"code,omitempty"`
	ErrorKind infra.ErrorKind `json:"error_kind,omitempty"`
}

// Error implements the error interface
func (e *GatewayError) Error() string {
	if e.Provider != "" {
		return fmt.Sprintf("[%s] %s: %s", e.Provider, e.Type, e.Message)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

// Unwrap returns the wrapped error
func (e *GatewayError) Unwrap() error {
	return e.Err
}

// HTTPStatusCode returns the appropriate HTTP status code for the error type
func (e *GatewayError) HTTPStatusCode() int {
	if e.StatusCode != 0 {
		return e.StatusCode
	}
	switch e.Type {
	case ErrorTypeRateLimit:
		return http.StatusTooManyRequests
	case ErrorTypeInvalidRequest:
		return http.StatusBadRequest
	case ErrorTypeAuthentication:
		return http.StatusUnauthorized
	case ErrorTypeNotFound:
		return http.StatusNotFound
	case ErrorTypeProvider:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// WithParam sets the param field and returns the error
func (e *GatewayError) WithParam(param string) *GatewayError {
	if len(param) > maxParamLength {
		param = param[:maxParamLength]
	}
	e.Param = &param
	return e
}

// WithCode sets the code field and returns the error
func (e *GatewayError) WithCode(code string) *GatewayError {
	if len(code) > maxParamLength {
		code = code[:maxParamLength]
	}
	e.Code = &code
	return e
}

// NewProviderError creates a new provider error
func NewProviderError(provider string, statusCode int, message string, err error) *GatewayError {
	return &GatewayError{
		Type:       ErrorTypeProvider,
		Message:    message,
		StatusCode: statusCode,
		Provider:   provider,
		Err:        err,
	}
}

// NewRateLimitError creates a new rate limit error
func NewRateLimitError(provider string, message string) *GatewayError {
	return &GatewayError{
		Type:       ErrorTypeRateLimit,
		Message:    message,
		StatusCode: http.StatusTooManyRequests,
		Provider:   provider,
	}
}

// NewInvalidRequestError creates a new invalid request error
func NewInvalidRequestError(message string, err error) *GatewayError {
	return NewInvalidRequestErrorWithStatus(http.StatusBadRequest, message, err)
}

// NewInvalidRequestErrorWithStatus creates a new invalid request error with a custom status code
func NewInvalidRequestErrorWithStatus(statusCode int, message string, err error) *GatewayError {
	return &GatewayError{
		Type:       ErrorTypeInvalidRequest,
		Message:    message,
		StatusCode: statusCode,
		Err:        err,
	}
}

// NewAuthenticationError creates a new authentication error
func NewAuthenticationError(provider string, message string) *GatewayError {
	return &GatewayError{
		Type:       ErrorTypeAuthentication,
		Message:    message,
		StatusCode: http.StatusUnauthorized,
		Provider:   provider,
	}
}

// NewNotFoundError creates a new not found error
func NewNotFoundError(message string) *GatewayError {
	return &GatewayError{
		Type:       ErrorTypeNotFound,
		Message:    message,
		StatusCode: http.StatusNotFound,
	}
}

// ParseProviderError converts a raw HTTP error response from an upstream provider into a GatewayError
func ParseProviderError(provider string, statusCode int, body []byte, originalErr error) *GatewayError {
	message := string(body)
	if len(body) > maxErrorBodySize {
		message = string(body[:maxErrorBodySize]) + "... (truncated)"
	}
	errorResponse := parseProviderErrorBody(body)
	if errorResponse.Message != "" {
		if len(errorResponse.Message) > maxErrorBodySize {
			errorResponse.Message = errorResponse.Message[:maxErrorBodySize] + "... (truncated)"
		}
		message = errorResponse.Message
	}

	var gatewayErr *GatewayError
	switch {
	case statusCode == http.StatusUnauthorized:
		gatewayErr = &GatewayError{
			Type:       ErrorTypeAuthentication,
			Message:    message,
			StatusCode: http.StatusUnauthorized,
			Provider:   provider,
			Err:        originalErr,
		}
	case statusCode == http.StatusForbidden:
		gatewayErr = &GatewayError{
			Type:       ErrorTypeAuthentication,
			Message:    message,
			StatusCode: http.StatusForbidden,
			Provider:   provider,
			Err:        originalErr,
		}
	case statusCode == http.StatusTooManyRequests:
		gatewayErr = &GatewayError{
			Type:       ErrorTypeRateLimit,
			Message:    message,
			StatusCode: http.StatusTooManyRequests,
			Provider:   provider,
			Err:        originalErr,
		}
	case statusCode == http.StatusNotFound:
		gatewayErr = NewNotFoundError(message)
		gatewayErr.Provider = provider
		gatewayErr.Err = originalErr
	case statusCode >= 400 && statusCode < 500:
		gatewayErr = NewInvalidRequestErrorWithStatus(statusCode, message, originalErr)
		gatewayErr.Provider = provider
	case statusCode >= 500:
		gatewayErr = NewProviderError(provider, statusCode, message, originalErr)
	default:
		gatewayErr = NewProviderError(provider, http.StatusBadGateway, message, originalErr)
	}

	if errorResponse.Param != "" {
		gatewayErr = gatewayErr.WithParam(errorResponse.Param)
	}
	if errorResponse.Code != "" {
		gatewayErr = gatewayErr.WithCode(errorResponse.Code)
	}
	return gatewayErr
}

// providerErrorDetails holds parsed error fields
type providerErrorDetails struct {
	Message string
	Param   string
	Code    string
}

// parseProviderErrorBody extracts error details from a provider response body
func parseProviderErrorBody(body []byte) providerErrorDetails {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Error) == 0 {
		return providerErrorDetails{}
	}
	if message := jsonString(payload.Error); message != "" {
		return providerErrorDetails{Message: message}
	}
	var errorFields map[string]json.RawMessage
	if err := json.Unmarshal(payload.Error, &errorFields); err != nil {
		return providerErrorDetails{}
	}
	details := providerErrorDetails{
		Message: jsonString(errorFields["message"]),
		Param:   jsonString(errorFields["param"]),
		Code:    jsonScalarString(errorFields["code"]),
	}
	if raw := providerErrorMetadataRaw(errorFields["metadata"]); shouldPreferProviderRaw(details.Message, raw) {
		details.Message = raw
	}
	return details
}

// shouldPreferProviderRaw handles OpenRouter wrapper errors: OpenRouter can
// return a generic "Provider returned ..." message while placing the useful
// upstream provider detail in metadata.raw.
func shouldPreferProviderRaw(message, raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	normalizedMessage := strings.ToLower(strings.TrimSpace(message))
	if normalizedMessage == "" || strings.HasPrefix(normalizedMessage, "provider returned") {
		return true
	}
	return false
}

// providerErrorMetadataRaw extracts the raw metadata field from an OpenRouter-wrapped error
func providerErrorMetadataRaw(raw json.RawMessage) string {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return ""
	}
	return jsonString(metadata["raw"])
}

// jsonString extracts a string value from JSON raw bytes
func jsonString(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// jsonScalarString extracts a string or number value from JSON raw bytes
func jsonScalarString(raw json.RawMessage) string {
	if v := jsonString(raw); v != "" {
		return v
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return ""
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return ""
	}
	return number.String()
}

// networkErrorFinishReasons are the finish_reason spellings upstreams use to
// signal that a generation terminated because of a network failure rather
// than completing (observed: "network_error", "network-error", "network
// error"). Matching is case-insensitive on the trimmed value.
var networkErrorFinishReasons = []string{
	"network_error",
	"network-error",
	"network error",
}

// isNetworkErrorFinishReason reports whether reason is a network-failure
// finish_reason variant.
func isNetworkErrorFinishReason(reason string) bool {
	normalized := strings.ToLower(strings.TrimSpace(reason))
	for _, variant := range networkErrorFinishReasons {
		if normalized == variant {
			return true
		}
	}
	return false
}

// terminalNetworkErrorReason returns the first network-failure finish_reason
// found in a completion body — OpenAI-shaped (choices[].finish_reason) or
// Anthropic-shaped (top-level stop_reason) — or "" when the body carries no
// such terminal. The policy is all-or-nothing: one network-failure choice
// voids the entire multi-choice completion, which is then treated as a failed
// upstream attempt instead of being relayed as a successful response.
func terminalNetworkErrorReason(responseMap map[string]interface{}) string {
	if reason, ok := responseMap["stop_reason"].(string); ok && isNetworkErrorFinishReason(reason) {
		return reason
	}
	choicesRaw, ok := responseMap["choices"].([]interface{})
	if !ok {
		return ""
	}
	for _, choiceRaw := range choicesRaw {
		choice, ok := choiceRaw.(map[string]interface{})
		if !ok {
			continue
		}
		reason, _ := choice["finish_reason"].(string)
		if isNetworkErrorFinishReason(reason) {
			return reason
		}
	}
	return ""
}

// capturedStreamHasNetworkErrorFinish reports whether a captured transformed
// SSE buffer contains a network-failure finish_reason or stop_reason. The
// buffer is lowercased first so case variants cannot defeat the match.
// Conservative substring matching (same style as the refusal check): false
// positives (user content echoing the literal JSON) cost only a cache miss.
func capturedStreamHasNetworkErrorFinish(captured []byte) bool {
	lower := bytes.ToLower(captured)
	for _, needle := range []string{
		`"finish_reason":"network_error"`,
		`"finish_reason": "network_error"`,
		`"finish_reason":"network-error"`,
		`"finish_reason": "network-error"`,
		`"finish_reason":"network error"`,
		`"finish_reason": "network error"`,
		`"stop_reason":"network_error"`,
		`"stop_reason": "network_error"`,
		`"stop_reason":"network-error"`,
		`"stop_reason": "network-error"`,
	} {
		if bytes.Contains(lower, []byte(needle)) {
			return true
		}
	}
	return false
}

// embeddedProviderError reports whether an HTTP-200 completion body is
// actually an error object — a top-level "error" key carrying a non-empty
// message, either an Ollama-style string or an OpenAI/Anthropic-style object.
// Returns the parsed details and true when the body is error-shaped. A
// missing, null, or empty "error" field is not an error (some providers emit
// "error": null on success).
func embeddedProviderError(body map[string]interface{}) (providerErrorDetails, bool) {
	if body == nil {
		return providerErrorDetails{}, false
	}
	raw, ok := body["error"]
	if !ok || raw == nil {
		return providerErrorDetails{}, false
	}
	switch errVal := raw.(type) {
	case string:
		if strings.TrimSpace(errVal) == "" {
			return providerErrorDetails{}, false
		}
		return providerErrorDetails{Message: errVal}, true
	case map[string]interface{}:
		message, _ := errVal["message"].(string)
		if strings.TrimSpace(message) == "" {
			// Use the raw JSON value as the message so structural errors
			// still surface instead of being silently relayed.
			if len(errVal) == 0 {
				return providerErrorDetails{}, false
			}
			if code, _ := errVal["code"].(string); code != "" {
				return providerErrorDetails{Message: code, Code: code}, true
			}
			return providerErrorDetails{Message: fmt.Sprintf("%v", errVal)}, true
		}
		details := providerErrorDetails{Message: message}
		if code, _ := errVal["code"].(string); code != "" {
			details.Code = code
		}
		return details, true
	case []interface{}:
		if len(errVal) == 0 {
			return providerErrorDetails{}, false
		}
		return providerErrorDetails{Message: fmt.Sprintf("%v", errVal)}, true
	default:
		// Numbers/booleans/arrays in the error field: non-standard, but
		// clearly not a completion — surface them rather than relaying.
		return providerErrorDetails{Message: fmt.Sprintf("%v", raw)}, true
	}
}

// capturedStreamEndsWithErrorObject reports whether a captured transformed
// SSE buffer ENDS on an error frame: the last non-empty line must be a
// `data:` line carrying a JSON payload classified by
// stream.IsStreamErrorPayload (top-level "error" key, "type":"error", or a
// flat "type":"*_error"). A [DONE] terminator means the upstream considers
// the stream a completion. A mid-stream error object superseded by later
// completion events does not match — only a stream whose terminal event is
// the error counts, since that is what the client actually received.
// Limitation: genuinely multi-line `data:` frames (rare) fragment the
// terminal-line parse and are not classified.
func capturedStreamEndsWithErrorObject(captured []byte) bool {
	trimmed := bytes.TrimSpace(captured)
	if len(trimmed) == 0 {
		return false
	}
	// Last non-empty line only (no slice allocations).
	idx := bytes.LastIndexByte(trimmed, '\n')
	line := bytes.TrimSpace(trimmed[idx+1:])
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false
	}
	if bytes.Equal(bytes.TrimSpace(line), []byte("data: [DONE]")) {
		return false
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
	if len(payload) == 0 || payload[0] != '{' {
		return false
	}
	var parsed map[string]any
	if json.Unmarshal(payload, &parsed) != nil {
		return false
	}
	return stream.IsStreamErrorPayload(parsed)
}

// writeGatewayError writes an OpenAI-compatible JSON error response to the
// client with the appropriate Content-Type header and the structured
// error_kind field (§6 contract: all error responses carry error_kind).
func writeGatewayError(w http.ResponseWriter, statusCode int, errType ErrorType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(OpenAIErrorEnvelope{
		Error: OpenAIErrorObject{
			Type:      errType,
			Message:   message,
			ErrorKind: mapErrorKind(errType),
		},
	})
}

// mapErrorKind converts ErrorType to ErrorKind for error_kind field.
func mapErrorKind(errType ErrorType) infra.ErrorKind {
	switch errType {
	case ErrorTypeAuthentication:
		return infra.ErrorKindAuthFailed
	case ErrorTypeRateLimit:
		return infra.ErrorKindRateLimited
	case ErrorTypeQuotaExhausted:
		return infra.ErrorKindQuotaExhausted
	case ErrorTypeProvider:
		return infra.ErrorKindProviderError
	case ErrorTypeGateway:
		return infra.ErrorKindNetworkError
	case ErrorTypeBouncer:
		return infra.ErrorKindBouncerError
	case ErrorTypeNotFound:
		return infra.ErrorKindModelNotFound
	case ErrorTypeInvalidRequest:
		return infra.ErrorKindInvalidRequest
	default:
		return infra.ErrorKindInternal
	}
}

// writeGatewayStreamError writes an OpenAI-compatible error as an SSE event
// followed by [DONE]. Use this for streaming requests that fail before headers
// are written, so the client receives a properly terminated SSE stream.
func writeGatewayStreamError(w http.ResponseWriter, statusCode int, errType ErrorType, message string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(statusCode)
	errPayload := map[string]any{
		"error": map[string]any{
			"message":    message,
			"type":       string(errType),
			"error_kind": string(mapErrorKind(errType)),
		},
	}
	errBytes, _ := json.Marshal(errPayload)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", errBytes)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
