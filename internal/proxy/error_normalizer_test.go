package proxy

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/nenya/internal/infra"
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
			if got.StatusCode != tt.wantStatus {
				t.Errorf("ParseProviderError() StatusCode = %v, want %v", got.StatusCode, tt.wantStatus)
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

func TestMapErrorKind(t *testing.T) {
	tests := []struct {
		name     string
		errType  ErrorType
		wantKind string
	}{
		{"authentication maps to auth_failed", ErrorTypeAuthentication, "auth_failed"},
		{"rate_limit maps to rate_limited", ErrorTypeRateLimit, "rate_limited"},
		{"quota_exhausted maps to quota_exhausted", ErrorTypeQuotaExhausted, "quota_exhausted"},
		{"provider maps to provider_error", ErrorTypeProvider, "provider_error"},
		{"not_found maps to model_not_found", ErrorTypeNotFound, "model_not_found"},
		{"invalid_request maps to invalid_request", ErrorTypeInvalidRequest, "invalid_request"},
		{"policy_refusal maps to policy_refusal", ErrorTypePolicyRefusal, "policy_refusal"},
		{"gateway maps to network_error", ErrorTypeGateway, "network_error"},
		{"bouncer maps to bouncer_error", ErrorTypeBouncer, "bouncer_error"},
		{"unknown maps to internal_error", ErrorType("unknown"), "internal_error"},
		{"empty maps to internal_error", ErrorType(""), "internal_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapErrorKind(tt.errType)
			if string(got) != tt.wantKind {
				t.Errorf("mapErrorKind(%v) = %v, want %v", tt.errType, got, tt.wantKind)
			}
		})
	}
}

func TestGatewayErrorWithQuotaExhausted(t *testing.T) {
	w := newMockResponseWriter()
	writeGatewayError(w, http.StatusPaymentRequired, ErrorTypeQuotaExhausted, "Quota exceeded")

	if w.statusCode != http.StatusPaymentRequired {
		t.Errorf("status = %d, want %d", w.statusCode, http.StatusPaymentRequired)
	}
	var got struct {
		Error struct {
			Type      ErrorType `json:"type"`
			Message   string    `json:"message"`
			ErrorKind string    `json:"error_kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.body, &got); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if got.Error.Type != ErrorTypeQuotaExhausted {
		t.Errorf("type = %v, want %v", got.Error.Type, ErrorTypeQuotaExhausted)
	}
	if got.Error.Message != "Quota exceeded" {
		t.Errorf("message = %v, want %v", got.Error.Message, "Quota exceeded")
	}
	if got.Error.ErrorKind != string(infra.ErrorKindQuotaExhausted) {
		t.Errorf("error_kind = %v, want %v", got.Error.ErrorKind, infra.ErrorKindQuotaExhausted)
	}
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
