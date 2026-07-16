package discovery

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/util"
)

const (
	fetchTimeout    = 10 * time.Second
	maxModelsBody   = 10 << 20 // 10 MB — models list responses are small
	maxModelsPerSrc = 5000
	maxIdleConns    = 20
)

type DiscoveryFetcher struct {
	client      *http.Client
	metrics     *infra.Metrics
	maxAttempts int
	keyProvider ProviderKeyProvider
}

// ProviderKeyProvider is a callback to retrieve provider API keys.
// This allows the fetcher to obtain keys from secure memory without
// depending on the gateway directly.
type ProviderKeyProvider func(providerName string) ([]byte, bool)

// NewDiscoveryFetcher creates a DiscoveryFetcher that fetches model catalogs
// from upstream providers. maxAttempts must be >= 1 (use EffectiveMaxRetryAttempts
// from GovernanceConfig to ensure a minimum fallback of 3).
func NewDiscoveryFetcher(maxAttempts int) *DiscoveryFetcher {
	return &DiscoveryFetcher{
		maxAttempts: maxAttempts,
		client: &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout:   5 * time.Second,
					KeepAlive: 5 * time.Second,
				}).DialContext,
				TLSHandshakeTimeout:   5 * time.Second,
				ResponseHeaderTimeout: fetchTimeout,
				IdleConnTimeout:       10 * time.Second,
				MaxIdleConns:          maxIdleConns,
				MaxIdleConnsPerHost:   2,
			},
		},
	}
}

func (df *DiscoveryFetcher) WithMetrics(m *infra.Metrics) *DiscoveryFetcher {
	df.metrics = m
	return df
}

func (df *DiscoveryFetcher) WithKeyProvider(kp ProviderKeyProvider) *DiscoveryFetcher {
	df.keyProvider = kp
	return df
}

func (df *DiscoveryFetcher) FetchAll(ctx context.Context, providers map[string]*config.Provider, logger *slog.Logger) *ModelCatalog {
	catalog := NewModelCatalog()

	type fetchResult struct {
		provider string
		models   []DiscoveredModel
		err      error
	}

	results := make(chan fetchResult, len(providers))

	var wg sync.WaitGroup
	for name, p := range providers {
		hasKey := false
		if df.keyProvider != nil {
			if keyBytes, ok := df.keyProvider(name); ok {
				hasKey = len(keyBytes) > 0
			}
		}
		if !hasKey && p.AuthStyle != "none" {
			logger.Debug("skipping model discovery: no API key", "provider", name)
			continue
		}
		wg.Add(1)
		go func(providerName string, provider *config.Provider) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					logger.Error("panic in model discovery goroutine", "provider", providerName, "err", r)
				}
			}()
			models, err := df.fetchProviderModels(ctx, providerName, provider, logger)
			results <- fetchResult{
				provider: providerName,
				models:   models,
				err:      err,
			}
		}(name, p)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	for result := range results {
		if result.err != nil {
			logger.Warn("model discovery failed", "provider", result.provider, "err", result.err)
		}
		if df.metrics != nil {
			df.metrics.RecordModelDiscovery(result.provider, result.err)
		}
		if result.err != nil {
			continue
		}
		for _, m := range result.models {
			catalog.Add(m)
		}
		logger.Info("discovered models", "provider", result.provider, "count", len(result.models))
	}

	logDiscoveredModels(ctx, logger, catalog)

	catalog.UpdateFetchedAt(time.Now())
	return catalog
}

func logDiscoveredModels(ctx context.Context, logger *slog.Logger, catalog *ModelCatalog) {
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	var providerSummary []string
	for provider, ids := range catalog.providers {
		providerSummary = append(providerSummary, fmt.Sprintf("%s:%d", provider, len(ids)))
	}
	sort.Strings(providerSummary)

	var allModels []string
	for _, m := range catalog.AllModels() {
		meta := buildCapabilityMetadata(m.Metadata)
		pricing := "false"
		if m.Pricing != nil {
			pricing = fmt.Sprintf("%.2f/%.2f", m.Pricing.InputCostPer1M, m.Pricing.OutputCostPer1M)
		}
		allModels = append(allModels, fmt.Sprintf("%s/%s ctx=%d out=%d caps=%s pricing=%s",
			m.Provider, m.ID, m.MaxContext, m.MaxOutput, meta, pricing))
	}
	sort.Strings(allModels)
	logger.Debug("discovery catalog", "providers", providerSummary, "models", allModels)
}

// buildCapabilityMetadata constructs a comma-separated string of model capabilities
// from the metadata. Returns empty string if metadata is nil.
func buildCapabilityMetadata(meta *ModelMetadata) string {
	if meta == nil {
		return ""
	}
	var caps []string
	if meta.SupportsVision {
		caps = append(caps, "vision")
	}
	if meta.SupportsToolCalls {
		caps = append(caps, "tools")
	}
	if meta.SupportsReasoning {
		caps = append(caps, "reasoning")
	}
	if meta.SupportsContentArrays {
		caps = append(caps, "content_arrays")
	}
	if meta.SupportsStreamOptions {
		caps = append(caps, "stream_options")
	}
	if meta.SupportsAutoToolChoice {
		caps = append(caps, "auto_tool_choice")
	}
	if meta.SupportsAudio {
		caps = append(caps, "audio")
	}
	return strings.Join(caps, ",")
}

func (df *DiscoveryFetcher) enrichOllama(ctx context.Context, baseURL string, models []DiscoveredModel, logger *slog.Logger) []DiscoveredModel {
	start := time.Now()
	enriched, err := enrichOllamaModels(ctx, baseURL, models, df.client, logger)
	if err != nil {
		if df.metrics != nil {
			df.metrics.RecordOllamaEnrichment("failed")
		}
		logger.Warn("Ollama model enrichment failed, using basic model list", "err", err)
		return models
	}
	if df.metrics != nil {
		df.metrics.RecordOllamaEnrichment("success")
		df.metrics.RecordOllamaEnrichmentDuration(time.Since(start))
	}
	return enriched
}

func (df *DiscoveryFetcher) fetchProviderModels(ctx context.Context, providerName string, provider *config.Provider, logger *slog.Logger) ([]DiscoveredModel, error) {
	endpoint := GetModelsEndpoint(provider.URL, providerName)
	if endpoint == "" {
		logger.Debug("no models endpoint for provider", "provider", providerName)
		return nil, nil
	}

	providerCtx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(providerCtx, "GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("discovery request: %w", err)
	}

	if err = injectAuth(req, providerName, provider, df); err != nil {
		return nil, fmt.Errorf("discovery auth: %w", err)
	}

	resp, err := df.fetchWithRetry(req, provider, providerCtx)
	if err != nil {
		return nil, fmt.Errorf("discovery fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		logger.Debug("non-200 from models endpoint", "provider", providerName, "status", resp.StatusCode)
		return nil, nil
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		logger.Debug("unexpected content-type from models endpoint", "provider", providerName, "content_type", ct)
		return nil, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, fmt.Errorf("discovery read body: %w", err)
	}

	models, err := ParseModelsResponse(body, providerName, logger)
	if err != nil {
		return nil, fmt.Errorf("discovery parse: %w", err)
	}

	if strings.EqualFold(providerName, "ollama") {
		models = df.enrichOllama(ctx, provider.URL, models, logger)
	}

	if len(models) > maxModelsPerSrc {
		models = models[:maxModelsPerSrc]
		logger.Warn("truncated models list", "provider", providerName, "limit", maxModelsPerSrc)
	}

	return models, nil
}

// fetchWithRetry executes the HTTP request with retries and records retry metrics.
func (df *DiscoveryFetcher) fetchWithRetry(req *http.Request, provider *config.Provider, ctx context.Context) (*http.Response, error) {
	maxAttempts := df.maxAttempts
	if provider.MaxRetryAttempts > 0 {
		maxAttempts = provider.MaxRetryAttempts
	}

	providerName := provider.Name
	attempt := 0
	return util.DoWithRetryResp(ctx, maxAttempts, func() (*http.Response, error) {
		attempt++
		resp, fetchErr := df.client.Do(req)
		if fetchErr != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			if attempt > 1 && df.metrics != nil {
				df.metrics.RecordRetry("model_discovery", providerName, fetchErr)
			}
			return nil, fetchErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			if attempt > 1 && df.metrics != nil {
				df.metrics.RecordRetry("model_discovery", providerName, fmt.Errorf("upstream error: %d", resp.StatusCode))
			}
			return nil, fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return resp, nil
	})
}

func injectAuth(req *http.Request, providerName string, provider *config.Provider, df *DiscoveryFetcher) error {
	a := adapter.ForProviderWithAuth(providerName, provider.AuthStyle)
	if a == nil {
		return nil
	}
	if df != nil && df.keyProvider != nil {
		if keyBytes, ok := df.keyProvider(providerName); ok {
			return a.InjectAuth(req, string(keyBytes))
		}
	}
	return a.InjectAuth(req, provider.APIKey)
}

const ollamaEnrichConcurrency = 5
const ollamaShowTimeout = 10 * time.Second

type ollamaShowResult struct {
	modelName    string
	maxCtx       int
	caps         []Capability
	serviceKinds []string
	err          error
}

func enrichSingleOllamaModel(ctx context.Context, modelName, showEndpoint string, client *http.Client) ollamaShowResult {
	res := ollamaShowResult{modelName: modelName}
	body, _ := json.Marshal(map[string]string{"name": modelName})
	showCtx, cancel := context.WithTimeout(ctx, ollamaShowTimeout)
	defer cancel()
	req, reqErr := http.NewRequestWithContext(showCtx, http.MethodPost, showEndpoint, bytes.NewReader(body))
	if reqErr != nil {
		res.err = reqErr
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	resp, doErr := client.Do(req)
	if doErr != nil {
		res.err = doErr
		return res
	}
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	_ = resp.Body.Close()
	if readErr != nil {
		res.err = readErr
		return res
	}
	if resp.StatusCode != http.StatusOK {
		res.err = fmt.Errorf("ollama /api/show returned status %d", resp.StatusCode)
		return res
	}
	var showResp OllamaShowResponse
	if jsonErr := json.Unmarshal(respBody, &showResp); jsonErr != nil {
		res.err = fmt.Errorf("ollama /api/show parse: %w", jsonErr)
		return res
	}
	res.maxCtx = extractContextLength(showResp.ModelInfo)
	res.caps = mapOllamaCaps(showResp.Capabilities)
	hasEmbeddings := extractHasEmbeddings(showResp.ModelInfo)
	res.serviceKinds = mapOllamaServiceKinds(showResp.Capabilities, hasEmbeddings)
	return res
}

func enrichOllamaModels(ctx context.Context, baseURL string, models []DiscoveredModel, client *http.Client, logger *slog.Logger) ([]DiscoveredModel, error) {
	showEndpoint := GetOllamaShowEndpoint(baseURL)
	if showEndpoint == "" {
		logger.Debug("cannot build /api/show endpoint", "url", baseURL)
		return nil, fmt.Errorf("no /api/show endpoint for Ollama URL: %s", baseURL)
	}

	type job struct {
		index     int
		modelName string
	}

	jobs := make(chan job, len(models))
	resultsCh := make(chan ollamaShowResult, len(models))

	var wg sync.WaitGroup
	for w := 0; w < ollamaEnrichConcurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := enrichSingleOllamaModel(ctx, j.modelName, showEndpoint, client)
				resultsCh <- res
			}
		}()
	}

	for i, m := range models {
		jobs <- job{index: i, modelName: m.ID}
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(resultsCh)
	}()

	modelMap := make(map[string]*DiscoveredModel)
	for i := range models {
		modelMap[models[i].ID] = &models[i]
	}

	for res := range resultsCh {
		m, ok := modelMap[res.modelName]
		if !ok {
			continue
		}
		if res.err != nil {
			logger.Debug("ollama model enrichment failed, using zero values",
				"model", res.modelName, "err", res.err)
			continue
		}
		if res.maxCtx > 0 {
			m.MaxContext = res.maxCtx
		}
		if len(res.caps) > 0 {
			if m.Metadata == nil {
				m.Metadata = &ModelMetadata{}
			}
			applyCapabilities(m.Metadata, res.caps)
		}
		if len(res.serviceKinds) > 0 {
			m.ServiceKinds = res.serviceKinds
		}
		logger.Debug("ollama model enriched",
			"model", res.modelName,
			"max_context", res.maxCtx,
			"caps", res.caps,
			"service_kinds", res.serviceKinds)
	}

	return models, nil
}
