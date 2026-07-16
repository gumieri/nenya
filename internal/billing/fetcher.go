package billing

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/resilience"
)

const (
	// defaultQuotaBackoffMax is the default maximum backoff between
	// quota fetch retries after consecutive failures.
	defaultQuotaBackoffMax = 5 * time.Minute
	maxQuotaResponseBytes  = 10 << 20
)

// AccountLister returns all account IDs for a provider.
// Used by QuotaFetcher to poll quota per account.
type AccountLister interface {
	ListAccountIDs(ctx context.Context, provider string) []string
}

type QuotaFetcher struct {
	client    *http.Client
	logger    *slog.Logger
	providers map[string]*quotaProviderConfig
	stopCh    chan struct{}
	wg        sync.WaitGroup
	started   bool
	mu        sync.Mutex
}

// quotaProviderConfig holds per-provider quota polling configuration,
// including backoff state, timeouts, and extraction settings.
type quotaProviderConfig struct {
	name         string
	accounts     []string
	url          string
	extraction   config.QuotaExtractionConfig
	pollInterval time.Duration
	timeout      time.Duration
	auth         string
	tracker      *BillingTracker
	backoff      *resilience.BackoffTracker
	maxBackoff   time.Duration
}

// QuotaFetchResult holds the result of a single quota fetch operation,
// including parsed quota info, HTTP status, Retry-After directive, and
// any error encountered during the request or extraction.
type QuotaFetchResult struct {
	Provider   string
	Account    string
	Info       *QuotaInfo
	FetchedAt  time.Time
	StatusCode int
	RetryAfter time.Duration
	Error      error
}

// NewQuotaFetcher creates a new QuotaFetcher with a shared HTTP client.
// The client has no global timeout (Timeout=0) to allow per-provider
// timeout control via context.WithTimeout in the polling goroutines.
// Per-provider timeouts are read from BillingConfig.QuotaTimeoutSeconds
// during Start() (default: 10s).
func NewQuotaFetcher(logger *slog.Logger) *QuotaFetcher {
	qf := &QuotaFetcher{
		client: &http.Client{
			Timeout: 0, // per-provider timeout via context.WithTimeout in fetchAndUpdate
			Transport: &http.Transport{
				MaxIdleConns:    10,
				IdleConnTimeout: 30 * time.Second,
			},
		},
		logger:    logger,
		providers: make(map[string]*quotaProviderConfig),
		stopCh:    make(chan struct{}),
	}
	return qf
}

func (qf *QuotaFetcher) FetchQuota(ctx context.Context, provider, account, url, auth, mode string, cfg config.QuotaExtractionConfig) QuotaFetchResult {
	result := QuotaFetchResult{
		Provider:  provider,
		Account:   account,
		FetchedAt: time.Now(),
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
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

func (qf *QuotaFetcher) configureProvider(ctx context.Context, tracker *BillingTracker, name string, p config.ProviderConfig, secrets *config.SecretsConfig, backoffTracker *resilience.BackoffTracker, accountLister AccountLister) *quotaProviderConfig {
	if p.Billing == nil {
		return nil
	}
	if p.Billing.QuotaSource != "api" {
		return nil
	}
	if p.Billing.QuotaURL == "" || p.Billing.QuotaExtraction == nil {
		qf.logger.Warn("provider has quota_source=api but missing quota_url or quota_extraction",
			"provider", name)
		return nil
	}

	if !qf.validateExtractionConfig(name, *p.Billing.QuotaExtraction) {
		return nil
	}

	interval := qf.parsePollInterval(name, p.Billing.QuotaInterval)
	apiKey := qf.getProviderKey(name, secrets)

	timeout := 10 * time.Second
	if p.Billing.QuotaTimeoutSeconds > 0 {
		timeout = time.Duration(p.Billing.QuotaTimeoutSeconds) * time.Second
	}

	maxBackoff := defaultQuotaBackoffMax
	if p.Billing.QuotaBackoffMaxSeconds > 0 {
		maxBackoff = time.Duration(p.Billing.QuotaBackoffMaxSeconds) * time.Second
	}

	accounts := []string{"default"}
	if accountLister != nil {
		if ids := accountLister.ListAccountIDs(ctx, name); len(ids) > 0 {
			accounts = ids
		}
	}

	return &quotaProviderConfig{
		name:         name,
		accounts:     accounts,
		url:          p.Billing.QuotaURL,
		extraction:   *p.Billing.QuotaExtraction,
		pollInterval: interval,
		timeout:      timeout,
		auth:         apiKey,
		tracker:      tracker,
		backoff:      backoffTracker,
		maxBackoff:   maxBackoff,
	}
}

func (qf *QuotaFetcher) Start(ctx context.Context, tracker *BillingTracker, providers map[string]config.ProviderConfig, secrets *config.SecretsConfig, accountLister AccountLister) {
	qf.mu.Lock()
	defer qf.mu.Unlock()

	if qf.started {
		return
	}

	backoffTracker := resilience.NewBackoffTracker()

	for name, p := range providers {
		cfg := qf.configureProvider(ctx, tracker, name, p, secrets, backoffTracker, accountLister)
		if cfg != nil {
			qf.providers[name] = cfg
		}
	}

	if len(qf.providers) == 0 {
		qf.logger.Debug("no providers with quota_source=api configured")
		qf.started = true
		return
	}

	qf.started = true

	var pollTasks []func()
	for _, cfg := range qf.providers {
		for _, acct := range cfg.accounts {
			pollCfg := *cfg
			pollCfg.accounts = nil
			acctName := acct
			pollTasks = append(pollTasks, func() {
				qf.wg.Add(1)
				go qf.pollLoop(ctx, &pollCfg, acctName)
			})
		}
	}

	for _, fn := range pollTasks {
		fn()
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

func (qf *QuotaFetcher) pollLoop(ctx context.Context, cfg *quotaProviderConfig, accountName string) {
	defer qf.wg.Done()

	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-qf.stopCh:
			return
		case <-timer.C:
			result := qf.fetchAndUpdate(ctx, cfg, accountName)
			timer.Reset(qf.nextPollDelay(cfg, result))
		}
	}
}

// fetchAndUpdate fetches quota from the provider API and updates the
// billing tracker with the result. Returns the result so callers can
// inspect RetryAfter, StatusCode, and Error for backoff decisions.
func (qf *QuotaFetcher) fetchAndUpdate(ctx context.Context, cfg *quotaProviderConfig, accountName string) QuotaFetchResult {
	pollCtx, cancel := context.WithTimeout(ctx, cfg.timeout+5*time.Second)
	defer cancel()

	result := qf.FetchQuota(pollCtx, cfg.name, accountName, cfg.url, cfg.auth, string(cfg.extraction.Mode), cfg.extraction)
	if result.Error != nil {
		qf.logger.WarnContext(ctx, "quota fetch failed",
			"provider", cfg.name, "account", accountName, "error", result.Error)
		return result
	}

	if result.Info != nil && cfg.tracker != nil {
		if result.Info.BalanceUSD <= 0 {
			cfg.tracker.MarkExhausted(ctx, cfg.name, accountName, "quota exhausted via API polling")
		}
	}
	return result
}

// nextPollDelay computes the delay until the next quota fetch based on
// the previous fetch result:
//   - Success: resets backoff and returns the normal poll interval
//   - 429 with Retry-After: uses the Retry-After value, capped at maxBackoff
//   - Other errors: increments backoff level and returns exponential delay
func (qf *QuotaFetcher) nextPollDelay(cfg *quotaProviderConfig, result QuotaFetchResult) time.Duration {
	if result.Error != nil {
		if result.StatusCode == http.StatusTooManyRequests && result.RetryAfter > 0 {
			return clampDuration(result.RetryAfter, 0, cfg.maxBackoff)
		}
		level, cb := cfg.backoff.Increment(cfg.name)
		if cb != nil {
			cb()
		}
		delay := resilience.ComputeExponentialBackoffWithJitter(level, cfg.pollInterval.Milliseconds())
		return clampDuration(delay, 0, cfg.maxBackoff)
	}
	cfg.backoff.Reset(cfg.name)
	return cfg.pollInterval
}

// clampDuration clamps d to the range [min, max].
func clampDuration(d, minDur, maxDur time.Duration) time.Duration {
	if d < minDur {
		return minDur
	}
	if d > maxDur {
		return maxDur
	}
	return d
}

// getRetryAfter parses the Retry-After header from a 429 response.
// Returns the duration to wait, or 0 if parsing fails.
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
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxQuotaResponseBytes))
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

// toQuotaExtractionConfig converts a config.QuotaExtractionConfig to the
// billing package's QuotaExtractionConfig type.
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

// toProviderExtractionConfig converts a billing.QuotaExtractionConfig to
// the config package's QuotaExtractionConfig type.
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
