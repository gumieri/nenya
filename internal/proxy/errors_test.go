package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
)

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

func TestValidateChatRequest_ClientCanceled(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger: testLog(t),
		Config: config.Config{Server: config.ServerConfig{MaxBodyBytes: 1 << 20}},
	}
	p := &Proxy{}

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	ctx, cancel := context.WithCancel(r.Context())
	r = r.WithContext(ctx)
	cancel()

	w := httptest.NewRecorder()
	_, herr := p.validateChatRequest(w, r, gw, nil)
	if herr == nil {
		t.Fatal("expected httpError, got nil")
	}
	if herr.Code != statusClientClosed {
		t.Fatalf("expected 499 sentinel, got %d", herr.Code)
	}
	// The sentinel must not have rendered a body: the client is gone.
	if w.Body.Len() != 0 {
		t.Fatalf("expected no rendered body, got %q", w.Body.String())
	}
}

func TestReadRequestBody_BranchClassification(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: testLog(t)}

	t.Run("canceled context yields 499 sentinel", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", iotest.ErrReader(errors.New("boom")))
		ctx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(ctx)
		cancel()
		_, herr := readRequestBody(r, gw, "test read")
		if herr == nil || herr.Code != statusClientClosed {
			t.Fatalf("expected 499 sentinel, got %+v", herr)
		}
	})

	t.Run("transport error yields 400 invalid_request", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", iotest.ErrReader(errors.New("boom")))
		_, herr := readRequestBody(r, gw, "test read")
		if herr == nil || herr.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %+v", herr)
		}
		if herr.Kind != infra.ErrorKindInvalidRequest {
			t.Fatalf("error_kind = %q, want invalid_request", herr.Kind)
		}
	})

	t.Run("max bytes error yields 413 payload_too_large", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("data"))
		r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 2)
		_, herr := readRequestBody(r, gw, "test read")
		if herr == nil || herr.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %+v", herr)
		}
		if herr.Kind != infra.ErrorKindPayloadTooLarge {
			t.Fatalf("error_kind = %q, want payload_too_large", herr.Kind)
		}
	})
}

func TestResolveModelRouting_UnknownModel(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:    testLog(t),
		Config:    config.Config{},
		Providers: map[string]*config.Provider{},
	}
	p := &Proxy{}

	req := &chatRequest{ModelName: "no-such-model-anywhere"}
	_, _, _, _, herr := p.resolveModelRouting(context.Background(), req, gw)
	if herr == nil {
		t.Fatal("expected httpError, got nil")
	}
	if herr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", herr.Code)
	}
	if herr.Kind != infra.ErrorKindModelNotFound {
		t.Fatalf("error_kind = %q, want %q", herr.Kind, infra.ErrorKindModelNotFound)
	}
}

func TestResolveModelRouting_NonChatModel(t *testing.T) {
	providers := config.ResolveProviders(
		&config.Config{Providers: map[string]config.ProviderConfig{
			"zen": {
				URL:           "https://opencode.ai/zen/v1/chat/completions",
				AuthStyle:     "bearer",
				NonChatModels: []string{`^jev-`},
			},
		}},
		&config.SecretsConfig{ProviderKeys: map[string]string{"zen": "test-key"}},
	)
	gw := &gateway.NenyaGateway{
		Logger:       testLog(t),
		Config:       config.Config{},
		Providers:    providers,
		ModelCatalog: discovery.NewModelCatalog(),
	}
	p := &Proxy{}

	req := &chatRequest{ModelName: "jev-1.13-free"}
	_, _, _, _, herr := p.resolveModelRouting(context.Background(), req, gw)
	if herr == nil {
		t.Fatal("expected httpError for a non-chat model, got nil")
	}
	if herr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", herr.Code)
	}
	if herr.Kind != infra.ErrorKindInvalidRequest {
		t.Fatalf("error_kind = %q, want %q", herr.Kind, infra.ErrorKindInvalidRequest)
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

func TestWritePipelineRejection_StrictError(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: testLog(t), Metrics: infra.NewMetrics()}
	w := httptest.NewRecorder()

	aborted := writePipelineRejection(gw, w, &pipeline.StrictError{
		Err:         errors.New("boom"),
		Kind:        infra.ErrorKindInternal,
		Interceptor: "injection",
		Message:     "request aborted: security preprocessing failed",
	})
	if !aborted {
		t.Fatal("writePipelineRejection must report StrictError as handled")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%q", w.Code, w.Body.String())
	}
	var resp infra.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Kind != infra.ErrorKindInternal {
		t.Fatalf("error_kind = %q, want %q", resp.Kind, infra.ErrorKindInternal)
	}
}

func TestWritePipelineRejection_OperationalErrorFailsOpen(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: testLog(t), Metrics: infra.NewMetrics()}
	w := httptest.NewRecorder()

	if writePipelineRejection(gw, w, errors.New("plain operational failure")) {
		t.Fatal("plain errors must not be treated as rejections")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("expected no response written, got %d", w.Code)
	}
}

func TestWritePipelineRejection_RejectError(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: testLog(t), Metrics: infra.NewMetrics()}
	w := httptest.NewRecorder()

	aborted := writePipelineRejection(gw, w, &pipeline.RejectError{
		Err:  errors.New("policy"),
		Kind: infra.ErrorKindInjection,
	})
	if !aborted {
		t.Fatal("writePipelineRejection must report RejectError as handled")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}

	// Empty Kind is defensively defaulted to internal_error.
	w2 := httptest.NewRecorder()
	aborted = writePipelineRejection(gw, w2, &pipeline.RejectError{Err: errors.New("policy")})
	if !aborted || w2.Code != http.StatusForbidden {
		t.Fatalf("handled=%v code=%d, want true/403", aborted, w2.Code)
	}
	var resp infra.ErrorResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}
	if resp.Kind != infra.ErrorKindInternal {
		t.Fatalf("error_kind = %q, want default %q", resp.Kind, infra.ErrorKindInternal)
	}
}
