package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/routing"
)

func (p *Proxy) handlePassthrough(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
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

	var bodyBytes []byte
	var err error
	hasBody := r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch
	if hasBody {
		r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
		defer func() { _ = r.Body.Close() }()
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			gw.Logger.Error("passthrough: failed to read request body", "provider", providerName, "err", err)
			http.Error(w, "Payload too large or malformed", http.StatusRequestEntityTooLarge)
			return
		}
	}

	upstreamURL := provider.BaseURL + "/" + subPath
	if r.URL.RawQuery != "" {
		upstreamURL += "?" + r.URL.RawQuery
	}

	ctx := r.Context()
	if provider.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(r.Context(), time.Duration(provider.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	req, err := p.buildUpstreamRequest(gw, ctx, r.Method, upstreamURL, bodyBytes, provider.Name, r.Header)
	if err != nil {
		gw.Logger.Error("passthrough: failed to create upstream request", "provider", providerName, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if r.Header.Get("Content-Type") != "" {
		req.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	}

	ctxLogger := gw.Logger.With(
		"operation", "passthrough",
		"provider", providerName,
		"method", r.Method,
		"path", subPath,
	)

	resp, err := gw.Client.Do(req)
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

func (p *Proxy) pipeSSE(ctx context.Context, ctxLogger *slog.Logger, src io.Reader, dst http.ResponseWriter) {
	buf := make([]byte, 4096)
	stallTimer := time.NewTimer(120 * time.Second)
	defer stallTimer.Stop()

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if !stallTimer.Stop() {
				select {
				case <-stallTimer.C:
				default:
				}
			}
			stallTimer.Reset(120 * time.Second)
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				ctxLogger.Debug("SSE write error", "err", writeErr)
				return
			}
			if flusher, ok := dst.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				ctxLogger.Debug("SSE read error", "err", err)
			}
			return
		}

		select {
		case <-ctx.Done():
			ctxLogger.Debug("SSE stream canceled by context")
			return
		case <-stallTimer.C:
			ctxLogger.Warn("SSE stream stalled, aborting")
			return
		default:
		}
	}
}
