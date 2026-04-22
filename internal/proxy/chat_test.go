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
	"nenya/internal/testutil"
)

func newChatProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = 60
	cfg.Governance.RatelimitMaxTPM = 100000
	cfg.SecurityFilter.Enabled = false
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider": {
			URL:           upstreamURL + "/v1/chat/completions",
			RoutePrefixes: []string{"test-"},
			AuthStyle:     "none",
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{},
	}
	gw := gateway.New(*cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func TestHandleChatCompletions_ValidUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := testutil.NewTestResponseRecorder()

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
	rec := testutil.NewTestResponseRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_EmptyModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"model":"","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := testutil.NewTestResponseRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_ModelTooLong(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	longModel := strings.Repeat("a", MaxModelNameLength+1)
	payload := fmt.Sprintf(`{"model":"%s","messages":[{"role":"user","content":"hi"}]}`, longModel)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", payload)
	rec := testutil.NewTestResponseRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_InvalidJSON(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{invalid json}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := testutil.NewTestResponseRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
}

func TestHandleChatCompletions_UnknownModel(t *testing.T) {
	p := newChatProxy(t, "http://127.0.0.1:1")
	body := `{"model":"unknown-model-xyz","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := testutil.NewTestResponseRecorder()

	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusBadRequest)
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
			RatelimitMaxRPM: 60,
			RatelimitMaxTPM: 100000,
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

func TestBuildUpstreamRequest_SetsContentType(t *testing.T) {
	var gotCT string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newChatProxy(t, upstream.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer test-token")
	p.ServeHTTP(httptest.NewRecorder(), req)

	if gotCT != "application/json" {
		t.Fatalf("expected Content-Type=application/json, got %q", gotCT)
	}
}
