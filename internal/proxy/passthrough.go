package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"nenya/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/routing"
	"nenya/internal/util"
)

func (p *Proxy) handlePassthrough(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segments) < 2 {
		gw.Logger.Warn("passthrough: invalid path format", "path", r.URL.Path)
		http.Error(w, "Invalid passthrough path format", http.StatusBadRequest)
		return
	}

	providerName := segments[1]
	if providerName == "" {
		gw.Logger.Warn("passthrough: missing provider name", "path", r.URL.Path)
		http.Error(w, "Missing provider name", http.StatusBadRequest)
		return
	}

	provider, ok := gw.Providers[providerName]
	if !ok {
		gw.Logger.Warn("passthrough: unknown provider", "provider", providerName)
		http.Error(w, "Unknown provider", http.StatusNotFound)
		return
	}

	if provider.APIKey == "" && provider.AuthStyle != "none" {
		gw.Logger.Warn("passthrough: provider not configured", "provider", providerName)
		http.Error(w, "Provider not configured", http.StatusServiceUnavailable)
		return
	}

	if len(segments) < 3 {
		gw.Logger.Warn("passthrough: missing endpoint path", "provider", providerName)
		http.Error(w, "Missing endpoint path", http.StatusBadRequest)
		return
	}

	subPath := path.Join(segments[2:]...)
	if strings.Contains(subPath, "..") {
		gw.Logger.Warn("passthrough: path traversal attempt", "provider", providerName, "path", subPath)
		http.Error(w, "Invalid path", http.StatusBadRequest)
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		gw.Metrics.RecordRateLimitRejected(infra.ExtractHost(provider.BaseURL))
		gw.Logger.Warn("passthrough: rate limit exceeded", "provider", providerName)
		http.Error(w, "Rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	bodyBytes, err := readPassthroughBody(gw, w, r)
	if err != nil {
		gw.Logger.Error("passthrough: failed to read request body", "provider", providerName, "err", err)
		http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
		return
	}

	upstreamURL := buildPassthroughURL(provider, subPath, r.URL.RawQuery)
	ctx, cancel := buildPassthroughContextAndCancel(r.Context(), provider)
	defer cancel()

	ctxLogger := gw.Logger.With(
		"operation", "passthrough",
		"provider", providerName,
		"method", r.Method,
		"path", subPath,
		"api_key", keyRef,
	)

	resp, err := p.executePassthroughUpstream(gw, ctx, r, upstreamURL, bodyBytes, provider)
	if err != nil {
		ctxLogger.Error("upstream request failed", "err", err)
		http.Error(w, "Upstream provider error", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	ctxLogger.Info("upstream response", "status", resp.StatusCode)

	gw.Stats.RecordRequest("proxy:"+providerName, 0)
	gw.Metrics.RecordUpstreamRequest("proxy:"+providerName, "", providerName)

	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		p.pipeSSE(ctx, ctxLogger, resp.Body, w)
	} else {
		if _, err := copyStream(ctx, w, resp.Body, nil); err != nil {
			ctxLogger.Debug("response copy ended", "err", err)
		}
	}
}

// executePassthroughUpstream performs the retried upstream HTTP round-trip for passthrough.
func (p *Proxy) executePassthroughUpstream(gw *gateway.NenyaGateway, ctx context.Context, r *http.Request, upstreamURL string, bodyBytes []byte, provider *config.Provider) (*http.Response, error) {
	maxAttempts := provider.MaxRetryAttempts
	if maxAttempts <= 0 {
		maxAttempts = gw.Config.Governance.EffectiveMaxRetryAttempts()
	}

	var resp *http.Response
	err := util.DoWithRetry(ctx, maxAttempts, func() error {
		req, reqErr := p.buildUpstreamRequest(gw, ctx, r.Method, upstreamURL, bodyBytes, provider.Name, r.Header)
		if reqErr != nil {
			return reqErr
		}
		if ct := r.Header.Get("Content-Type"); ct != "" {
			req.Header.Set("Content-Type", ct)
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
	return resp, err
}

func readPassthroughBody(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
		return nil, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()
	return io.ReadAll(r.Body)
}

func buildPassthroughURL(provider *config.Provider, subPath, rawQuery string) string {
	upstreamURL := provider.BaseURL + "/" + subPath
	if rawQuery != "" {
		upstreamURL += "?" + rawQuery
	}
	return upstreamURL
}

func buildPassthroughContextAndCancel(ctx context.Context, provider *config.Provider) (context.Context, func()) {
	if provider.TimeoutSeconds > 0 {
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(provider.TimeoutSeconds)*time.Second)
		return timeoutCtx, cancel
	}
	return ctx, func() {}
}
func (p *Proxy) pipeSSE(ctx context.Context, ctxLogger *slog.Logger, src io.Reader, dst http.ResponseWriter) {
	stallR := newStallReader(src, 120*time.Second)
	buf := make([]byte, 4096)
	defer func() {
		stallR.Stop()
		if stallR != nil {
			_, _ = stallR.DrainPending(3 * time.Second)
		}
	}()

	for {
		n, err := stallR.Read(buf)
		if n > 0 {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				ctxLogger.Debug("SSE write error", "err", writeErr)
				return
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err == errStreamStalled {
				ctxLogger.Warn("passthrough SSE stream stalled, aborting")
			} else if err != io.EOF {
				ctxLogger.Debug("passthrough SSE read error", "err", err)
			}
			return
		}

		if ctx.Err() != nil {
			ctxLogger.Debug("passthrough SSE stream canceled by context")
			return
		}
	}
}
