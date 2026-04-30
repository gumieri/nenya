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

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/routing"
	"nenya/internal/util"
)

func (p *Proxy) handleFiles(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.handleFilesOrBatches(gw, w, r, "files")
}

func (p *Proxy) handleBatches(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.handleFilesOrBatches(gw, w, r, "batches")
}

// handleFilesOrBatches handles both Files and Batches API endpoints with shared logic.
// The endpoint parameter should be "files" or "batches".
func (p *Proxy) handleFilesOrBatches(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, endpoint string) {
	if !p.isPathSafe(r.URL.Path, "/v1/"+endpoint) {
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	provider := p.validateFilesProvider(gw, w)
	if provider == nil {
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	targetURL, err := p.resolveAPIURL(provider, endpoint, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
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

	var resp *http.Response
	err = util.DoWithRetry(ctx, maxAttempts, func() error {
		upstreamReq, reqErr := p.buildUpstreamRequest(gw, ctx, r.Method, targetURL, bodyBytes, provider.Name, r.Header)
		if reqErr != nil {
			return reqErr
		}
		if contentType != "" {
			upstreamReq.Header.Set("Content-Type", contentType)
		}

		var fetchErr error
		resp, fetchErr = gw.Client.Do(upstreamReq)
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
		gw.Logger.Error("files/batches upstream request failed", "provider", provider.Name, "endpoint", endpoint, "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	gw.Stats.RecordRequest("proxy:"+provider.Name, 0)
	if gw.Metrics != nil {
		gw.Metrics.RecordUpstreamRequest("proxy:"+provider.Name, "", provider.Name)
	}

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	if _, err := copyStream(ctx, w, resp.Body, nil); err != nil {
		gw.Logger.Debug("files/batches response copy ended", "err", err)
	}
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
		http.Error(w, "No provider configured for files", http.StatusServiceUnavailable)
		return nil
	}

	if provider.APIKey == "" && provider.AuthStyle != "none" {
		http.Error(w, "Provider not configured", http.StatusServiceUnavailable)
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
		gw.Logger.Error("failed to read files request body", "err", readErr)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return nil, false
	}

	if len(bodyBytes) == 0 {
		http.Error(w, "Empty request body", http.StatusBadRequest)
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
