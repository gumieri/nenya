package local

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/nenya/config"
)

func TestNewSessionManager(t *testing.T) {
	sm := NewSessionManager("http://localhost:11434", 30*time.Second)
	if sm == nil {
		t.Fatal("NewSessionManager returned nil")
	}
	if sm.baseURL != "http://localhost:11434" {
		t.Errorf("baseURL = %q, want %q", sm.baseURL, "http://localhost:11434")
	}
	if sm.timeout != 30*time.Second {
		t.Errorf("timeout = %v, want 30s", sm.timeout)
	}
}

func TestLoadModel_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/generate" {
			t.Errorf("path = %q, want /api/generate", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	session, err := sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadModel failed: %v", err)
	}
	if session == nil {
		t.Fatal("session is nil")
	}
	if session.ModelID != "qwen2.5-coder:7b" {
		t.Errorf("ModelID = %q, want qwen2.5-coder:7b", session.ModelID)
	}

	if !sm.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should be loaded")
	}
}

func TestLoadModel_ReuseExisting(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	_, err := sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
	if err != nil {
		t.Fatalf("first LoadModel failed: %v", err)
	}

	_, err = sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
	if err != nil {
		t.Fatalf("second LoadModel failed: %v", err)
	}

	if callCount != 1 {
		t.Errorf("callCount = %d, want 1 (should reuse existing session)", callCount)
	}
}

func TestUnloadModel_HappyPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/generate" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"done": true, "response": ""}`))
		}
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	_, err := sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadModel failed: %v", err)
	}

	if err := sm.UnloadModel(ctx, "qwen2.5-coder:7b"); err != nil {
		t.Fatalf("UnloadModel failed: %v", err)
	}

	if sm.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should be unloaded")
	}
}

func TestUnloadModel_NotLoaded(t *testing.T) {
	sm := NewSessionManager("http://localhost:11434", 10*time.Second)
	ctx := context.Background()

	if err := sm.UnloadModel(ctx, "qwen2.5-coder:7b"); err != nil {
		t.Fatalf("UnloadModel failed: %v", err)
	}
}

func TestIsLoaded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	if sm.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should not be loaded initially")
	}

	_, err := sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
	if err != nil {
		t.Fatalf("LoadModel failed: %v", err)
	}

	if !sm.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should be loaded")
	}
}

func TestGetLoadedModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	models := sm.GetLoadedModels()
	if len(models) != 0 {
		t.Errorf("initial models = %d, want 0", len(models))
	}

	_, _ = sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
	_, _ = sm.LoadModel(ctx, "llama3.1:8b", LoadOptions{})

	models = sm.GetLoadedModels()
	if len(models) != 2 {
		t.Errorf("models after load = %d, want 2", len(models))
	}
}

func TestListInstalledModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{
				"models": [
					{"name": "qwen2.5-coder:7b", "size": 4678608524, "modified_at": "2024-01-15T10:30:00Z"},
					{"name": "llama3.1:8b", "size": 4920487461, "modified_at": "2024-02-01T15:45:00Z"}
				]
			}`))
		}
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	models, err := sm.ListInstalledModels(ctx)
	if err != nil {
		t.Fatalf("ListInstalledModels failed: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("models = %d, want 2", len(models))
	}
	if models[0].ID != "qwen2.5-coder:7b" {
		t.Errorf("models[0].ID = %q, want qwen2.5-coder:7b", models[0].ID)
	}
}

func TestConcurrentLoad(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})
		}()
	}
	wg.Wait()

	if !sm.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should be loaded")
	}

	loaded := sm.GetLoadedModels()
	if len(loaded) != 1 {
		t.Errorf("loaded models = %d, want 1", len(loaded))
	}
}

func TestLoadModel_WithOptions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload generateRequest
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if payload.Stream {
			t.Error("stream should be false")
		}
		if payload.KeepAlive != -1 {
			t.Errorf("keep_alive = %d, want -1", payload.KeepAlive)
		}
		if payload.Options["num_gpu"] != float64(2) {
			t.Errorf("num_gpu = %v, want 2", payload.Options["num_gpu"])
		}
		if payload.Options["num_ctx"] != float64(4096) {
			t.Errorf("num_ctx = %v, want 4096", payload.Options["num_ctx"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	sm := NewSessionManager(server.URL, 10*time.Second)
	ctx := context.Background()

	_, err := sm.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{
		NumGPU: 2,
		NumCtx: 4096,
	})
	if err != nil {
		t.Fatalf("LoadModel failed: %v", err)
	}
}

func TestNewEngineManager(t *testing.T) {
	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        "http://localhost:11434",
		TimeoutSeconds: 120,
		MaxSessions:    3,
	}

	em := NewEngineManager(cfg, logger)
	if em == nil {
		t.Fatal("NewEngineManager returned nil")
	}
	if em.config.MaxSessions != 3 {
		t.Errorf("MaxSessions = %d, want 3", em.config.MaxSessions)
	}
}

func TestNewEngineManager_DefaultMaxSessions(t *testing.T) {
	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        "http://localhost:11434",
		TimeoutSeconds: 120,
	}

	em := NewEngineManager(cfg, logger)
	if em.config.MaxSessions != 3 {
		t.Errorf("MaxSessions = %d, want 3 (default)", em.config.MaxSessions)
	}
}

func TestEngineManager_LoadModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 10,
		MaxSessions:    3,
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	if err := em.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{}); err != nil {
		t.Fatalf("LoadModel failed: %v", err)
	}

	if !em.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should be loaded")
	}
}

func TestEngineManager_UnloadModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"done": true, "response": ""}`))
	}))
	defer server.Close()

	logger := slog.Default()
	cfg := &config.LocalEngineConfig{
		BaseURL:        server.URL,
		TimeoutSeconds: 10,
		MaxSessions:    3,
	}
	em := NewEngineManager(cfg, logger)
	ctx := context.Background()

	_ = em.LoadModel(ctx, "qwen2.5-coder:7b", LoadOptions{})

	if err := em.UnloadModel(ctx, "qwen2.5-coder:7b"); err != nil {
		t.Fatalf("UnloadModel failed: %v", err)
	}

	if em.IsLoaded("qwen2.5-coder:7b") {
		t.Error("model should be unloaded")
	}
}