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

// systemOneURLCacheKey is the provider-level format key holding the System One
// decision endpoint (TypeSafe Jev). Providers that serve System One models
// declare it under FormatURLs; when absent, the client-supplied model name is
// resolved through the catalog to a provider that does.
const systemOneURLCacheKey = "systemone"

// handleSystemOne proxies System One decision requests (TypeSafe Jev) to the
// provider's decision endpoint. The body is forwarded verbatim (it is not an
// OpenAI chat body: it carries `state` and typed `questions`), the provider
// key is injected, and the non-streaming JSON response is relayed. Nenya
// client authentication and RBAC are enforced by the route chain; usage is
// recorded from the response's `usage` object.
func (p *Proxy) handleSystemOne(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}

	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, herr := readRequestBody(r, gw, "failed to read system one request body")
	if renderBodyReadError(w, herr) {
		return
	}

	modelName, herr := parseSystemOneModel(gw, bodyBytes)
	if herr != nil {
		writeStructuredError(w, herr.Code, herr.Kind, herr.Message)
		return
	}

	provider, herr := resolveSystemOneProvider(gw, modelName)
	if herr != nil {
		writeStructuredError(w, herr.Code, herr.Kind, herr.Message)
		return
	}

	endpoint := provider.FormatURLs[systemOneURLCacheKey]
	if endpoint == "" {
		gw.Logger.Warn("provider has no system one endpoint configured", "provider", provider.Name, "model", modelName)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, "Provider does not support System One decision requests")
		return
	}

	if !gw.RateLimiter.Check(provider.Name, provider.BaseURL, 0) {
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	tokenCount := gw.CountTokens(string(bodyBytes))
	gw.Stats.RecordRequest(modelName, tokenCount)
	gw.Metrics.RecordUpstreamRequest(modelName, "", provider.Name)
	gw.Metrics.RecordTokens("client", modelName, "", provider.Name, tokenCount)

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

	p.forwardSystemOneRequest(gw, w, ctx, endpoint, bodyBytes, provider.Name, modelName, r.Header, maxAttempts, keyRef)
}

// parseSystemOneModel extracts and validates the "model" field from a System
// One request body.
func parseSystemOneModel(gw *gateway.NenyaGateway, bodyBytes []byte) (string, *httpError) {
	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse system one JSON")
		return "", &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindInvalidRequest, Message: "Invalid JSON payload"}
	}
	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model in system one request")
		return "", &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindInvalidRequest, Message: `Missing or empty "model" field`}
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length in system one request", "length", len(modelName))
		return "", &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindInvalidRequest, Message: "Model name too long"}
	}
	return modelName, nil
}

// resolveSystemOneProvider resolves the provider for a System One model. A
// non-chat model (which is what System One models are) is resolved even though
// it is absent from the merged chat catalog: the request's model field selects
// the provider directly when exactly one configured provider declares the
// endpoint, and otherwise falls back to catalog resolution.
func resolveSystemOneProvider(gw *gateway.NenyaGateway, modelName string) (*config.Provider, *httpError) {
	if matches := routing.ResolveProviders(modelName, gw.Providers, gw.ModelCatalog); len(matches) > 0 {
		if p, ok := gw.Providers[matches[0].Provider]; ok {
			return p, nil
		}
	}

	// System One models are excluded from the chat catalog. Resolve by the
	// provider that actually serves them as non-chat models.
	owner, owners := systemOneProviderFor(gw, modelName)
	switch len(owners) {
	case 0:
		gw.Logger.Warn("no provider for system one model", "model", modelName)
		return nil, &httpError{Code: http.StatusBadRequest, Kind: infra.ErrorKindModelNotFound, Message: util.ErrNoProvider}
	case 1:
		return owners[0], nil
	default:
		// Ambiguous: prefer one whose declared pattern is the most specific,
		// which for the built-in zen is the only realistic case today.
		gw.Logger.Warn("multiple providers serve system one model; using first", "model", modelName, "provider", owner)
		return owners[0], nil
	}
}

// systemOneProviderFor returns the name and Providers that classify the model
// as non-chat and declare a System One endpoint.
func systemOneProviderFor(gw *gateway.NenyaGateway, modelName string) (string, []*config.Provider) {
	var names []string
	var owners []*config.Provider
	for name, p := range gw.Providers {
		if p == nil || !p.IsNonChatModel(modelName) {
			continue
		}
		if p.FormatURLs[systemOneURLCacheKey] == "" {
			continue
		}
		names = append(names, name)
		owners = append(owners, p)
	}
	if len(names) == 0 {
		return "", nil
	}
	return names[0], owners
}

// forwardSystemOneRequest performs the retried upstream round-trip and relays
// the non-streaming JSON response, recording usage from its `usage` object.
func (p *Proxy) forwardSystemOneRequest(gw *gateway.NenyaGateway, w http.ResponseWriter, ctx context.Context, endpoint string, bodyBytes []byte, providerName, modelName string, srcHeaders http.Header, maxAttempts int, keyRef string) {
	req, err := p.buildUpstreamRequest(gw, ctx, http.MethodPost, endpoint, bodyBytes, providerName, modelName, "", srcHeaders)
	if err != nil {
		gw.Logger.Error("failed to create system one upstream request", "err", err, "api_key", keyRef)
		writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "Internal Server Error")
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := util.DoWithRetryResp(ctx, maxAttempts, func() (*http.Response, error) {
		r, fetchErr := gw.ClientFor(providerName).Do(req)
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
		gw.Logger.Error("system one upstream request failed", "err", err, "provider", providerName)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	ctxLogger := gw.Logger.With("operation", "systemone", "api_key", keyRef, "provider", providerName)
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxEmbeddingsResponseBytes))
	if err != nil {
		ctxLogger.Error("failed to read system one response body", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Failed to read upstream response")
		return
	}

	gw.Stats.RecordOutput(modelName, systemOneOutputTokens(respBody))
	if gw.Metrics != nil {
		gw.Metrics.RecordUpstreamRequest(modelName, "", providerName)
		if input, output, ok := systemOneTokenCounts(respBody); ok {
			gw.Metrics.RecordTokens("input", modelName, "", providerName, input)
			gw.Metrics.RecordTokens("output", modelName, "", providerName, output)
		}
	}

	writeUpstreamBytesResponse(ctx, w, resp, respBody, ctxLogger)
}

// systemOneTokenCounts extracts the input/output token counts from a System
// One response's usage object.
func systemOneTokenCounts(respBody []byte) (input, output int, ok bool) {
	var payload struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return 0, 0, false
	}
	return payload.Usage.InputTokens, payload.Usage.OutputTokens, true
}

// systemOneOutputTokens returns the output token count, or 0 when absent.
func systemOneOutputTokens(respBody []byte) int {
	_, output, _ := systemOneTokenCounts(respBody)
	return output
}
