package discovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"nenya/config"
)

func TestFetchProviderModels_RetryOnNetworkError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{map[string]string{"id": "test-model"}},
		})
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:           "test-provider",
		URL:            server.URL + "/chat/completions",
		AuthStyle:      "none",
		TimeoutSeconds: 30,
	}

	df := NewDiscoveryFetcher(3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	logger := slog.Default()
	models, err := df.fetchProviderModels(ctx, "test-provider", provider, logger)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if models[0].ID != "test-model" {
		t.Errorf("expected model ID test-model, got %s", models[0].ID)
	}
	if attempts.Load() != 3 {
		t.Errorf("expected 3 attempts, got %d", attempts.Load())
	}
}

func TestFetchProviderModels_NoRetryOnContextTimeout(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{},
		})
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:           "test-provider",
		URL:            server.URL + "/chat/completions",
		AuthStyle:      "none",
		TimeoutSeconds: 30,
	}

	df := NewDiscoveryFetcher(10)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	logger := slog.Default()
	_, err := df.fetchProviderModels(ctx, "test-provider", provider, logger)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if attempts.Load() > 2 {
		t.Errorf("expected at most 2 attempts due to timeout, got %d", attempts.Load())
	}
}

func TestFetchProviderModels_FirstAttemptSucceeds(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{map[string]string{"id": "model-1"}},
		})
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:           "test-provider",
		URL:            server.URL + "/chat/completions",
		AuthStyle:      "none",
		TimeoutSeconds: 30,
	}

	df := NewDiscoveryFetcher(5)
	ctx := context.Background()
	logger := slog.Default()

	models, err := df.fetchProviderModels(ctx, "test-provider", provider, logger)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}
	if attempts.Load() != 1 {
		t.Errorf("expected 1 attempt, got %d", attempts.Load())
	}
}

func TestFetchProviderModels_ProviderOverride(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{map[string]string{"id": "model-2"}},
		})
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:             "test-provider",
		URL:              server.URL + "/chat/completions",
		AuthStyle:        "none",
		TimeoutSeconds:   30,
		MaxRetryAttempts: 2,
	}

	df := NewDiscoveryFetcher(5)
	ctx := context.Background()
	logger := slog.Default()

	models, err := df.fetchProviderModels(ctx, "test-provider", provider, logger)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if len(models) != 1 || models[0].ID != "model-2" {
		t.Fatalf("expected 1 model with ID model-2, got %d models", len(models))
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts (provider override), got %d", attempts.Load())
	}
}
