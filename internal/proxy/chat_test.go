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
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/memory"
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

func TestInjectMemoryContext(t *testing.T) {
	t.Run("no agent name", func(t *testing.T) {
		p := newChatProxy(t, "http://127.0.0.1")
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "")
		msgs := payload["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Errorf("expected 1 message (unchanged), got %d", len(msgs))
		}
	})

	t.Run("no memory client", func(t *testing.T) {
		p := newChatProxy(t, "http://127.0.0.1")
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "nonexistent-agent")
		msgs := payload["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Errorf("expected 1 message (unchanged), got %d", len(msgs))
		}
	})

	t.Run("last message not user role", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("mem0 should not be called when last message is not user")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"results": nil})
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "assistant", "content": "previous response"},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "mem-agent")
		msgs := payload["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Errorf("expected 1 message (unchanged), got %d", len(msgs))
		}
	})

	t.Run("empty last message content", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("mem0 should not be called for empty content")
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"results": nil})
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": ""},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "mem-agent")
		msgs := payload["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Errorf("expected 1 message (unchanged), got %d", len(msgs))
		}
	})

	t.Run("memories injected before last user message", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"results": []map[string]interface{}{
					{"id": "m1", "memory": "User likes Go", "score": 0.9},
				},
			})
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": "you are helpful"},
				map[string]interface{}{"role": "user", "content": "what should I use?"},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "mem-agent")

		msgs := payload["messages"].([]interface{})
		if len(msgs) != 3 {
			t.Fatalf("expected 3 messages (system + memory + user), got %d", len(msgs))
		}

		first := msgs[0].(map[string]interface{})
		if first["role"] != "system" || first["content"] != "you are helpful" {
			t.Errorf("first message should be original system, got: %+v", first)
		}

		injected := msgs[1].(map[string]interface{})
		if injected["role"] != "system" {
			t.Errorf("injected message should be system role, got: %+v", injected)
		}
		injectedContent, _ := injected["content"].(string)
		if !strings.Contains(injectedContent, "User likes Go") {
			t.Errorf("injected message should contain memory, got: %s", injectedContent)
		}

		last := msgs[2].(map[string]interface{})
		if last["role"] != "user" || last["content"] != "what should I use?" {
			t.Errorf("last message should be original user message, got: %+v", last)
		}
	})

	t.Run("no memories found", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}})
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "mem-agent")
		msgs := payload["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Errorf("expected 1 message (unchanged when no results), got %d", len(msgs))
		}
	})

	t.Run("mem0 search failure does not block", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("mem0 down"))
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}
		payload := map[string]interface{}{
			"messages": []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
			},
		}
		messages := payload["messages"].([]interface{})
		p.injectMemoryContext(context.Background(), payload, messages, "mem-agent")
		msgs := payload["messages"].([]interface{})
		if len(msgs) != 1 {
			t.Errorf("expected 1 message (unchanged on error), got %d", len(msgs))
		}
	})
}

func TestAsyncStoreMemory(t *testing.T) {
	t.Run("stores assistant content", func(t *testing.T) {
		var receivedBody addRequestBody
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&receivedBody)
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}})
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}

		p.asyncStoreMemory("mem-agent", "Hello from assistant")
		time.Sleep(200 * time.Millisecond)

		if len(receivedBody.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(receivedBody.Messages))
		}
		if receivedBody.Messages[0].Role != "assistant" {
			t.Errorf("expected role 'assistant', got %q", receivedBody.Messages[0].Role)
		}
		if receivedBody.Messages[0].Content != "Hello from assistant" {
			t.Errorf("expected content 'Hello from assistant', got %q", receivedBody.Messages[0].Content)
		}
		if receivedBody.UserID != "u1" {
			t.Errorf("expected user_id 'u1', got %q", receivedBody.UserID)
		}
	})

	t.Run("no-op for unknown agent", func(t *testing.T) {
		p := newChatProxy(t, "http://127.0.0.1")
		p.asyncStoreMemory("nonexistent", "content")
	})

	t.Run("no-op for empty content", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("mem0 should not be called for empty content")
			w.WriteHeader(http.StatusOK)
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}
		p.asyncStoreMemory("mem-agent", "")
	})

	t.Run("error is logged not panicked", func(t *testing.T) {
		memSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte("mem0 error"))
		}))
		defer memSrv.Close()

		p := newChatProxy(t, "http://127.0.0.1")
		p.GW.MemoryClients = map[string]*memory.Mem0Client{
			"mem-agent": memory.NewMem0Client(memory.MemoryConfig{URL: memSrv.URL, UserID: "u1"}, nil),
		}

		done := make(chan struct{})
		go func() {
			p.asyncStoreMemory("mem-agent", "some content")
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("asyncStoreMemory goroutine did not complete within timeout")
		}
	})
}

type addRequestBody struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	UserID   string `json:"user_id,omitempty"`
}
