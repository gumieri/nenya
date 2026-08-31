package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       []byte
		want       infra.ErrorKind
	}{
		{"rate limited", http.StatusTooManyRequests, nil, infra.ErrorKindRateLimited},
		{"unauthorized", http.StatusUnauthorized, nil, infra.ErrorKindAuthFailed},
		{"forbidden", http.StatusForbidden, nil, infra.ErrorKindAuthFailed},
		{"not found", http.StatusNotFound, nil, infra.ErrorKindModelNotFound},
		{"payload too large", http.StatusRequestEntityTooLarge, nil, infra.ErrorKindPayloadTooLarge},
		{"bad request no body", http.StatusBadRequest, nil, infra.ErrorKindInvalidRequest},
		{"bad request context exceeded", http.StatusBadRequest, bodyWithMessage("context_length_exceeded"), infra.ErrorKindContextExceeded},
		{"bad request rate limit", http.StatusBadRequest, bodyWithMessage("rate_limit_exceeded"), infra.ErrorKindRateLimited},
		{"bad request timeout", http.StatusBadRequest, bodyWithMessage("request timed out"), infra.ErrorKindProviderTimeout},
		{"500 server error", 500, nil, infra.ErrorKindProviderError},
		{"502 server error", 502, nil, infra.ErrorKindProviderError},
		{"503 timeout", 503, []byte("timeout"), infra.ErrorKindProviderTimeout},
		{"network error", 0, nil, infra.ErrorKindNetworkError},
		{"unknown status", 418, nil, infra.ErrorKindInvalidRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyError(tt.statusCode, tt.body)
			if got != tt.want {
				t.Errorf("classifyError(%d, %q) = %q, want %q", tt.statusCode, string(tt.body), got, tt.want)
			}
		})
	}
}

func bodyWithMessage(msg string) []byte {
	data, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
		},
	})
	return data
}

func TestInferErrorKind_MalformedBody(t *testing.T) {
	got := inferErrorKind([]byte(`{invalid json`))
	if got != infra.ErrorKindInvalidRequest {
		t.Errorf("got %q, want %q", got, infra.ErrorKindInvalidRequest)
	}
}

func TestInferErrorKind_EmptyBody(t *testing.T) {
	got := inferErrorKind(nil)
	if got != infra.ErrorKindInvalidRequest {
		t.Errorf("got %q, want %q", got, infra.ErrorKindInvalidRequest)
	}
}

func TestClassifyServerError(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want infra.ErrorKind
	}{
		{"generic timeout", []byte("upstream timeout"), infra.ErrorKindProviderTimeout},
		{"generic 500", []byte("internal server error"), infra.ErrorKindProviderError},
		{"empty body", nil, infra.ErrorKindProviderError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyServerError(tt.body)
			if got != tt.want {
				t.Errorf("classifyServerError(%q) = %q, want %q", string(tt.body), got, tt.want)
			}
		})
	}
}

func TestWriteError(t *testing.T) {
	w := newMockResponseWriter()
	writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindProviderError, "upstream failed")

	if w.statusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", w.statusCode, http.StatusBadGateway)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	var resp infra.ErrorResponse
	if err := json.Unmarshal(w.body, &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Kind != infra.ErrorKindProviderError {
		t.Errorf("Kind = %q, want %q", resp.Kind, infra.ErrorKindProviderError)
	}
	if resp.Error.Message != "upstream failed" {
		t.Errorf("Message = %q, want %q", resp.Error.Message, "upstream failed")
	}
}

func TestWriteHTTPError(t *testing.T) {
	tests := []struct {
		name     string
		herr     *httpError
		wantCode int
		wantKind infra.ErrorKind
	}{
		{
			"explicit kind passes through",
			&httpError{Code: http.StatusForbidden, Message: "denied", Kind: infra.ErrorKindAuthFailed},
			http.StatusForbidden, infra.ErrorKindAuthFailed,
		},
		{
			"empty kind falls back to invalid_request",
			&httpError{Code: http.StatusBadRequest, Message: "bad input"},
			http.StatusBadRequest, infra.ErrorKindInvalidRequest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newMockResponseWriter()
			writeHTTPError(w, tt.herr)
			if w.statusCode != tt.wantCode {
				t.Errorf("status = %d, want %d", w.statusCode, tt.wantCode)
			}
			var resp infra.ErrorResponse
			if err := json.Unmarshal(w.body, &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			if resp.Kind != tt.wantKind {
				t.Errorf("Kind = %q, want %q", resp.Kind, tt.wantKind)
			}
			if resp.Error.Message != tt.herr.Message {
				t.Errorf("Message = %q, want %q", resp.Error.Message, tt.herr.Message)
			}
		})
	}
}

func TestWriteHTTPError_NilHerr(t *testing.T) {
	w := newMockResponseWriter()
	writeHTTPError(w, nil)
	if w.statusCode != 0 || len(w.body) != 0 {
		t.Errorf("nil herr must write nothing, got status=%d body=%q", w.statusCode, w.body)
	}
}

func TestHandleResponses_RBACAgentDenial(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger: testLog(t),
		Config: config.Config{Server: config.ServerConfig{MaxBodyBytes: 1 << 20}},
	}
	p := &Proxy{}
	apiKey := &config.ApiKey{
		Name:          "restricted",
		Roles:         []string{"user"},
		AllowedAgents: []string{"other-agent"},
		Enabled:       true,
	}

	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"scoped-agent","input":"hi"}`))
	w := httptest.NewRecorder()
	p.handleResponses(gw, w, r, apiKey)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%q", w.Code, w.Body.String())
	}
	var resp infra.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Kind != infra.ErrorKindAuthFailed {
		t.Fatalf("error_kind = %q, want %q", resp.Kind, infra.ErrorKindAuthFailed)
	}
}

func TestContainsAny(t *testing.T) {
	if !containsAny("hello world", "world") {
		t.Error("expected true for 'world' in 'hello world'")
	}
	if containsAny("hello world", "foo") {
		t.Error("expected false for 'foo' in 'hello world'")
	}
	if !containsAny("a b c", "a", "b") {
		t.Error("expected true for 'a' or 'b' in 'a b c'")
	}
	if containsAny("", "a") {
		t.Error("expected false for empty string")
	}
}

// mockResponseWriter implements http.ResponseWriter for testing.
type mockResponseWriter struct {
	statusCode int
	header     http.Header
	body       []byte
}

func newMockResponseWriter() *mockResponseWriter {
	return &mockResponseWriter{header: make(http.Header)}
}

func (m *mockResponseWriter) Header() http.Header { return m.header }
func (m *mockResponseWriter) Write(b []byte) (int, error) {
	m.body = append(m.body, b...)
	return len(b), nil
}
func (m *mockResponseWriter) WriteHeader(code int) { m.statusCode = code }
