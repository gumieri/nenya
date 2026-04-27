package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	maxErrorBodySize = 64 * 1024 // 64KB limit for error bodies
	maxParamLength   = 256       // 256 char limit for param/code fields
)

// ErrorType represents the type of error that occurred
type ErrorType string

const (
	// ErrorTypeProvider indicates an upstream provider error (5xx)
	ErrorTypeProvider ErrorType = "provider_error"
	// ErrorTypeRateLimit indicates a rate limit error (429)
	ErrorTypeRateLimit ErrorType = "rate_limit_error"
	// ErrorTypeInvalidRequest indicates a client error (4xx)
	ErrorTypeInvalidRequest ErrorType = "invalid_request_error"
	// ErrorTypeAuthentication indicates an authentication error (401)
	ErrorTypeAuthentication ErrorType = "authentication_error"
	// ErrorTypeNotFound indicates a not found error (404)
	ErrorTypeNotFound ErrorType = "not_found_error"
)

// GatewayError is the base error type for all gateway errors
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

// OpenAI-compatible envelope for responses
type OpenAIErrorEnvelope struct {
	Error OpenAIErrorObject `json:"error"`
}

// OpenAIErrorObject is the public error object
type OpenAIErrorObject struct {
	Type    ErrorType `json:"type"`
	Message string    `json:"message"`
	Param   *string   `json:"param,omitempty"`
	Code    *string   `json:"code,omitempty"`
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

// ToJSON converts the error to an OpenAI-compatible JSON map
func (e *GatewayError) ToJSON() map[string]any {
	var param any
	if e.Param != nil {
		param = *e.Param
	}
	var code any
	if e.Code != nil {
		code = *e.Code
	}
	return map[string]any{
		"error": map[string]any{
			"type":    e.Type,
			"message": e.Message,
			"param":   param,
			"code":    code,
		},
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
