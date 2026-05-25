package proxy

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"git.0ur.uk/nenya/config"
	"git.0ur.uk/nenya/internal/billing"
	"git.0ur.uk/nenya/internal/routing"
)

func TestFilterExhaustedTargets(t *testing.T) {
	bt := billing.NewBillingTracker(nil, nil)
	ctx := context.Background()

	bt.MarkExhausted(ctx, "openai", "exhausted", "over limit")
	bt.RecordSpend(ctx, billing.SpendEntry{
		ProviderName: "openai",
		AccountName:  "active",
		CostUSD:      1.00,
	})

	targets := []routing.UpstreamTarget{
		{Provider: "openai", AccountName: "active", Model: "gpt-4"},
		{Provider: "openai", AccountName: "exhausted", Model: "gpt-4"},
		{Provider: "anthropic", AccountName: "default", Model: "claude-3"},
	}

	filtered := filterExhaustedTargets(targets, bt, slog.Default())

	if len(filtered) != 2 {
		t.Fatalf("Expected 2 targets after filtering, got %d", len(filtered))
	}

	for _, target := range filtered {
		if target.AccountName == "exhausted" {
			t.Error("Exhausted target should have been filtered out")
		}
	}

	if !containsTarget(filtered, "openai", "active") {
		t.Error("Active openai account should be present")
	}
	if !containsTarget(filtered, "anthropic", "default") {
		t.Error("Anthropic default account should be present")
	}
}

func TestFilterExhaustedTargets_NilTracker(t *testing.T) {
	targets := []routing.UpstreamTarget{
		{Provider: "openai", AccountName: "account1", Model: "gpt-4"},
		{Provider: "anthropic", AccountName: "account2", Model: "claude-3"},
	}

	filtered := filterExhaustedTargets(targets, nil, slog.Default())

	if len(filtered) != 2 {
		t.Errorf("Expected 2 targets with nil tracker, got %d", len(filtered))
	}
}

func TestCollectProviderFreeModels(t *testing.T) {
	providers := map[string]*config.Provider{
		"openai": {
			Billing: &config.BillingConfig{
				FreeModels: []string{"gpt-4o-mini", "gpt-4o-mini-free"},
			},
		},
		"anthropic": {
			Billing: &config.BillingConfig{
				FreeOnly: true,
			},
		},
		"none": {
			Billing: nil,
		},
	}

	freeModels := collectProviderFreeModels(providers)

	if len(freeModels) != 1 {
		t.Fatalf("Expected 1 provider with free_models list, got %d", len(freeModels))
	}

	openaiModels, ok := freeModels["openai"]
	if !ok {
		t.Fatal("Expected openai in free models map")
	}
	if len(openaiModels) != 2 {
		t.Errorf("Expected 2 free models for openai, got %d", len(openaiModels))
	}

	if _, ok := freeModels["anthropic"]; ok {
		t.Error("anthropic should not be in free_models (it's free_only, not a list)")
	}
}

func containsTarget(targets []routing.UpstreamTarget, provider, account string) bool {
	for _, target := range targets {
		if target.Provider == provider && target.AccountName == account {
			return true
		}
	}
	return false
}

func TestIntegration_BillingPipelineWithExhaustion(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	billingTracker := billing.NewBillingTracker(slog.Default(), nil)

	billingTracker.MarkExhausted(ctx, "exhausted-provider", "account1", "test exhaustion")

	billingTracker.RecordSpend(ctx, billing.SpendEntry{
		ProviderName: "active-provider",
		AccountName:  "account1",
		CostUSD:      0.01,
	})

	if !billingTracker.IsExhausted("exhausted-provider", "account1") {
		t.Error("Exhausted account should be marked as exhausted")
	}

	if billingTracker.IsExhausted("active-provider", "account1") {
		t.Error("Active account should not be exhausted")
	}

	ratio := billingTracker.GetUtilizationRatio("active-provider", "account1", 1.00)
	if ratio != 0.01 {
		t.Errorf("Expected utilization ratio 0.01, got %f", ratio)
	}

	billingTracker.ResetSpend(ctx, "active-provider", "account1")
	if spend := billingTracker.GetTotalSpend("active-provider", "account1"); spend != 0 {
		t.Errorf("Expected spend 0 after reset, got %f", spend)
	}
}

func TestIntegration_ScoringWithFreeModels(t *testing.T) {
	targets := []routing.UpstreamTarget{
		{Provider: "openai", AccountName: "default", Model: "gpt-4o-mini-free", Format: "openai"},
		{Provider: "openai", AccountName: "default", Model: "gpt-4o", Format: "openai"},
		{Provider: "anthropic", AccountName: "default", Model: "claude-3-haiku:free", Format: "anthropic"},
		{Provider: "anthropic", AccountName: "default", Model: "claude-3-opus", Format: "anthropic"},
	}

	mixedProvider := "openai"
	opts := routing.SortOptions{
		LatencyWeight:   0.5,
		CostWeight:      0.5,
		BillingFreeOnly: map[string]bool{},
		BillingModel: map[string]string{
			mixedProvider: string(config.BillingMixed),
		},
		BillingFreeModels: map[string][]string{
			mixedProvider: {"gpt-4o-mini-free"},
		},
	}

	sorted := routing.SortTargetsByBalanced(targets, nil, nil, nil, opts)

	if len(sorted) != 4 {
		t.Fatalf("Expected 4 sorted targets, got %d", len(sorted))
	}

	freeTargets := 0
	for _, target := range sorted {
		if target.Model == "gpt-4o-mini-free" || target.Model == "claude-3-haiku:free" {
			freeTargets++
		}
	}

	if freeTargets != 2 {
		t.Errorf("Expected 2 free models in results, got %d", freeTargets)
	}
}

func TestIntegration_ConcurrentStressAndExhaustion(t *testing.T) {
	ctx := context.Background()
	bt := billing.NewBillingTracker(slog.Default(), nil)

	const numGoroutines = 100
	const costPerRequest = 0.01

	errCh := make(chan error, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			account := "account1"
			if id%3 == 0 {
				account = "account2"
			}

			bt.RecordSpend(ctx, billing.SpendEntry{
				ProviderName: "openai",
				AccountName:  account,
				CostUSD:      costPerRequest,
				RequestID:    "req-" + string(rune(id)),
			})

			if bt.GetTotalSpend("openai", account) > 0.5 {
				bt.MarkExhausted(ctx, "openai", account, "over limit")
			}
			errCh <- nil
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		if err := <-errCh; err != nil {
			t.Errorf("Goroutine %d failed: %v", i, err)
		}
	}

	totalRequests := bt.TotalRequests.Load()
	if totalRequests != uint64(numGoroutines) {
		t.Errorf("Expected %d requests, got %d", numGoroutines, totalRequests)
	}

	targets := []routing.UpstreamTarget{
		{Provider: "openai", AccountName: "account1", Model: "gpt-4"},
		{Provider: "openai", AccountName: "account2", Model: "gpt-4"},
		{Provider: "openai", AccountName: "account3", Model: "gpt-4"},
	}

	filtered := filterExhaustedTargets(targets, bt, slog.Default())

	if len(filtered) != 2 {
		t.Errorf("Expected 2 non-exhausted targets (account2, account3), got %d", len(filtered))
	}

	for _, target := range filtered {
		if target.AccountName == "account1" {
			t.Error("Exhausted account1 should be filtered out")
		}
	}
}
