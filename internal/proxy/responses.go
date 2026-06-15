package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

func (p *Proxy) authorizeResponsesAgent(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey, bodyBytes []byte) (string, *httpError) {
	var modelName string
	if len(bodyBytes) > 0 {
		var payload map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &payload); err == nil {
			if m, ok := payload["model"].(string); ok {
				modelName = m
			}
		}
	}

	if modelName != "" && apiKey != nil && !auth.AuthorizeAgent(apiKey, modelName) {
		gw.Metrics.IncAuthDenials(apiKey.Name, "agent")
		p.logAuthDenial(gw, apiKey, fmt.Sprintf("agent %s", modelName), r)
		return "", &httpError{http.StatusForbidden, "Forbidden"}
	}
	return modelName, nil
}

func (p *Proxy) handleResponses(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}
	pathSafe := p.isPathSafeResponses(r.URL.Path)
	if !pathSafe {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid path")
		return
	}

	bodyBytes, ok := p.readResponsesBody(gw, w, r)
	if !ok {
		return
	}

	responsesModel, herr := p.authorizeResponsesAgent(gw, w, r, apiKey, bodyBytes)
	if herr != nil {
		return
	}

	provider := p.resolveResponsesProviderFromModel(gw, responsesModel)
	if provider == nil {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindModelNotFound, util.ErrNoProvider)
		return
	}

	targetURL := p.resolveResponsesURL(provider, r.URL.Path, r.URL.RawQuery)
	if targetURL == "" {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Provider does not support responses API")
		return
	}

	ctx, cancel := p.buildResponsesContext(r, provider)
	defer cancel()

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	maxAttempts := provider.MaxRetryAttempts
	if maxAttempts <= 0 {
		maxAttempts = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}

	gw.Stats.RecordRequest(responsesModel, 0)
	gw.Metrics.RecordUpstreamRequest(responsesModel, "", provider.Name)

	ctxLogger := gw.Logger.With("operation", "responses", "provider", provider.Name, "api_key", keyRef)

	var resp *http.Response
	err := util.DoWithRetry(ctx, maxAttempts, func() error {
		req, reqErr := p.buildUpstreamRequest(gw, ctx, r.Method, targetURL, bodyBytes, provider.Name, responsesModel, "", r.Header)
		if reqErr != nil {
			return reqErr
		}
		if len(bodyBytes) > 0 {
			req.Header.Set("Content-Type", "application/json")
		}

		var fetchErr error
		resp, fetchErr = gw.Client.Do(req)
		if fetchErr != nil {
			return fetchErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return nil
	})

	if err != nil {
		ctxLogger.Error("responses upstream request failed", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	writeUpstreamResponse(ctx, w, resp, ctxLogger)
}

// isPathSafeResponses checks if the decoded and cleaned path is safe (no path
// traversal) and has the expected /v1/responses prefix.
func (p *Proxy) isPathSafeResponses(pathStr string) bool {
	decodedPath, err := url.PathUnescape(pathStr)
	if err != nil {
		return false
	}

	cleanPath := path.Clean(decodedPath)
	if strings.Contains(cleanPath, "..") || strings.Contains(decodedPath, "..") {
		return false
	}

	if !strings.HasPrefix(cleanPath, "/v1/responses") {
		return false
	}
	return true
}

func (p *Proxy) resolveResponsesProviderFromModel(gw *gateway.NenyaGateway, modelName string) *config.Provider {
	if modelName != "" {
		matches := routing.ResolveProviders(modelName, gw.Providers, gw.ModelCatalog)
		if len(matches) > 0 {
			if p, ok := gw.Providers[matches[0].Provider]; ok {
				return p
			}
		}
	}

	return p.getDefaultResponseProvider(gw)
}

func (p *Proxy) readResponsesBody(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		return []byte{}, true
	}

	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		gw.Logger.Error("failed to read responses request body", "err", readErr)
		writeStructuredError(w, http.StatusRequestEntityTooLarge, infra.ErrorKindPayloadTooLarge, "Payload too large or malformed")
		return nil, false
	}

	return bodyBytes, true
}

func (p *Proxy) resolveResponsesURL(provider *config.Provider, pathStr, query string) string {
	// Fallback: BaseURL → trimmed URL → empty (provider must have BaseURL or URL ending with /v1)
	baseURL := strings.TrimSuffix(provider.BaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimSuffix(provider.URL, "/chat/completions")
	}
	if baseURL == provider.URL || baseURL == "" {
		return ""
	}

	subPath := strings.TrimPrefix(pathStr, "/v1/responses")
	var target string
	if strings.HasSuffix(baseURL, "/v1") {
		target = baseURL + "/responses" + subPath
	} else {
		target = baseURL + "/v1/responses" + subPath
	}
	if query != "" {
		target += "?" + query
	}
	return target
}

func (p *Proxy) buildResponsesContext(r *http.Request, provider *config.Provider) (context.Context, func()) {
	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(provider.TimeoutSeconds)*time.Second)
		return timeoutCtx, cancel
	}
	return ctx, func() {}
}

func (p *Proxy) getDefaultResponseProvider(gw *gateway.NenyaGateway) *config.Provider {
	preferred := []string{"deepseek", "openai", "anthropic", "openrouter"}
	for _, name := range preferred {
		if pr, ok := gw.Providers[name]; ok && pr.APIKey != "" {
			return pr
		}
	}

	for _, pr := range gw.Providers {
		if pr.APIKey != "" && pr.AuthStyle != "none" {
			return pr
		}
	}

	for _, pr := range gw.Providers {
		if pr.AuthStyle == "none" {
			return pr
		}
	}
	return nil
}
