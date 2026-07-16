package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

func (p *Proxy) handleEmbeddings(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		gw.Logger.Error("failed to read embeddings request body", "err", err)
		writeStructuredError(w, http.StatusRequestEntityTooLarge, infra.ErrorKindPayloadTooLarge, "Payload too large or malformed")
		return
	}

	var payload map[string]interface{}
	if err = json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse embeddings JSON")
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid JSON payload")
		return
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model in embeddings request")
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, `Missing or empty "model" field`)
		return
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length in embeddings request", "length", len(modelName))
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Model name too long")
		return
	}

	matches := routing.ResolveProviders(modelName, gw.Providers, gw.ModelCatalog)
	if len(matches) == 0 {
		gw.Logger.Warn("no provider for embeddings model", "model", modelName)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, util.ErrNoProvider)
		return
	}

	provider, ok := gw.Providers[matches[0].Provider]
	if !ok {
		gw.Logger.Error("provider not found in providers map", "provider", matches[0].Provider)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, util.ErrNoProvider)
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	tokenCount := countEmbeddingInputTokens(gw, payload)

	gw.Stats.RecordRequest(modelName, tokenCount)
	gw.Metrics.RecordUpstreamRequest(modelName, "", provider.Name)

	embeddingURL := provider.BaseURL + "/embeddings"
	if embeddingURL == "/embeddings" {
		gw.Logger.Warn("provider BaseURL is empty, cannot derive embeddings endpoint",
			"provider", provider.Name)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Provider does not support embeddings")
		return
	}

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	maxAttempts := provider.MaxRetryAttempts
	if maxAttempts <= 0 {
		maxAttempts = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}

	p.forwardEmbeddingsRequest(gw, w, ctx, http.MethodPost, embeddingURL, bodyBytes, provider.Name, modelName, r.Header, maxAttempts, keyRef)
}

func countEmbeddingInputTokens(gw *gateway.NenyaGateway, payload map[string]interface{}) int {
	inputRaw, ok := payload["input"]
	if !ok {
		return 0
	}

	var totalTokens int
	switch input := inputRaw.(type) {
	case string:
		totalTokens = gw.CountTokens(input)
	case []interface{}:
		for _, item := range input {
			if s, ok := item.(string); ok {
				totalTokens += gw.CountTokens(s)
			}
		}
	}
	return totalTokens
}

func (p *Proxy) forwardEmbeddingsRequest(gw *gateway.NenyaGateway, w http.ResponseWriter, ctx context.Context, method, url string, bodyBytes []byte, providerName, modelName string, srcHeaders http.Header, maxAttempts int, keyRef string) {
	req, err := p.buildUpstreamRequest(gw, ctx, method, url, bodyBytes, providerName, modelName, "", srcHeaders)
	if err != nil {
		ctxLogger := gw.Logger.With("operation", "forward", "api_key", keyRef)
		ctxLogger.Error("failed to create embeddings upstream request", "err", err)
		writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "Internal Server Error")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := util.DoWithRetryResp(ctx, maxAttempts, func() (*http.Response, error) {
		r, fetchErr := gw.Client.Do(req)
		if fetchErr != nil {
			if r != nil {
				_ = r.Body.Close()
			}
			return nil, fetchErr
		}
		if r.StatusCode >= 500 {
			_ = r.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", r.StatusCode)
		}
		return r, nil
	})
	if err != nil {
		ctxLogger := gw.Logger.With("operation", "forward", "api_key", keyRef, "provider", providerName)
		ctxLogger.Error("embeddings upstream request failed", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	ctxLogger := gw.Logger.With("operation", "forward", "api_key", keyRef, "provider", providerName)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingsResponseBytes))
	if err != nil {
		ctxLogger.Error("failed to read embeddings response body", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Failed to read upstream response")
		return
	}

	recordEmbeddingUsage(gw, respBody, providerName)

	writeUpstreamBytesResponse(ctx, w, resp, respBody, ctxLogger)
}

func recordEmbeddingUsage(gw *gateway.NenyaGateway, respBody []byte, providerName string) {
	var responseMap map[string]interface{}
	if err := json.Unmarshal(respBody, &responseMap); err != nil {
		return
	}
	recordUsageFromMap(gw, responseMap, "", providerName)
}
