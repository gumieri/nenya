package discovery

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"nenya/config"
)

func testHealthLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestNewAdvancedHealthChecker_DefaultConfig(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	if hc.config.Timeout != 10*time.Second {
		t.Errorf("expected default timeout 10s, got %v", hc.config.Timeout)
	}
	if hc.config.MaxRetries != 3 {
		t.Errorf("expected default max retries 3, got %d", hc.config.MaxRetries)
	}
	if hc.config.RetryDelay != 1*time.Second {
		t.Errorf("expected default retry delay 1s, got %v", hc.config.RetryDelay)
	}
	if hc.config.DriftWarningLevel != 5 {
		t.Errorf("expected default drift warning level 5, got %d", hc.config.DriftWarningLevel)
	}
	if hc.config.MaxConcurrent != 5 {
		t.Errorf("expected default max concurrent 5, got %d", hc.config.MaxConcurrent)
	}
}

func TestNewAdvancedHealthChecker_CustomConfig(t *testing.T) {
	cfg := HealthCheckConfig{
		Timeout:           30 * time.Second,
		MaxRetries:        5,
		RetryDelay:        2 * time.Second,
		EnableDriftCheck:  true,
		DriftWarningLevel: 10,
		MaxConcurrent:     2,
	}

	hc := NewAdvancedHealthChecker(cfg, nil, nil, testHealthLogger())

	if hc.config.Timeout != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", hc.config.Timeout)
	}
	if hc.config.MaxConcurrent != 2 {
		t.Errorf("expected max concurrent 2, got %d", hc.config.MaxConcurrent)
	}
}

func TestAdvancedHealthChecker_CheckProvider_Success(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{Timeout: 5 * time.Second}, nil, nil, testHealthLogger())

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		return []DiscoveredModel{
			{ID: "model-1"},
			{ID: "model-2"},
		}, nil
	}

	result := hc.CheckProvider(context.Background(), "test-provider", fetchFunc)

	if result.Status != HealthStatusOK {
		t.Errorf("expected status 'ok', got %q", result.Status)
	}
	if result.ModelsFound != 2 {
		t.Errorf("expected 2 models found, got %d", result.ModelsFound)
	}
	if result.Error != "" {
		t.Errorf("expected no error, got %q", result.Error)
	}
	if result.ResponseTime <= 0 {
		t.Error("expected positive response time")
	}
}

func TestAdvancedHealthChecker_CheckProvider_EmptyResponse(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		return []DiscoveredModel{}, nil
	}

	result := hc.CheckProvider(context.Background(), "test-provider", fetchFunc)

	if result.Status != HealthStatusEmpty {
		t.Errorf("expected status 'empty', got %q", result.Status)
	}
}

func TestAdvancedHealthChecker_CheckProvider_FetchError(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{
		MaxRetries: 2,
		RetryDelay: 10 * time.Millisecond,
	}, nil, nil, testHealthLogger())

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		return nil, errors.New("network error")
	}

	result := hc.CheckProvider(context.Background(), "test-provider", fetchFunc)

	if result.Status != HealthStatusUnreachable {
		t.Errorf("expected status 'unreachable', got %q", result.Status)
	}
	if result.Error == "" {
		t.Error("expected error message")
	}
}

func TestAdvancedHealthChecker_CheckProvider_RetrySuccess(t *testing.T) {
	attempts := 0
	hc := NewAdvancedHealthChecker(HealthCheckConfig{
		MaxRetries: 3,
		RetryDelay: 10 * time.Millisecond,
	}, nil, nil, testHealthLogger())

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		attempts++
		if attempts < 2 {
			return nil, errors.New("transient error")
		}
		return []DiscoveredModel{{ID: "model-1"}}, nil
	}

	result := hc.CheckProvider(context.Background(), "test-provider", fetchFunc)

	if result.Status != HealthStatusOK {
		t.Errorf("expected status 'ok', got %q", result.Status)
	}
	if attempts != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts)
	}
}

func TestAdvancedHealthChecker_CheckProvider_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		return nil, errors.New("should not reach")
	}

	result := hc.CheckProvider(ctx, "test-provider", fetchFunc)

	if result.Status != HealthStatusUnreachable {
		t.Errorf("expected status 'unreachable', got %q", result.Status)
	}
	if result.Error != context.Canceled.Error() {
		t.Errorf("expected '%s', got '%s'", context.Canceled.Error(), result.Error)
	}
}

func TestAdvancedHealthChecker_CheckAllProviders(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{MaxConcurrent: 2}, nil, nil, testHealthLogger())

	fetchFuncs := map[string]func(context.Context) ([]DiscoveredModel, error){
		"p1": func(ctx context.Context) ([]DiscoveredModel, error) {
			return []DiscoveredModel{{ID: "p1-m1"}}, nil
		},
		"p2": func(ctx context.Context) ([]DiscoveredModel, error) {
			return []DiscoveredModel{{ID: "p2-m1"}, {ID: "p2-m2"}}, nil
		},
		"p3": func(ctx context.Context) ([]DiscoveredModel, error) {
			return nil, errors.New("p3 error")
		},
	}

	results := hc.CheckAllProviders(context.Background(), fetchFuncs)

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}
	if results["p1"].Status != HealthStatusOK {
		t.Errorf("p1: expected status 'ok', got %q", results["p1"].Status)
	}
	if results["p2"].ModelsFound != 2 {
		t.Errorf("p2: expected 2 models, got %d", results["p2"].ModelsFound)
	}
	if results["p3"].Status != HealthStatusUnreachable {
		t.Errorf("p3: expected status 'unreachable', got %q", results["p3"].Status)
	}
}

func TestAdvancedHealthChecker_AnalyzeDrift(t *testing.T) {
	registry := map[string]config.ModelEntry{
		"openai:gpt-4":       {Provider: "openai"},
		"openai:gpt-3.5":     {Provider: "openai"},
		"anthropic:claude-3": {Provider: "anthropic"},
	}

	hc := NewAdvancedHealthChecker(HealthCheckConfig{
		EnableDriftCheck:  true,
		DriftWarningLevel: 1,
	}, nil, registry, testHealthLogger())

	discovered := []DiscoveredModel{
		{ID: "openai:gpt-4"},
		{ID: "anthropic:claude-3-opus"},
	}

	result := HealthCheckResult{}

	hc.analyzeDrift("openai", discovered, &result)

	if len(result.MissingModels) != 1 {
		t.Errorf("expected 1 missing model, got %d: %v", len(result.MissingModels), result.MissingModels)
	}
	if len(result.NewModels) != 1 {
		t.Errorf("expected 1 new model, got %d: %v", len(result.NewModels), result.NewModels)
	}
	if result.Status != HealthStatusDegraded {
		t.Errorf("expected status 'degraded', got %q", result.Status)
	}
}

func TestAdvancedHealthChecker_GetResult(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	_, exists := hc.GetResult("nonexistent")
	if exists {
		t.Error("expected false for nonexistent provider")
	}

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		return []DiscoveredModel{{ID: "m1"}}, nil
	}
	hc.CheckProvider(context.Background(), "p1", fetchFunc)

	result, exists := hc.GetResult("p1")
	if !exists {
		t.Error("expected true for existing provider")
	}
	if result.Provider != "p1" {
		t.Errorf("expected provider 'p1', got %q", result.Provider)
	}
}

func TestAdvancedHealthChecker_GetAllResults(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	fetchFunc := func(ctx context.Context) ([]DiscoveredModel, error) {
		return []DiscoveredModel{{ID: "m1"}}, nil
	}
	hc.CheckProvider(context.Background(), "p1", fetchFunc)
	hc.CheckProvider(context.Background(), "p2", fetchFunc)

	results := hc.GetAllResults()
	if len(results) != 2 {
		t.Errorf("expected 2 results, got %d", len(results))
	}
}

func TestAdvancedHealthChecker_GetSummary(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	hc.results["p1"] = HealthCheckResult{
		Status:        HealthStatusOK,
		ModelsFound:   10,
		MissingModels: []string{"m1"},
		NewModels:     []string{"m2"},
	}
	hc.results["p2"] = HealthCheckResult{
		Status:      HealthStatusDegraded,
		ModelsFound: 5,
	}

	summary := hc.GetSummary()

	if summary["ok"].(int) != 1 {
		t.Errorf("expected ok=1, got %v", summary["ok"])
	}
	if summary["degraded"].(int) != 1 {
		t.Errorf("expected degraded=1, got %v", summary["degraded"])
	}
	if summary["total_models"].(int) != 15 {
		t.Errorf("expected total_models=15, got %v", summary["total_models"])
	}
	if summary["missing_models"].(int) != 1 {
		t.Errorf("expected missing_models=1, got %v", summary["missing_models"])
	}
	if summary["new_models"].(int) != 1 {
		t.Errorf("expected new_models=1, got %v", summary["new_models"])
	}
}

func TestAdvancedHealthChecker_GetDriftReport(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	hc.results["p1"] = HealthCheckResult{
		Status:        HealthStatusDegraded,
		MissingModels: []string{"m1"},
		NewModels:     []string{"m2"},
	}
	hc.results["p2"] = HealthCheckResult{
		Status: HealthStatusOK,
	}

	report := hc.GetDriftReport()

	if len(report["drifted_providers"].([]string)) != 1 {
		t.Errorf("expected 1 drifted provider, got %v", report["drifted_providers"])
	}
	if report["drifted_count"].(int) != 1 {
		t.Errorf("expected drifted_count=1, got %v", report["drifted_count"])
	}

	p1Report := report["p1"].(map[string]interface{})
	if p1Report["status"] != HealthStatusDegraded {
		t.Errorf("expected status 'degraded', got %v", p1Report["status"])
	}
}

func TestAdvancedHealthChecker_ClearResults(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	hc.results["p1"] = HealthCheckResult{Status: HealthStatusOK}
	hc.ClearResults()

	if len(hc.results) != 0 {
		t.Errorf("expected empty results after clear, got %d", len(hc.results))
	}
}

func TestAdvancedHealthChecker_IsProviderHealthy(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	hc.results["p1"] = HealthCheckResult{Status: HealthStatusOK}
	hc.results["p2"] = HealthCheckResult{Status: HealthStatusDegraded}
	hc.results["p3"] = HealthCheckResult{Status: HealthStatusUnreachable}

	if !hc.IsProviderHealthy("p1") {
		t.Error("expected p1 to be healthy")
	}
	if !hc.IsProviderHealthy("p2") {
		t.Error("expected p2 to be healthy (degraded)")
	}
	if hc.IsProviderHealthy("p3") {
		t.Error("expected p3 to be unhealthy")
	}
	if hc.IsProviderHealthy("nonexistent") {
		t.Error("expected false for nonexistent provider")
	}
}

func TestAdvancedHealthChecker_GetUnhealthyProviders(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())

	hc.results["p1"] = HealthCheckResult{Status: HealthStatusOK}
	hc.results["p2"] = HealthCheckResult{Status: HealthStatusDegraded}
	hc.results["p3"] = HealthCheckResult{Status: HealthStatusUnreachable}
	hc.results["p4"] = HealthCheckResult{Status: HealthStatusEmpty}

	unhealthy := hc.GetUnhealthyProviders()
	if len(unhealthy) != 2 {
		t.Errorf("expected 2 unhealthy, got %d: %v", len(unhealthy), unhealthy)
	}
}

func TestAdvancedHealthChecker_LogSummary(t *testing.T) {
	hc := NewAdvancedHealthChecker(HealthCheckConfig{}, nil, nil, testHealthLogger())
	hc.results["p1"] = HealthCheckResult{
		Status:        HealthStatusDegraded,
		MissingModels: []string{"m1"},
		NewModels:     []string{"m2"},
	}

	hc.LogSummary()
}
