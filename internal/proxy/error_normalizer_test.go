package proxy

import (
	"encoding/json"
	"net/http"
	"testing"
)

type testCase struct {
	name        string
	provider    string
	statusCode  int
	body        []byte
	originalErr error
	wantType    ErrorType
	wantMessage string
	wantCode    string
	wantParam   string
	wantStatus  int
}

func TestParseProviderError(t *testing.T) {
	tests := []testCase{
		{
			name:        "401 unauthorized",
			provider:    "openai",
			statusCode:  http.StatusUnauthorized,
			body:        []byte(`{"error":{"message":"Invalid API key"}}`),
			wantType:    ErrorTypeAuthentication,
			wantMessage: "Invalid API key",
			wantStatus:  http.StatusUnauthorized,
		},
		{
			name:        "403 forbidden",
			provider:    "anthropic",
			statusCode:  http.StatusForbidden,
			body:        []byte(`{"error":{"message":"Forbidden"}}`),
			wantType:    ErrorTypeAuthentication,
			wantMessage: "Forbidden",
			wantStatus:  http.StatusForbidden,
		},
		{
			name:        "429 rate limit",
			provider:    "openrouter",
			statusCode:  http.StatusTooManyRequests,
			body:        []byte(`{"error":{"message":"Rate limit exceeded"}}`),
			wantType:    ErrorTypeRateLimit,
			wantMessage: "Rate limit exceeded",
			wantStatus:  http.StatusTooManyRequests,
		},
		{
			name:        "404 not found",
			provider:    "openai",
			statusCode:  http.StatusNotFound,
			body:        []byte(`{"error":{"message":"Model not found"}}`),
			wantType:    ErrorTypeNotFound,
			wantMessage: "Model not found",
			wantStatus:  http.StatusNotFound,
		},
		{
			name:        "400 invalid request",
			provider:    "anthropic",
			statusCode:  http.StatusBadRequest,
			body:        []byte(`{"error":{"message":"Invalid request","param":"model"}}`),
			wantType:    ErrorTypeInvalidRequest,
			wantMessage: "Invalid request",
			wantParam:   "model",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "500 provider error",
			provider:    "openai",
			statusCode:  http.StatusInternalServerError,
			body:        []byte(`{"error":{"message":"Internal server error"}}`),
			wantType:    ErrorTypeProvider,
			wantMessage: "Internal server error",
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:        "502 bad gateway",
			provider:    "openrouter",
			statusCode:  http.StatusBadGateway,
			body:        []byte(`{"error":{"message":"Bad gateway"}}`),
			wantType:    ErrorTypeProvider,
			wantMessage: "Bad gateway",
			wantStatus:  http.StatusBadGateway,
		},
		{
			name:        "503 service unavailable",
			provider:    "openai",
			statusCode:  http.StatusServiceUnavailable,
			body:        []byte(`{"error":{"message":"Service unavailable"}}`),
			wantType:    ErrorTypeProvider,
			wantMessage: "Service unavailable",
			wantStatus:  http.StatusServiceUnavailable,
		},
		{
			name:        "504 gateway timeout",
			provider:    "anthropic",
			statusCode:  http.StatusGatewayTimeout,
			body:        []byte(`{"error":{"message":"Gateway timeout"}}`),
			wantType:    ErrorTypeProvider,
			wantMessage: "Gateway timeout",
			wantStatus:  http.StatusGatewayTimeout,
		},
		{
			name:        "422 unprocessable entity",
			provider:    "openai",
			statusCode:  http.StatusUnprocessableEntity,
			body:        []byte(`{"error":{"message":"Unprocessable entity"}}`),
			wantType:    ErrorTypeInvalidRequest,
			wantMessage: "Unprocessable entity",
			wantStatus:  http.StatusUnprocessableEntity,
		},
		{
			name:        "413 payload too large",
			provider:    "anthropic",
			statusCode:  http.StatusRequestEntityTooLarge,
			body:        []byte(`{"error":{"message":"Payload too large"}}`),
			wantType:    ErrorTypeInvalidRequest,
			wantMessage: "Payload too large",
			wantStatus:  http.StatusRequestEntityTooLarge,
		},
		{
			name:        "plain string error",
			provider:    "openai",
			statusCode:  http.StatusBadRequest,
			body:        []byte(`{"error":"Plain string error"}`),
			wantType:    ErrorTypeInvalidRequest,
			wantMessage: "Plain string error",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "error with code",
			provider:    "openai",
			statusCode:  http.StatusBadRequest,
			body:        []byte(`{"error":{"message":"Invalid request","code":"invalid_request"}}`),
			wantType:    ErrorTypeInvalidRequest,
			wantMessage: "Invalid request",
			wantCode:    "invalid_request",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "openrouter raw message",
			provider:    "openrouter",
			statusCode:  http.StatusBadRequest,
			body:        []byte(`{"error":{"message":"Provider returned an error","metadata":{"raw":"Actual upstream error"}}}`),
			wantType:    ErrorTypeInvalidRequest,
			wantMessage: "Actual upstream error",
			wantStatus:  http.StatusBadRequest,
		},
		{
			name:        "empty body",
			provider:    "openai",
			statusCode:  http.StatusInternalServerError,
			body:        []byte{},
			wantType:    ErrorTypeProvider,
			wantMessage: "",
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:        "non-JSON body",
			provider:    "openai",
			statusCode:  http.StatusInternalServerError,
			body:        []byte(`plain text error`),
			wantType:    ErrorTypeProvider,
			wantMessage: "plain text error",
			wantStatus:  http.StatusInternalServerError,
		},
		{
			name:        "unknown status code",
			provider:    "openai",
			statusCode:  http.StatusCreated,
			body:        []byte(`{"error":{"message":"Created"}}`),
			wantType:    ErrorTypeProvider,
			wantMessage: "Created",
			wantStatus:  http.StatusBadGateway,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseProviderError(tt.provider, tt.statusCode, tt.body, tt.originalErr)

			if got.Type != tt.wantType {
				t.Errorf("ParseProviderError() Type = %v, want %v", got.Type, tt.wantType)
			}
			if got.Message != tt.wantMessage {
				t.Errorf("ParseProviderError() Message = %v, want %v", got.Message, tt.wantMessage)
			}
			if got.HTTPStatusCode() != tt.wantStatus {
				t.Errorf("ParseProviderError() HTTPStatusCode = %v, want %v", got.HTTPStatusCode(), tt.wantStatus)
			}
			if got.Provider != tt.provider {
				t.Errorf("ParseProviderError() Provider = %v, want %v", got.Provider, tt.provider)
			}
			if tt.wantCode != "" && (got.Code == nil || *got.Code != tt.wantCode) {
				t.Errorf("ParseProviderError() Code = %v, want %v", got.Code, tt.wantCode)
			}
			if tt.wantParam != "" && (got.Param == nil || *got.Param != tt.wantParam) {
				t.Errorf("ParseProviderError() Param = %v, want %v", got.Param, tt.wantParam)
			}
		})
	}
}

func TestGatewayErrorToJSON(t *testing.T) {
	tests := []struct {
		name string
		err  *GatewayError
		want map[string]any
	}{
		{
			name: "basic error",
			err:  NewInvalidRequestError("test error", nil),
			want: map[string]any{
				"error": map[string]any{
					"type":    ErrorTypeInvalidRequest,
					"message": "test error",
					"param":   nil,
					"code":    nil,
				},
			},
		},
		{
			name: "error with param",
			err:  NewInvalidRequestError("test error", nil).WithParam("model"),
			want: map[string]any{
				"error": map[string]any{
					"type":    ErrorTypeInvalidRequest,
					"message": "test error",
					"param":   "model",
					"code":    nil,
				},
			},
		},
		{
			name: "error with code",
			err:  NewInvalidRequestError("test error", nil).WithCode("invalid_request"),
			want: map[string]any{
				"error": map[string]any{
					"type":    ErrorTypeInvalidRequest,
					"message": "test error",
					"param":   nil,
					"code":    "invalid_request",
				},
			},
		},
		{
			name: "provider error",
			err:  NewProviderError("openai", http.StatusInternalServerError, "upstream error", nil),
			want: map[string]any{
				"error": map[string]any{
					"type":    ErrorTypeProvider,
					"message": "upstream error",
					"param":   nil,
					"code":    nil,
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.ToJSON()
			if !jsonEqual(got, tt.want) {
				t.Errorf("GatewayError.ToJSON() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGatewayErrorHTTPStatusCode(t *testing.T) {
	tests := []struct {
		name     string
		err      *GatewayError
		wantCode int
	}{
		{
			name:     "rate limit error",
			err:      NewRateLimitError("openai", "rate limit"),
			wantCode: http.StatusTooManyRequests,
		},
		{
			name:     "invalid request error",
			err:      NewInvalidRequestError("invalid", nil),
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "authentication error",
			err:      NewAuthenticationError("openai", "auth failed"),
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "not found error",
			err:      NewNotFoundError("not found"),
			wantCode: http.StatusNotFound,
		},
		{
			name:     "provider error",
			err:      NewProviderError("openai", http.StatusInternalServerError, "upstream error", nil),
			wantCode: http.StatusInternalServerError,
		},
		{
			name:     "provider error with custom status",
			err:      NewProviderError("openai", http.StatusBadGateway, "bad gateway", nil),
			wantCode: http.StatusBadGateway,
		},
		{
			name:     "unknown error type",
			err:      &GatewayError{Type: ErrorType("unknown"), Message: "unknown"},
			wantCode: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.HTTPStatusCode()
			if got != tt.wantCode {
				t.Errorf("GatewayError.HTTPStatusCode() = %v, want %v", got, tt.wantCode)
			}
		})
	}
}

func TestGatewayErrorError(t *testing.T) {
	tests := []struct {
		name    string
		err     *GatewayError
		wantMsg string
	}{
		{
			name:    "error without provider",
			err:     NewInvalidRequestError("test error", nil),
			wantMsg: "invalid_request_error: test error",
		},
		{
			name:    "error with provider",
			err:     NewProviderError("openai", http.StatusInternalServerError, "upstream error", nil),
			wantMsg: "[openai] provider_error: upstream error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.err.Error()
			if got != tt.wantMsg {
				t.Errorf("GatewayError.Error() = %v, want %v", got, tt.wantMsg)
			}
		})
	}
}

func jsonEqual(a, b map[string]any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func FuzzParseProviderErrorBody(f *testing.F) {
	f.Add([]byte(`{"error":{"message":"test"}}`))
	f.Add([]byte(`{"error":"plain"}`))
	f.Add([]byte(`{"error":{}}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"error":{"message":"nested","param":"value","code":"err"}}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_ = parseProviderErrorBody(body)
	})
}
