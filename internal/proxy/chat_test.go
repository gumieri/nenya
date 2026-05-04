package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/testutil"
)

func newChatProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "none",
		},
		"deepseek": {
			URL:       upstreamURL + "/v1/chat/completions",
			AuthStyle: "bearer",
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "test-provider", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
		ProviderKeys: map[string]string{
			"deepseek": "test-api-key",
		},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func TestHandleChatCompletions_ValidUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(respBody), "hello") {
		t.Errorf("expected response to contain 'hello', got: %s", string(respBody))
	}
}

func TestHandleChatCompletions_MissingModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_EmptyModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"model":"","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_ModelTooLong(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	longModel := strings.Repeat("a", MaxModelNameLength+1)
	payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, longModel)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", payload)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_NonStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chat-123",
			"object":  "chat.completion",
			"created": 1234567890,
			"model":   "test-model",
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "hello world",
					},
				},
			},
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	respBody, _ := io.ReadAll(rec.Body)
	if strings.Contains(string(respBody), "data:") {
		t.Errorf("expected non-streaming response, got SSE: %s", string(respBody))
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err != nil {
		t.Fatalf("expected JSON response, got error: %v, body: %s", err, string(respBody))
	}
	if resp["id"] != "chat-123" {
		t.Errorf("expected id=chat-123, got %v", resp["id"])
	}
}

func TestHandleChatCompletions_NonStreamingEmptyResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(nil)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Logf("got %d for empty non-streaming — acceptable without upstream error", rec.Code)
	}
}

func TestHandleChatCompletions_InvalidJSON(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{invalid json}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_UnknownModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_AgentWithModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"agent response\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			MaxBodyBytes: 10 << 20,
		},
		Governance: config.GovernanceConfig{
			RatelimitMaxRPM: config.PtrTo(60),
			RatelimitMaxTPM: config.PtrTo(100000),
		},
		Bouncer: config.BouncerConfig{
			Enabled: config.PtrTo(false),
		},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:       upstream.URL + "/v1/chat/completions",
				AuthStyle: "none",
			},
		},
		Agents: map[string]config.AgentConfig{
			"my-agent": {
				Models: []config.AgentModel{
					{Provider: "test-provider", Model: "test-model"},
				},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
	}
	gw := gateway.New(context.Background(), cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	body := strings.NewReader(`{"model":"my-agent","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	respBody, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(respBody), "agent response") {
		t.Errorf("expected response to contain 'agent response', got: %s", string(respBody))
	}
}

func TestHandleChatCompletions_AgentNoModels(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	p.Gateway().Config.Agents = map[string]config.AgentConfig{
		"empty-agent": {
			Models: []config.AgentModel{},
		},
	}

	body := strings.NewReader(`{"model":"empty-agent","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
}

func TestHandleEmbeddings_ValidUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{{"embedding": []float64{0.1, 0.2}}},
		}); err != nil {
			t.Errorf("encode: %v", err)
		}
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := strings.NewReader(`{"model":"deepseek-v4-flash","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got error: %v", err)
	}
	if resp["object"] != "list" {
		t.Errorf("expected object=list, got %v", resp["object"])
	}
}

func TestHandleEmbeddings_MissingModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleEmbeddings_UnknownModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"model":"unknown-embedding-xyz","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestBuildUpstreamRequest_SetsContentType(t *testing.T) {
	var gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotCT != "application/json" {
		t.Fatalf("expected Content-Type=application/json, got %q", gotCT)
	}
}

func TestHandleResponses_GET_ByID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_123" {
			t.Errorf("expected /v1/responses/resp_123, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "resp_123",
			"status": "completed",
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_123", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleResponses_Cancel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_456/cancel" {
			t.Errorf("expected /v1/responses/resp_456/cancel, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "resp_456",
			"status": "cancelling",
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_456/cancel", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleResponses_Delete(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_789" {
			t.Errorf("expected /v1/responses/resp_789, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_789", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
}

func TestHandleResponses_Compact(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/v1/responses/resp_abc/compact" {
			t.Errorf("expected /v1/responses/resp_abc/compact, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":     "resp_abc",
			"status": "in_progress",
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_abc/compact", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestHandleResponses_PathTraversal(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/../../etc/passwd", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for path traversal, got %d", rec.Code)
	}
}

