package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"git.0ur.uk/nenya/config"
	"git.0ur.uk/nenya/internal/gateway"
	"git.0ur.uk/nenya/internal/infra"
	"git.0ur.uk/nenya/internal/routing"
)

func (p *Proxy) handlePassthrough(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, keyRef string) {
	ctxLogger := gw.Logger.With("operation", "passthrough", "api_key", keyRef)
	segments := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(segments) < 2 {
		ctxLogger.Warn("invalid path format", "path", r.URL.Path)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid passthrough path format")
		return
	}

	providerName := segments[1]
	if providerName == "" {
		ctxLogger.Warn("missing provider name", "path", r.URL.Path)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Missing provider name")
		return
	}

	provider, ok := gw.Providers[providerName]
	if !ok {
		ctxLogger.Warn("unknown provider", "provider", providerName)
		writeStructuredError(w, http.StatusNotFound, infra.ErrorKindModelNotFound, "Unknown provider")
		return
	}

	if provider.APIKey == "" && provider.AuthStyle != "none" {
		ctxLogger.Warn("provider not configured", "provider", providerName)
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInvalidRequest, "Provider not configured")
		return
	}

	if len(segments) < 3 {
		ctxLogger.Warn("missing endpoint path", "provider", providerName)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Missing endpoint path")
		return
	}

	subPath := path.Join(segments[2:]...)
	if strings.Contains(subPath, "..") {
		ctxLogger.Warn("path traversal attempt", "provider", providerName, "path", subPath)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid path")
		return
	}

	if !gw.RateLimiter.Check(provider.BaseURL, 0) {
		gw.Metrics.RecordRateLimitRejected(infra.ExtractHost(provider.BaseURL))
		ctxLogger.Warn("rate limit exceeded", "provider", providerName)
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "Rate limit exceeded")
		return
	}

	bodyBytes, err := readPassthroughBody(gw, w, r)
	if err != nil {
		ctxLogger.Error("failed to read request body", "provider", providerName, "err", err)
		writeStructuredError(w, http.StatusRequestEntityTooLarge, infra.ErrorKindPayloadTooLarge, "Payload too large or malformed")
		return
	}

	upstreamURL := buildPassthroughURL(provider, subPath, r.URL.RawQuery)
	ctx, cancel := buildPassthroughContextAndCancel(r.Context(), provider)
	defer cancel()

	ctxLogger = ctxLogger.With(
		"provider", providerName,
		"method", r.Method,
		"path", subPath,
	)

	resp, err := p.executePassthroughUpstream(gw, ctx, r, upstreamURL, bodyBytes, provider)
	if err != nil {
		ctxLogger.Error("upstream request failed", "err", err)
		writeStructuredError(w, http.StatusBadGateway, infra.ErrorKindNetworkError, "Upstream provider error")
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
	return p.doUpstreamRoundTrip(ctx, gw, r.Method, upstreamURL, bodyBytes, provider.Name, "", r.Header, r.Header.Get("Content-Type"), maxAttempts)
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
	stallR := newStallReader(ctx, src, 120*time.Second)
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
