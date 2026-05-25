package billing

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"git.0ur.uk/nenya/config"
)

type QuotaFetcher struct {
	client    *http.Client
	logger    *slog.Logger
	providers map[string]*quotaProviderConfig
	stopCh    chan struct{}
	wg        sync.WaitGroup
	started   bool
	mu        sync.Mutex
}

type quotaProviderConfig struct {
	name         string
	url          string
	extraction   config.QuotaExtractionConfig
	pollInterval time.Duration
	auth         string
	tracker      *BillingTracker
}

type QuotaFetcherOption func(*QuotaFetcher)

func WithTimeout(d time.Duration) QuotaFetcherOption {
	return func(qf *QuotaFetcher) {
		qf.client.Timeout = d
	}
}

type QuotaFetchResult struct {
	Provider   string
	Account    string
	Info       *QuotaInfo
	FetchedAt  time.Time
	StatusCode int
	RetryAfter time.Duration
	Error      error
}

func NewQuotaFetcher(logger *slog.Logger, opts ...QuotaFetcherOption) *QuotaFetcher {
	qf := &QuotaFetcher{
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:    10,
				IdleConnTimeout: 30 * time.Second,
			},
		},
		logger:    logger,
		providers: make(map[string]*quotaProviderConfig),
		stopCh:    make(chan struct{}),
	}
	for _, opt := range opts {
		opt(qf)
	}
	return qf
}

func (qf *QuotaFetcher) FetchQuota(ctx context.Context, provider, account, url, auth, mode string, cfg config.QuotaExtractionConfig) QuotaFetchResult {
	result := QuotaFetchResult{
		Provider:  provider,
		Account:   account,
		FetchedAt: time.Now(),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result.Error = fmt.Errorf("failed to create request: %w", err)
		return result
	}

	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("User-Agent", "Nenya/1.0")

	qf.logger.DebugContext(ctx, "fetching quota", "provider", provider, "account", account, "url", url)

	resp, err := qf.client.Do(req)
	if err != nil {
		result.Error = fmt.Errorf("request failed: %w", err)
		return result
	}
	defer func() { _ = resp.Body.Close() }()

	result.StatusCode = resp.StatusCode

	if resp.StatusCode != http.StatusOK {
		result.Error = fmt.Errorf("unexpected status code: %d", resp.StatusCode)
		result.RetryAfter = getRetryAfter(resp)
		return result
	}

	info, err := qf.extractQuotaInfo(ctx, resp, mode, cfg)
	if err != nil {
		result.Error = fmt.Errorf("failed to extract quota: %w", err)
		return result
	}
	result.Info = info

	qf.logger.DebugContext(ctx, "quota fetched successfully",
		"provider", provider,
		"account", account,
	)

	return result
}

func (qf *QuotaFetcher) Start(ctx context.Context, tracker *BillingTracker, providers map[string]config.ProviderConfig, secrets *config.SecretsConfig) {
	qf.mu.Lock()
	defer qf.mu.Unlock()

	if qf.started {
		return
	}

	for name, p := range providers {
		if p.Billing == nil {
			continue
		}
		if p.Billing.QuotaSource != "api" {
			continue
		}
		if p.Billing.QuotaURL == "" || p.Billing.QuotaExtraction == nil {
			qf.logger.Warn("provider has quota_source=api but missing quota_url or quota_extraction",
				"provider", name)
			continue
		}

		if !qf.validateExtractionConfig(name, *p.Billing.QuotaExtraction) {
			continue
		}

		interval := qf.parsePollInterval(name, p.Billing.QuotaInterval)
		apiKey := qf.getProviderKey(name, secrets)

		qf.providers[name] = &quotaProviderConfig{
			name:         name,
			url:          p.Billing.QuotaURL,
			extraction:   *p.Billing.QuotaExtraction,
			pollInterval: interval,
			auth:         apiKey,
			tracker:      tracker,
		}
	}

	if len(qf.providers) == 0 {
		qf.logger.Debug("no providers with quota_source=api configured")
		qf.started = true
		return
	}

	qf.started = true

	for _, cfg := range qf.providers {
		qf.wg.Add(1)
		go qf.pollLoop(ctx, cfg)
	}

	qf.logger.Info("quota fetcher started", "providers", len(qf.providers))
}

func (qf *QuotaFetcher) validateExtractionConfig(providerName string, cfg config.QuotaExtractionConfig) bool {
	if cfg.Mode == "" {
		qf.logger.Warn("provider has quota_source=api but empty extraction mode",
			"provider", providerName)
		return false
	}
	if cfg.Mode == config.ExtractionModeSimpleJSON && cfg.BalancePath == "" {
		qf.logger.Warn("provider has extraction mode simple_json but empty balance_path",
			"provider", providerName)
		return false
	}
	if cfg.Mode == config.ExtractionModeMaxFromArray && cfg.ArrayPath == "" {
		qf.logger.Warn("provider has extraction mode max_from_array but empty array_path",
			"provider", providerName)
		return false
	}
	return true
}

func (qf *QuotaFetcher) parsePollInterval(providerName, intervalStr string) time.Duration {
	if intervalStr == "" {
		return 5 * time.Minute
	}
	d, err := time.ParseDuration(intervalStr)
	if err == nil && d > 0 {
		return d
	}
	qf.logger.Warn("invalid poll_interval, using default", "provider", providerName, "interval", intervalStr)
	return 5 * time.Minute
}

func (qf *QuotaFetcher) getProviderKey(providerName string, secrets *config.SecretsConfig) string {
	if secrets == nil {
		return ""
	}
	return secrets.ProviderKeys[providerName]
}

func (qf *QuotaFetcher) Stop() {
	qf.mu.Lock()
	if !qf.started {
		qf.mu.Unlock()
		return
	}
	close(qf.stopCh)
	qf.started = false
	qf.mu.Unlock()

	qf.wg.Wait()
	qf.logger.Info("quota fetcher stopped")
}

func (qf *QuotaFetcher) pollLoop(ctx context.Context, cfg *quotaProviderConfig) {
	defer qf.wg.Done()

	qf.fetchAndUpdate(ctx, cfg)

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-qf.stopCh:
			return
		case <-ticker.C:
			qf.fetchAndUpdate(ctx, cfg)
		}
	}
}

func (qf *QuotaFetcher) fetchAndUpdate(ctx context.Context, cfg *quotaProviderConfig) {
	pollCtx, cancel := context.WithTimeout(ctx, qf.client.Timeout+5*time.Second)
	defer cancel()

	result := qf.FetchQuota(pollCtx, cfg.name, "default", cfg.url, cfg.auth, string(cfg.extraction.Mode), cfg.extraction)
	if result.Error != nil {
		qf.logger.WarnContext(ctx, "quota fetch failed",
			"provider", cfg.name, "error", result.Error)
		return
	}

	if result.Info != nil && cfg.tracker != nil {
		if result.Info.BalanceUSD <= 0 {
			cfg.tracker.MarkExhausted(ctx, cfg.name, "default", "quota exhausted via API polling")
		}
	}
}

func getRetryAfter(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	retryAfter := resp.Header.Get("Retry-After")
	if retryAfter == "" {
		return 0
	}
	seconds, err := time.ParseDuration(retryAfter + "s")
	if err != nil {
		return 0
	}
	return seconds
}

func (qf *QuotaFetcher) extractQuotaInfo(ctx context.Context, resp *http.Response, mode string, cfg config.QuotaExtractionConfig) (*QuotaInfo, error) {
	billingCfg := toQuotaExtractionConfig(cfg)
	switch config.QuotaExtractionMode(mode) {
	case config.ExtractionModeSimpleJSON, config.ExtractionModeMaxFromArray:
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read quota response body: %w", err)
		}
		info, err := ExtractQuotaFromResponse(ctx, body, billingCfg, qf.logger)
		if err != nil {
			return nil, fmt.Errorf("failed to extract quota from response: %w", err)
		}
		return info, nil

	case config.ExtractionModeHeaders:
		info, err := ExtractQuotaFromHeaders(ctx, resp.Header, billingCfg, qf.logger)
		if err != nil {
			return nil, fmt.Errorf("failed to extract quota from headers: %w", err)
		}
		return info, nil

	default:
		return nil, fmt.Errorf("unsupported quota mode: %s (supported: simple_json, max_from_array, headers)", mode)
	}
}

func toQuotaExtractionConfig(cfg config.QuotaExtractionConfig) QuotaExtractionConfig {
	return QuotaExtractionConfig{
		Mode:            string(cfg.Mode),
		BalancePath:     cfg.BalancePath,
		ArrayPath:       cfg.ArrayPath,
		ValueField:      cfg.ValueField,
		ValueDivideBy:   cfg.ValueDivideBy,
		ResetField:      cfg.ResetField,
		ResetUnit:       cfg.ResetUnit,
		LevelField:      cfg.LevelField,
		RemainingHeader: cfg.RemainingHeader,
		LimitHeader:     cfg.LimitHeader,
		ResetHeader:     cfg.ResetHeader,
	}
}

func toProviderExtractionConfig(cfg QuotaExtractionConfig) config.QuotaExtractionConfig {
	return config.QuotaExtractionConfig{
		Mode:            config.QuotaExtractionMode(cfg.Mode),
		BalancePath:     cfg.BalancePath,
		ArrayPath:       cfg.ArrayPath,
		ValueField:      cfg.ValueField,
		ValueDivideBy:   cfg.ValueDivideBy,
		ResetField:      cfg.ResetField,
		ResetUnit:       cfg.ResetUnit,
		LevelField:      cfg.LevelField,
		RemainingHeader: cfg.RemainingHeader,
		LimitHeader:     cfg.LimitHeader,
		ResetHeader:     cfg.ResetHeader,
	}
}

func (qf *QuotaFetcher) FetchMultipleQuotas(ctx context.Context, fetches []QuotaFetchRequest) []QuotaFetchResult {
	results := make([]QuotaFetchResult, len(fetches))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 5)

	for i, fetch := range fetches {
		wg.Add(1)
		go func(idx int, f QuotaFetchRequest) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			cfg := toProviderExtractionConfig(f.Config)
			results[idx] = qf.FetchQuota(ctx, f.Provider, f.Account, f.URL, f.Auth, f.Mode, cfg)
		}(i, fetch)
	}

	wg.Wait()
	return results
}

type QuotaFetchRequest struct {
	Provider string
	Account  string
	URL      string
	Auth     string
	Mode     string
	Config   QuotaExtractionConfig
}
