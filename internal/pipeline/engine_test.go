package pipeline

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"nenya/config"
)

func noopInject(providerName string, headers http.Header) error {
	return nil
}

func TestCallEngine_OpenAI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		msgs, ok := body["messages"].([]interface{})
		if !ok || len(msgs) < 2 {
			t.Fatal("expected messages array in request")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"message": map[string]interface{}{
						"content": "summary",
					},
				},
			},
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	provider := &config.Provider{
		Name:      "test",
		URL:       srv.URL,
		ApiFormat: "openai",
	}
	engine := config.EngineConfig{
		Provider: "test",
		Model:    "gpt-4",
	}

	result, err := CallEngine(context.Background(), srv.Client(), provider, engine, noopInject, "sys prompt", "user prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "summary" {
		t.Errorf("result = %q, want %q", result, "summary")
	}
}

func TestCallEngine_Ollama(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("failed to decode request: %v", err)
		}

		if _, ok := body["system"]; !ok {
			t.Error("expected 'system' key in ollama request")
		}
		if _, ok := body["prompt"]; !ok {
			t.Error("expected 'prompt' key in ollama request")
		}

		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]interface{}{
			"response": "ollama-summary",
		}); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}))
	defer srv.Close()

	provider := &config.Provider{
		Name:      "ollama-test",
		URL:       srv.URL,
		ApiFormat: "ollama",
	}
	engine := config.EngineConfig{
		Provider: "ollama-test",
		Model:    "qwen2.5-coder",
	}

	result, err := CallEngine(context.Background(), srv.Client(), provider, engine, noopInject, "sys prompt", "user prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "ollama-summary" {
		t.Errorf("result = %q, want %q", result, "ollama-summary")
	}
}

func TestCallEngine_Unreachable(t *testing.T) {
	provider := &config.Provider{
		Name: "bad",
		URL:  "http://127.0.0.1:1",
	}
	engine := config.EngineConfig{
		Provider: "bad",
		Model:    "test",
	}

	_, err := CallEngine(context.Background(), &http.Client{}, provider, engine, noopInject, "sys", "prompt")
	if err == nil {
		t.Fatal("expected error for unreachable engine")
	}
}

func TestCallEngine_Non200Status(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		if _, err := w.Write([]byte("rate limited")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	provider := &config.Provider{
		Name: "rate-limited",
		URL:  srv.URL,
	}
	engine := config.EngineConfig{
		Provider: "rate-limited",
		Model:    "test",
	}

	_, err := CallEngine(context.Background(), srv.Client(), provider, engine, noopInject, "sys", "prompt")
	if err == nil {
		t.Fatal("expected error for non-200 status")
	}
}

func TestCallEngine_InvalidResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte("not-json")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer srv.Close()

	provider := &config.Provider{
		Name: "broken",
		URL:  srv.URL,
	}
	engine := config.EngineConfig{
		Provider: "broken",
		Model:    "test",
	}

	_, err := CallEngine(context.Background(), srv.Client(), provider, engine, noopInject, "sys", "prompt")
	if err == nil {
		t.Fatal("expected error for invalid response body")
	}
}
