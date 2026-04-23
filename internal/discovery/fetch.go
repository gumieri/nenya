package discovery

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"nenya/internal/adapter"
	"nenya/internal/config"
)

const (
	fetchTimeout    = 10 * time.Second
	maxModelsBody   = 10 << 20 // 10 MB — models list responses are small
	maxModelsPerSrc = 5000
	maxIdleConns    = 20
)

type DiscoveryFetcher struct {
	client *http.Client
}

func NewDiscoveryFetcher() *DiscoveryFetcher {
	return &DiscoveryFetcher{
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
		if p.APIKey == "" && p.AuthStyle != "none" {
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
			logger.Warn("model discovery failed", "err", result.err)
			continue
		}
		for _, m := range result.models {
			catalog.Add(m)
		}
		logger.Info("discovered models", "provider", result.provider, "count", len(result.models))
	}

	catalog.UpdateFetchedAt(time.Now())
	return catalog
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

	if err = injectAuth(req, providerName, provider); err != nil {
		return nil, fmt.Errorf("discovery auth: %w", err)
	}

	resp, err := df.client.Do(req)
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

	if len(models) > maxModelsPerSrc {
		models = models[:maxModelsPerSrc]
		logger.Warn("truncated models list", "provider", providerName, "limit", maxModelsPerSrc)
	}

	return models, nil
}

func injectAuth(req *http.Request, providerName string, provider *config.Provider) error {
	a := adapter.ForProviderWithAuth(providerName, provider.AuthStyle)
	if a != nil {
		return a.InjectAuth(req, provider.APIKey)
	}
	return nil
}
