package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nenya/internal/config"
	"nenya/internal/gateway"
)

func newChatProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{
			MaxBodyBytes: 10 << 20,
		},
		Governance: config.GovernanceConfig{
			ContextSoftLimit: 100000,
			ContextHardLimit: 200000,
			RatelimitMaxRPM:  60,
			RatelimitMaxTPM:  100000,
		},
		SecurityFilter: config.SecurityFilterConfig{
			Enabled: false,
		},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:           upstreamURL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "none",
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
	}
	gw := gateway.New(cfg, secrets, slog.Default())
	return &Proxy{GW: gw}
}

func TestHandleChatCompletions_ValidUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	respBody, _ := io.ReadAll(rec.Body)
	if !strings.Contains(string(respBody), "hello") {
		t.Errorf("expected response to contain 'hello', got: %s", string(respBody))
	}
}

func TestHandleChatCompletions_MissingModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_EmptyModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"model":"","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_ModelTooLong(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	longModel := strings.Repeat("a", MaxModelNameLength+1)
	payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, longModel)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_InvalidJSON(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{invalid json}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_UnknownModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := strings.NewReader(`{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestHandleChatCompletions_AgentWithModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"agent response\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := config.Config{
		Server: config.ServerConfig{
			MaxBodyBytes: 10 << 20,
		},
		Governance: config.GovernanceConfig{
			ContextSoftLimit: 100000,
			ContextHardLimit: 200000,
			RatelimitMaxRPM:  60,
			RatelimitMaxTPM:  100000,
		},
		SecurityFilter: config.SecurityFilterConfig{
			Enabled: false,
		},
		Providers: map[string]config.ProviderConfig{
			"test-provider": {
				URL:           upstream.URL + "/v1/chat/completions",
				RoutePrefixes: []string{"test-"},
				AuthStyle:     "none",
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
	gw := gateway.New(cfg, secrets, slog.Default())
	p := &Proxy{GW: gw}

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
	p.GW.Config.Agents = map[string]config.AgentConfig{
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
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []map[string]interface{}{{"embedding": []float64{0.1, 0.2}}},
		})
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := strings.NewReader(`{"model":"test-embedding","input":"hello"}`)
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
