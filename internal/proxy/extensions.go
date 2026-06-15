package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// endpointConfig maps an API endpoint to its default provider and URL path.
type endpointConfig struct {
	ProviderName string
	APIPath      string
}

var endpointDefaults = map[string]endpointConfig{
	"images/generations":   {ProviderName: "openai", APIPath: "images/generations"},
	"audio/transcriptions": {ProviderName: "openai", APIPath: "audio/transcriptions"},
	"audio/speech":         {ProviderName: "openai", APIPath: "audio/speech"},
	"moderations":          {ProviderName: "openai", APIPath: "moderations"},
	"rerank":               {ProviderName: "cohere", APIPath: "rerank"},
	"a2a":                  {ProviderName: "gemini", APIPath: "a2a"},
}

// handleExtensionEndpoint is the shared handler for extension API endpoints.
// It validates path safety, selects a provider with the preferred name,
// builds the upstream URL, reads the request body, executes with retry, and copies the response.
func (p *Proxy) handleExtensionEndpoint(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef, endpoint string) {
	ctxLogger := gw.Logger.With("operation", endpoint, "api_key", keyRef)
	cfg, ok := endpointDefaults[endpoint]
	if !ok {
		ctxLogger.Error("unknown extension endpoint")
		writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "Unknown endpoint")
		return
	}

	if !p.isPathSafe(r.URL.Path, "/v1/"+endpoint) {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid path")
		return
	}

	provider := p.selectExtensionProvider(gw, cfg.ProviderName)
	if provider == nil {
		ctxLogger.Error("no provider available", "preferred", cfg.ProviderName)
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindModelNotFound, "No provider available for endpoint")
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		gw.Metrics.RecordRateLimitRejected(endpoint)
		ctxLogger.Warn("rate limit exceeded", "provider", provider.Name)
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	targetURL, err := p.resolveExtensionAPIURL(provider, cfg, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, err.Error())
		return
	}

	bodyBytes, ok := p.readExtensionBody(gw, w, r)
	if !ok {
		return
	}

	ctx, cancel := p.buildExtensionContext(r, provider)
	defer cancel()

	maxAttempts := provider.MaxRetryAttempts
	if maxAttempts <= 0 {
		maxAttempts = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}

	contentType := r.Header.Get("Content-Type")
	ctxLogger = ctxLogger.With("provider", provider.Name)

	resp, err := p.doUpstreamRoundTrip(ctx, gw, r.Method, targetURL, bodyBytes, provider.Name, "", r.Header, contentType, maxAttempts)

	if err != nil {
		ctxLogger.Error("upstream request failed", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindProviderError, "Upstream provider error")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	gw.Stats.RecordRequest("proxy:"+provider.Name, 0)
	gw.Metrics.RecordUpstreamRequest("proxy:"+provider.Name, "", provider.Name)

	writeUpstreamResponse(ctx, w, resp, ctxLogger)
}

// selectExtensionProvider returns the preferred provider by name, falling back
// to the first available provider with an API key.
func (p *Proxy) selectExtensionProvider(gw *gateway.NenyaGateway, preferredName string) *config.Provider {
	if preferred, ok := gw.Providers[preferredName]; ok {
		if preferred.APIKey != "" || preferred.AuthStyle == "none" {
			return preferred
		}
	}
	for _, pr := range gw.Providers {
		if pr.APIKey != "" || pr.AuthStyle == "none" {
			return pr
		}
	}
	return nil
}

// resolveExtensionAPIURL builds the upstream URL for an extension endpoint.
// It checks provider.FormatURLs for custom endpoint URLs first, then falls back
// to the provider's BaseURL + the default API path.
func (p *Proxy) resolveExtensionAPIURL(provider *config.Provider, cfg endpointConfig, pathStr, query string) (string, error) {
	if provider.BaseURL == "" {
		return "", fmt.Errorf("provider BaseURL is empty")
	}

	subPath := strings.TrimPrefix(pathStr, "/v1/"+cfg.APIPath)

	target := p.lookupFormatURL(provider, cfg.APIPath, subPath)
	if target == "" {
		basePath := strings.TrimSuffix(provider.BaseURL, "/")
		target = basePath + "/" + cfg.APIPath + subPath
	}

	if query != "" {
		target += "?" + query
	}

	return target, nil
}

// lookupFormatURL checks the provider's FormatURLs map for a custom endpoint URL
// and appends the subPath. Returns empty string if not found.
func (p *Proxy) lookupFormatURL(provider *config.Provider, apiPath, subPath string) string {
	if provider.FormatURLs == nil {
		return ""
	}
	customURL, ok := provider.FormatURLs[apiPath]
	if !ok {
		return ""
	}
	return strings.TrimSuffix(customURL, "/") + subPath
}

// readExtensionBody reads the full request body, enforcing MaxBodyBytes.
func (p *Proxy) readExtensionBody(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method == http.MethodGet || r.Method == http.MethodDelete || r.Method == http.MethodHead {
		return nil, true
	}

	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		writeStructuredError(w, http.StatusRequestEntityTooLarge, infra.ErrorKindPayloadTooLarge, "Payload too large or malformed")
		return nil, false
	}

	if len(bodyBytes) == 0 && r.Method == http.MethodPost {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Empty request body")
		return nil, false
	}

	return bodyBytes, true
}

// buildExtensionContext derives a request-scoped context with the provider's timeout.
func (p *Proxy) buildExtensionContext(r *http.Request, provider *config.Provider) (context.Context, func()) {
	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(provider.TimeoutSeconds)*time.Second)
		return timeoutCtx, cancel
	}
	return ctx, func() {}
}

// handleImages handles POST /v1/images/generations.
func (p *Proxy) handleImages(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleExtensionEndpoint(gw, w, r, keyRef, "images/generations")
}

// handleAudioTranscriptions handles POST /v1/audio/transcriptions.
// The incoming Content-Type (typically multipart/form-data) is preserved so
// the upstream provider receives the full form, including the audio file.
func (p *Proxy) handleAudioTranscriptions(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleExtensionEndpoint(gw, w, r, keyRef, "audio/transcriptions")
}

// handleAudioSpeech handles POST /v1/audio/speech.
func (p *Proxy) handleAudioSpeech(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleExtensionEndpoint(gw, w, r, keyRef, "audio/speech")
}

// handleModerations handles POST /v1/moderations.
func (p *Proxy) handleModerations(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleExtensionEndpoint(gw, w, r, keyRef, "moderations")
}

// handleRerank handles POST /v1/rerank.
func (p *Proxy) handleRerank(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleExtensionEndpoint(gw, w, r, keyRef, "rerank")
}

// handleA2A handles POST /v1/a2a (Agent-to-Agent protocol).
func (p *Proxy) handleA2A(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleExtensionEndpoint(gw, w, r, keyRef, "a2a")
}
