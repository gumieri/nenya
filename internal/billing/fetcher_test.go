package billing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"git.0ur.uk/nenya/config"
	"git.0ur.uk/nenya/internal/resilience"
	"git.0ur.uk/nenya/internal/testutil"
)

func TestQuotaFetcher_Lifecycle(t *testing.T) {
	logger := testutil.NewTestLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/quota/provider1" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"balance": 100.0,
			})
		} else if r.URL.Path == "/quota/provider2" {
			w.WriteHeader(http.StatusTooManyRequests)
		} else if r.URL.Path == "/quota/provider3" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"balance": []map[string]any{
					{"percentage": 75, "name": "tier1"},
					{"percentage": 95, "name": "tier2"},
				},
			})
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	providers := map[string]config.ProviderConfig{
		"provider1": {
			Billing: &config.BillingConfig{
				Model:         config.BillingCredit,
				QuotaSource:   config.QuotaSourceAPI,
				QuotaURL:      server.URL + "/quota/provider1",
				QuotaInterval: "100ms",
				QuotaExtraction: &config.QuotaExtractionConfig{
					Mode:        config.ExtractionModeSimpleJSON,
					BalancePath: "balance",
				},
			},
		},
		"provider2": {
			Billing: &config.BillingConfig{
				Model:         config.BillingSubscription,
				QuotaSource:   config.QuotaSourceAPI,
				QuotaURL:      server.URL + "/quota/provider2",
				QuotaInterval: "200ms",
				QuotaExtraction: &config.QuotaExtractionConfig{
					Mode:        config.ExtractionModeSimpleJSON,
					BalancePath: "balance",
				},
			},
		},
		"provider3": {
			Billing: &config.BillingConfig{
				Model:         config.BillingFree,
				QuotaSource:   config.QuotaSourceAPI,
				QuotaURL:      server.URL + "/quota/provider3",
				QuotaInterval: "150ms",
				QuotaExtraction: &config.QuotaExtractionConfig{
					Mode:          config.ExtractionModeMaxFromArray,
					ArrayPath:     "balance",
					ValueField:    "percentage",
					ValueDivideBy: 1,
				},
			},
		},
		"provider_no_billing": {
			Billing: nil,
		},
	}
	secrets := &config.SecretsConfig{
		ProviderKeys: map[string]string{
			"provider1": "Bearer test-key",
		},
	}

	tracker := NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	fetcher.Start(ctx, tracker, providers, secrets, nil)

	time.Sleep(350 * time.Millisecond)

	if tracker.GetTotalSpend("provider1", "") != 0 {
		t.Errorf("provider1 should have spend=0, got %f", tracker.GetTotalSpend("provider1", ""))
	}

	if tracker.IsExhausted("provider1", "") {
		t.Error("provider1 should not be exhausted")
	}

	fetcher.Stop()
}

func TestQuotaFetcher_InitialFetch(t *testing.T) {
	logger := testutil.NewTestLogger()
	_ = NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"balance": 50.0,
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := fetcher.FetchQuota(ctx, "test-provider", "default", server.URL, "", string(config.ExtractionModeSimpleJSON), config.QuotaExtractionConfig{
		Mode:        config.ExtractionModeSimpleJSON,
		BalancePath: "balance",
	})

	if result.Error != nil {
		t.Fatalf("Unexpected error: %v", result.Error)
	}
	if result.Provider != "test-provider" {
		t.Errorf("Provider = %q, want test-provider", result.Provider)
	}
	if result.Info == nil {
		t.Fatal("Info is nil")
	}
	if result.Info.BalanceUSD != 50.0 {
		t.Errorf("BalanceUSD = %f, want 50.0", result.Info.BalanceUSD)
	}
}

func TestFetchAndUpdate_AccountName(t *testing.T) {
	logger := testutil.NewTestLogger()

	tracker := NewBillingTracker(logger, nil)
	tracker.RecordSpend(context.Background(), SpendEntry{
		ProviderName: "test-provider",
		AccountName:  "account-1",
		InputTokens:  1000,
		OutputTokens: 500,
		CostUSD:      0.001,
		Timestamp:    time.Now(),
	})

	qf := NewQuotaFetcher(logger)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"balance": 100.0,
		})
	}))
	defer server.Close()

	cfg := &quotaProviderConfig{
		name:  "test-provider",
		url:   server.URL,
		auth:  "test-key",
		tracker: tracker,
		extraction: config.QuotaExtractionConfig{
			Mode:        config.ExtractionModeSimpleJSON,
			BalancePath: "balance",
		},
	}

	result := qf.fetchAndUpdate(context.Background(), cfg, "account-1")
	if result.Error != nil {
		t.Fatalf("unexpected error: %v", result.Error)
	}
	if result.Account != "account-1" {
		t.Errorf("expected account-1, got %s", result.Account)
	}
	if result.Provider != "test-provider" {
		t.Errorf("expected test-provider, got %s", result.Provider)
	}
}

type mockAccountLister struct {
	accounts map[string][]string
}

func (m *mockAccountLister) ListAccountIDs(ctx context.Context, provider string) []string {
	return m.accounts[provider]
}

func TestStart_MultipleAccounts(t *testing.T) {
	logger := testutil.NewTestLogger()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"balance": 100.0})
	}))
	defer server.Close()

	providers := map[string]config.ProviderConfig{
		"provider1": {
			Billing: &config.BillingConfig{
				QuotaSource:   "api",
				QuotaURL:      server.URL + "/quota/provider1",
				QuotaInterval: "10ms",
				QuotaExtraction: &config.QuotaExtractionConfig{
					Mode:        config.ExtractionModeSimpleJSON,
					BalancePath: "balance",
				},
			},
		},
	}

	tracker := NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)
	lister := &mockAccountLister{
		accounts: map[string][]string{
			"provider1": {"account-1", "account-2"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	fetcher.Start(ctx, tracker, providers, nil, lister)

	time.Sleep(50 * time.Millisecond)

	spend := tracker.GetTotalSpend("provider1", "account-1")
	if spend != 0 {
		t.Errorf("expected 0 spend, got %f", spend)
	}

	spend2 := tracker.GetTotalSpend("provider1", "account-2")
	if spend2 != 0 {
		t.Errorf("expected 0 spend, got %f", spend2)
	}
}

func TestQuotaFetcher_HTTPError(t *testing.T) {
	logger := testutil.NewTestLogger()
	_ = NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := fetcher.FetchQuota(ctx, "test", "default", server.URL, "", string(config.ExtractionModeSimpleJSON), config.QuotaExtractionConfig{
		Mode:        config.ExtractionModeSimpleJSON,
		BalancePath: "balance",
	})

	if result.Error == nil {
		t.Error("Expected error for 500 response, got nil")
	}
	if result.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d, want 500", result.StatusCode)
	}
}

func TestQuotaFetcher_ParseError(t *testing.T) {
	logger := testutil.NewTestLogger()
	_ = NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("invalid json{{{"))
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := fetcher.FetchQuota(ctx, "test", "default", server.URL, "", string(config.ExtractionModeSimpleJSON), config.QuotaExtractionConfig{
		Mode:        config.ExtractionModeSimpleJSON,
		BalancePath: "balance",
	})

	if result.Error == nil {
		t.Error("Expected error for invalid JSON, got nil")
	} else if !strings.Contains(result.Error.Error(), "failed to extract quota") {
		t.Errorf("Error message = %q, want substring %q", result.Error, "failed to extract quota")
	}
	if result.Info != nil {
		t.Error("Info should be nil for parse error")
	}
}

func TestQuotaFetcher_UnsupportedMode(t *testing.T) {
	logger := testutil.NewTestLogger()
	_ = NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"balance": 1.0})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := fetcher.FetchQuota(ctx, "test", "default", server.URL, "", "invalid_mode", config.QuotaExtractionConfig{
		Mode:        "invalid_mode",
		BalancePath: "balance",
	})

	if result.Error == nil {
		t.Fatal("Expected error for unsupported mode, got nil")
	}
	if !strings.Contains(result.Error.Error(), "unsupported quota mode") {
		t.Errorf("Error message = %q, want substring %q", result.Error, "unsupported quota mode")
	}
	if !strings.Contains(result.Error.Error(), "simple_json") || !strings.Contains(result.Error.Error(), "headers") {
		t.Errorf("Error message should list supported modes: %q", result.Error)
	}
}

func TestQuotaFetcher_ErrorWrapping(t *testing.T) {
	logger := testutil.NewTestLogger()
	fetcher := NewQuotaFetcher(logger)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := fetcher.FetchQuota(ctx, "test", "default", "http://127.0.0.1:1/nonexistent", "", string(config.ExtractionModeSimpleJSON), config.QuotaExtractionConfig{
		Mode:        config.ExtractionModeSimpleJSON,
		BalancePath: "balance",
	})

	if result.Error == nil {
		t.Fatal("Expected error for connection refused, got nil")
	}
	if !strings.Contains(result.Error.Error(), "request failed") {
		t.Errorf("Error message = %q, want substring %q", result.Error, "request failed")
	}
}

func TestQuotaFetcher_RetryAfterHeader(t *testing.T) {
	logger := testutil.NewTestLogger()
	_ = NewBillingTracker(logger, nil)
	fetcher := NewQuotaFetcher(logger)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	result := fetcher.FetchQuota(ctx, "test", "default", server.URL, "", string(config.ExtractionModeSimpleJSON), config.QuotaExtractionConfig{
		Mode:        config.ExtractionModeSimpleJSON,
		BalancePath: "balance",
	})

	if result.Error == nil {
		t.Error("Expected error for 429, got nil")
	}
	if result.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter = %v, want 30s", result.RetryAfter)
	}
}

func TestQuotaFetcher_ClampDuration(t *testing.T) {
	tests := []struct {
		name        string
		d, min, max time.Duration
		want        time.Duration
	}{
		{"in range", 5 * time.Second, 0, 10 * time.Second, 5 * time.Second},
		{"below min", -1 * time.Second, 0, 10 * time.Second, 0},
		{"above max", 20 * time.Second, 0, 10 * time.Second, 10 * time.Second},
		{"equals min", 0, 0, 10 * time.Second, 0},
		{"equals max", 10 * time.Second, 0, 10 * time.Second, 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampDuration(tt.d, tt.min, tt.max); got != tt.want {
				t.Errorf("clampDuration(%v, %v, %v) = %v, want %v", tt.d, tt.min, tt.max, got, tt.want)
			}
		})
	}
}

func TestQuotaFetcher_NextPollDelay_SuccessResetsBackoff(t *testing.T) {
	qf := &QuotaFetcher{logger: testutil.NewTestLogger()}
	cfg := &quotaProviderConfig{
		name:         "test-provider",
		pollInterval: 1 * time.Minute,
		backoff:      resilience.NewBackoffTracker(),
		maxBackoff:   5 * time.Minute,
	}

	_, _ = cfg.backoff.Increment("test-provider")
	if cfg.backoff.GetLevel("test-provider") != 1 {
		t.Fatalf("backoff level should be 1 before success, got %d", cfg.backoff.GetLevel("test-provider"))
	}

	result := QuotaFetchResult{Error: nil}
	delay := qf.nextPollDelay(cfg, result)

	if delay != cfg.pollInterval {
		t.Errorf("delay = %v, want pollInterval %v", delay, cfg.pollInterval)
	}
	if cfg.backoff.GetLevel("test-provider") != 0 {
		t.Errorf("backoff level should reset to 0 on success, got %d", cfg.backoff.GetLevel("test-provider"))
	}
}

func TestQuotaFetcher_NextPollDelay_429UsesRetryAfter(t *testing.T) {
	qf := &QuotaFetcher{logger: testutil.NewTestLogger()}
	cfg := &quotaProviderConfig{
		name:         "test-provider",
		pollInterval: 1 * time.Minute,
		backoff:      resilience.NewBackoffTracker(),
		maxBackoff:   5 * time.Minute,
	}

	result := QuotaFetchResult{
		Error:      fmt.Errorf("rate limited"),
		StatusCode: http.StatusTooManyRequests,
		RetryAfter: 2 * time.Minute,
	}
	delay := qf.nextPollDelay(cfg, result)

	if delay != 2*time.Minute {
		t.Errorf("delay = %v, want 2min", delay)
	}
	if cfg.backoff.GetLevel("test-provider") != 0 {
		t.Errorf("backoff level should not increment when Retry-After is used, got %d", cfg.backoff.GetLevel("test-provider"))
	}
}

func TestQuotaFetcher_NextPollDelay_ErrorIncrementsBackoff(t *testing.T) {
	qf := &QuotaFetcher{logger: testutil.NewTestLogger()}
	cfg := &quotaProviderConfig{
		name:         "test-provider",
		pollInterval: 10 * time.Second,
		backoff:      resilience.NewBackoffTracker(),
		maxBackoff:   5 * time.Minute,
	}

	result := QuotaFetchResult{
		Error:      fmt.Errorf("network error"),
		StatusCode: http.StatusInternalServerError,
	}

	var delays []time.Duration
	for i := 0; i < 5; i++ {
		delay := qf.nextPollDelay(cfg, result)
		delays = append(delays, delay)
	}

	if cfg.backoff.GetLevel("test-provider") != 5 {
		t.Errorf("backoff level after 5 errors = %d, want 5", cfg.backoff.GetLevel("test-provider"))
	}

	if delays[0] >= delays[1] {
		t.Errorf("delays should increase: delay[0]=%v, delay[1]=%v", delays[0], delays[1])
	}

	if cfg.backoff.GetLevel("test-provider") > 1 {
		qf.nextPollDelay(cfg, QuotaFetchResult{})
		if cfg.backoff.GetLevel("test-provider") != 0 {
			t.Errorf("backoff level should reset to 0 after success, got %d", cfg.backoff.GetLevel("test-provider"))
		}
	}
}

func TestQuotaFetcher_NextPollDelay_RetryAfterCappedAtMax(t *testing.T) {
	qf := &QuotaFetcher{logger: testutil.NewTestLogger()}
	cfg := &quotaProviderConfig{
		name:         "test-provider",
		pollInterval: 1 * time.Minute,
		backoff:      resilience.NewBackoffTracker(),
		maxBackoff:   2 * time.Minute,
	}

	result := QuotaFetchResult{
		Error:      fmt.Errorf("rate limited"),
		StatusCode: http.StatusTooManyRequests,
		RetryAfter: 10 * time.Minute,
	}
	delay := qf.nextPollDelay(cfg, result)

	if delay != 2*time.Minute {
		t.Errorf("delay = %v, want 2min (maxBackoff)", delay)
	}
}

func TestQuotaFetcher_NextPollDelay_ExponentialCappedAtMax(t *testing.T) {
	qf := &QuotaFetcher{logger: testutil.NewTestLogger()}
	cfg := &quotaProviderConfig{
		name:         "test-provider",
		pollInterval: 1 * time.Minute,
		backoff:      resilience.NewBackoffTracker(),
		maxBackoff:   3 * time.Minute,
	}

	result := QuotaFetchResult{
		Error:      fmt.Errorf("network error"),
		StatusCode: http.StatusInternalServerError,
	}

	for i := 0; i < 10; i++ {
		delay := qf.nextPollDelay(cfg, result)
		if delay > cfg.maxBackoff {
			t.Errorf("delay %d = %v exceeds maxBackoff %v", i, delay, cfg.maxBackoff)
		}
	}
}
