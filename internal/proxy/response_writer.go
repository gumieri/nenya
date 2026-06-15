package proxy

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/nenya/internal/routing"
)

// writeUpstreamResponse copies headers, status code, and body from an upstream
// response to the client writer. Debug-level errors from copyStream are logged.
func writeUpstreamResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, logger *slog.Logger) {
	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)
	if _, err := copyStream(ctx, w, resp.Body, nil); err != nil {
		logger.DebugContext(ctx, "response copy ended", "err", err)
	}
}

// writeUpstreamBytesResponse writes headers, status code, and a pre-read body
// to the client writer. Debug-level errors from Write are logged.
func writeUpstreamBytesResponse(ctx context.Context, w http.ResponseWriter, resp *http.Response, body []byte, logger *slog.Logger) {
	routing.CopyHeaders(resp.Header, w.Header())
	w.WriteHeader(resp.StatusCode)
	if _, err := w.Write(body); err != nil {
		logger.DebugContext(ctx, "response write ended", "err", err)
	}
}
