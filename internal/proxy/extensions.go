package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/routing"
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
	cfg, ok := endpointDefaults[endpoint]
	if !ok {
		gw.Logger.Error("unknown extension endpoint", "endpoint", endpoint)
		http.Error(w, "Unknown endpoint", http.StatusInternalServerError)
		return
	}

	if !p.isPathSafe(r.URL.Path, "/v1/"+endpoint) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	provider := p.selectExtensionProvider(gw, cfg.ProviderName)
	if provider == nil {
		gw.Logger.Error("no provider available for endpoint",
			"endpoint", endpoint, "preferred", cfg.ProviderName)
		http.Error(w, "No provider available for endpoint", http.StatusServiceUnavailable)
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		gw.Metrics.RecordRateLimitRejected(endpoint)
		gw.Logger.Warn("rate limit exceeded for endpoint",
			"endpoint", endpoint, "provider", provider.Name)
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	targetURL, err := p.resolveExtensionAPIURL(provider, cfg, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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
	ctxLogger := gw.Logger.With("operation", endpoint, "provider", provider.Name, "api_key", keyRef)

	resp, err := p.doUpstreamRoundTrip(ctx, gw, r.Method, targetURL, bodyBytes, provider.Name, r.Header, contentType, maxAttempts)

	if err != nil {
		ctxLogger.Error("upstream request failed", "endpoint", endpoint, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	gw.Stats.RecordRequest("proxy:"+provider.Name, 0)
	gw.Metrics.RecordUpstreamRequest("proxy:"+provider.Name, "", provider.Name)

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := copyStream(ctx, w, resp.Body, nil); err != nil {
		ctxLogger.Debug("response copy ended", "err", err)
	}
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
		gw.Logger.Error("failed to read extension request body", "err", readErr)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return nil, false
	}

	if len(bodyBytes) == 0 && r.Method == http.MethodPost {
		http.Error(w, "Empty request body", http.StatusBadRequest)
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
