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

	"github.com/nenya/config"
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
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
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
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

func TestFetchProviderModels_NoBackfillWithoutProviderAllows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{map[string]string{"id": "gpt-4"}},
		})
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:           "test-provider",
		URL:            server.URL + "/chat/completions",
		AuthStyle:      "none",
		TimeoutSeconds: 30,
		AllowedModels:  []string{},
	}

	df := NewDiscoveryFetcher(5)
	ctx := context.Background()
	logger := slog.Default()

	models, err := df.fetchProviderModels(ctx, "test-provider", provider, logger)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	if len(models) != 1 {
		t.Errorf("expected 1 model (no backfill), got %d", len(models))
	}
	if models[0].ID != "gpt-4" {
		t.Errorf("expected model ID gpt-4, got %s", models[0].ID)
	}
}

func TestFetchProviderModels_BackfillRespectsProviderAllows(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"object": "list",
			"data":   []interface{}{map[string]string{"id": "gpt-4"}},
		})
	}))
	defer server.Close()

	provider := &config.Provider{
		Name:           "test-provider",
		URL:            server.URL + "/chat/completions",
		AuthStyle:      "none",
		TimeoutSeconds: 30,
		AllowedModels:  []string{"^gpt-4$"},
	}

	df := NewDiscoveryFetcher(5)
	ctx := context.Background()
	logger := slog.Default()

	models, err := df.fetchProviderModels(ctx, "test-provider", provider, logger)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}

	if len(models) != 1 {
		t.Errorf("expected 1 model (anchored pattern prevents backfill), got %d", len(models))
	}
	if models[0].ID != "gpt-4" {
		t.Errorf("expected model ID gpt-4, got %s", models[0].ID)
	}
}

func TestBackfillStaticModels_PreservesInferredCapabilities(t *testing.T) {
	logger := slog.Default()

	t.Run("zai-coding-plan models keep inferred capabilities", func(t *testing.T) {
		provider := &config.Provider{
			Name:           "zai-coding-plan",
			URL:            "https://api.z.ai/api/coding/paas/v4/chat/completions",
			AuthStyle:      "none",
			TimeoutSeconds: 30,
			AllowedModels:  []string{"glm-5.3-flash"},
		}
		// Raw AllowedModels without compiled regexes (unit context: allowedRE
		// is only populated by config loading) makes AllowsModel allow-all, so
		// every static zai-coding-plan model is backfilled deterministically.

		models := backfillStaticModels(nil, "zai-coding-plan", provider, logger)
		// Exactly 2 because the zai-coding-plan registry has exactly two
		// entries and the uncompiled allowlist (allowedRE nil) allow-alls.
		if len(models) != 2 {
			t.Fatalf("expected 2 backfilled models (glm-5.3, glm-5.3-flash), got %d", len(models))
		}
		byID := make(map[string]DiscoveredModel, len(models))
		for _, m := range models {
			byID[m.ID] = m
		}

		flash, ok := byID["glm-5.3-flash"]
		if !ok {
			t.Fatal("expected glm-5.3-flash to be backfilled")
		}
		if flash.Metadata == nil {
			t.Fatal("expected non-nil metadata on backfilled glm-5.3-flash")
		}
		if !flash.Metadata.SupportsVision {
			t.Error("backfilled glm-5.3-flash lost SupportsVision (InferCapabilities not applied)")
		}
		if !flash.Metadata.SupportsReasoning {
			t.Error("backfilled glm-5.3-flash lost SupportsReasoning")
		}
		if !flash.Metadata.SupportsToolCalls {
			t.Error("backfilled glm-5.3-flash lost SupportsToolCalls")
		}
		if flash.Metadata.Pricing == nil {
			t.Fatal("expected static registry pricing to carry over to backfilled glm-5.3-flash")
		}
		if flash.Metadata.Pricing.InputCostPer1M != 0.15 || flash.Metadata.Pricing.OutputCostPer1M != 0.5 {
			t.Errorf("unexpected pricing on backfilled glm-5.3-flash: input=%v output=%v, want 0.15/0.5",
				flash.Metadata.Pricing.InputCostPer1M, flash.Metadata.Pricing.OutputCostPer1M)
		}

		text, ok := byID["glm-5.3"]
		if !ok {
			t.Fatal("expected glm-5.3 to be backfilled")
		}
		if text.Metadata == nil {
			t.Fatal("expected non-nil metadata on backfilled glm-5.3")
		}
		if text.Metadata.SupportsVision {
			t.Error("backfilled glm-5.3 must remain text-only (no SupportsVision)")
		}
		if !text.Metadata.SupportsReasoning {
			t.Error("backfilled glm-5.3 lost SupportsReasoning")
		}
	})

	// minimax-m2.5 has no capability prefix rule and no Thinking config, so
	// backfill must produce empty-but-non-nil metadata rather than nil.
	t.Run("model without capability rule gets empty metadata", func(t *testing.T) {
		provider := &config.Provider{
			Name:           "minimax_free",
			URL:            "https://api.minimax.io/v1/chat/completions",
			AuthStyle:      "none",
			TimeoutSeconds: 30,
			AllowedModels:  []string{"minimax-m2.5"},
		}

		models := backfillStaticModels(nil, "minimax_free", provider, logger)
		var target *DiscoveredModel
		for i := range models {
			if models[i].ID == "minimax-m2.5" {
				target = &models[i]
				break
			}
		}
		if target == nil {
			t.Fatal("expected minimax-m2.5 to be backfilled")
		}
		if target.Metadata == nil {
			t.Fatal("expected non-nil metadata on backfilled model without capability rules")
		}
		if target.Metadata.SupportsVision {
			t.Error("minimax-m2.5 should not gain SupportsVision from fallback")
		}
	})
}
