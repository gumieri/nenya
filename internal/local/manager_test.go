package local

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/nenya/config"
)

func TestEngineManager_Startup(t *testing.T) {
	callCount := 0
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 10,
		MaxSessions:    3,
		StartupModels:  []string{"model1", "model2"},
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	if err := em.Startup(ctx); err != nil {
		t.Fatalf("Startup failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if callCount != 2 {
		t.Errorf("expected 2 API calls, got %d", callCount)
	}

	if !em.IsLoaded("model1") || !em.IsLoaded("model2") {
		t.Error("startup models should be loaded")
	}
}

func TestEngineManager_StartupPartialFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("model") == "fail" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 10,
		MaxSessions:    3,
		StartupModels:  []string{"success", "fail"},
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	if err := em.Startup(ctx); err != nil {
		t.Fatalf("Startup should not fail on partial errors, got: %v", err)
	}

	if !em.IsLoaded("success") {
		t.Error("successful model should be loaded")
	}
}

func TestEngineManager_EvictLRU(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}

		if keepAlive, ok := payload["keep_alive"].(float64); ok && keepAlive == 0 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 10,
		MaxSessions:    2,
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	if err := em.LoadModel(ctx, "model1", LoadOptions{}); err != nil {
		t.Fatalf("LoadModel model1 failed: %v", err)
	}
	if err := em.LoadModel(ctx, "model2", LoadOptions{}); err != nil {
		t.Fatalf("LoadModel model2 failed: %v", err)
	}

	if !em.IsLoaded("model1") || !em.IsLoaded("model2") {
		t.Fatal("both models should be loaded")
	}

	if err := em.LoadModel(ctx, "model3", LoadOptions{}); err != nil {
		t.Fatalf("LoadModel model3 failed: %v", err)
	}

	if em.IsLoaded("model1") {
		t.Error("model1 should be evicted (oldest)")
	}
	if !em.IsLoaded("model2") {
		t.Error("model2 should still be loaded")
	}
	if !em.IsLoaded("model3") {
		t.Error("model3 should be loaded")
	}
}

func TestEngineManager_EvictLRUFails(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		// For the first 2 loads, respond with success
		if callCount <= 2 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
			return
		}
		// For the 3rd load, respond with success for the load
		if callCount == 3 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 1,
		MaxSessions:    2,
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	if err := em.LoadModel(ctx, "model1", LoadOptions{}); err != nil {
		t.Fatalf("LoadModel model1 failed: %v", err)
	}
	if err := em.LoadModel(ctx, "model2", LoadOptions{}); err != nil {
		t.Fatalf("LoadModel model2 failed: %v", err)
	}

	if err := em.LoadModel(ctx, "model3", LoadOptions{}); err == nil {
		t.Error("LoadModel should fail when eviction fails")
	}
}

func TestEngineManager_LoadModelAutoLoadDisabled(t *testing.T) {
	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        "http://localhost:11434",
		TimeoutSeconds: 10,
		MaxSessions:    3,
		AutoLoad:       false,
	}
	em := NewEngineManager(cfg, logger)

	if em.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should not be loaded initially")
	}
}

func TestEngineManager_Shutdown_UnloadsAllModels(t *testing.T) {
	var mu sync.Mutex
	var unloadKeepAlives []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req generateRequest
		body, err := io.ReadAll(r.Body)
		if err == nil {
			_ = json.Unmarshal(body, &req)
		}
		mu.Lock()
		unloadKeepAlives = append(unloadKeepAlives, req.KeepAlive)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 10,
		MaxSessions:    3,
		StartupModels:  []string{"model1", "model2"},
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	if err := em.Startup(ctx); err != nil {
		t.Fatalf("Startup failed: %v", err)
	}

	em.Shutdown(ctx)

	if em.IsLoaded("model1") || em.IsLoaded("model2") {
		t.Error("models should be unloaded after Shutdown")
	}
	mu.Lock()
	defer mu.Unlock()
	// 2 load requests (keep_alive=-1 by LoadOptions default) + 2 unload
	// requests; every unload must carry keep_alive=0 so Ollama evicts
	// the pinned models.
	if len(unloadKeepAlives) != 4 {
		t.Fatalf("expected 4 total generate calls (2 load + 2 unload), got %d", len(unloadKeepAlives))
	}
	for i, ka := range unloadKeepAlives[2:] {
		if ka != 0 {
			t.Errorf("unload request %d: expected keep_alive=0, got %d", i, ka)
		}
	}
}

func TestEngineManager_Shutdown_NoModelsNoop(t *testing.T) {
	em := NewEngineManager(&config.LocalEngineConfig{
		BaseURL: "http://127.0.0.1:1", // nothing loaded: no HTTP call expected
	}, slog.Default())
	em.Shutdown(context.Background()) // must not panic or block
}
