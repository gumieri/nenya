package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"git.0ur.uk/nenya/config"
	"git.0ur.uk/nenya/internal/gateway"
	"git.0ur.uk/nenya/internal/infra"
)

func (p *Proxy) handleFiles(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleFilesOrBatches(gw, w, r, "files", keyRef)
}

func (p *Proxy) handleBatches(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	p.handleFilesOrBatches(gw, w, r, "batches", keyRef)
}

// handleFilesOrBatches handles both Files and Batches API endpoints with shared logic.
// The endpoint parameter should be "files" or "batches".
func (p *Proxy) handleFilesOrBatches(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, endpoint string, keyRef string) {
	ctxLogger := gw.Logger.With("operation", endpoint, "api_key", keyRef)
	if !p.isPathSafe(r.URL.Path, "/v1/"+endpoint) {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid path")
		return
	}

	provider := p.validateFilesProvider(gw, w)
	if provider == nil {
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	targetURL, err := p.resolveAPIURL(provider, endpoint, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, err.Error())
		return
	}

	bodyBytes, ok := p.readFilesBody(gw, w, r)
	if !ok {
		return
	}

	ctx, cancel := p.buildFilesContext(r, provider)
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

// isPathSafe checks if the decoded and cleaned path is safe (no path traversal)
// and has the expected prefix.
func (p *Proxy) isPathSafe(pathStr, prefix string) bool {
	decodedPath, err := url.PathUnescape(pathStr)
	if err != nil {
		return false
	}

	cleanPath := path.Clean(decodedPath)
	if strings.Contains(cleanPath, "..") {
		return false
	}

	if !strings.HasPrefix(cleanPath, prefix) {
		return false
	}

	return true
}

// validateFilesProvider returns the configured provider for Files/Batches API,
// or writes an error and returns nil if not available.
func (p *Proxy) validateFilesProvider(gw *gateway.NenyaGateway, w http.ResponseWriter) *config.Provider {
	provider, ok := gw.Providers["openai"]
	if !ok {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindModelNotFound, "No provider configured for files")
		return nil
	}

	if provider.APIKey == "" && provider.AuthStyle != "none" {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInvalidRequest, "Provider not configured")
		return nil
	}

	return provider
}

// resolveAPIURL derives the upstream API URL from the provider config for the given endpoint.
// The endpoint should be "files" or "batches".
func (p *Proxy) resolveAPIURL(provider *config.Provider, endpoint, pathStr, query string) (string, error) {
	if provider.BaseURL == "" {
		return "", fmt.Errorf("provider BaseURL is empty")
	}

	basePath := strings.TrimSuffix(provider.BaseURL, "/") + "/" + endpoint

	subPath := strings.TrimPrefix(pathStr, "/v1/"+endpoint)
	target := basePath + subPath
	if query != "" {
		target += "?" + query
	}

	return target, nil
}

// readFilesBody reads the full request body, enforcing MaxBodyBytes.
// Returns the body bytes and true on success, or writes an error and returns false.
// For GET/DELETE/HEAD methods, returns nil and true without reading.
func (p *Proxy) readFilesBody(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) ([]byte, bool) {
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

	if len(bodyBytes) == 0 {
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Empty request body")
		return nil, false
	}

	return bodyBytes, true
}

// buildFilesContext derives a request-scoped context with the provider's timeout.
// Always returns a valid cancel func (possibly a no-op) for safe deferring.
func (p *Proxy) buildFilesContext(r *http.Request, provider *config.Provider) (context.Context, func()) {
	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(provider.TimeoutSeconds)*time.Second)
		return timeoutCtx, cancel
	}
	return ctx, func() {}
}
