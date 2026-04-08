package main

import (
	"fmt"
	"net/http"
	"net/url"
	"time"
)

type responseWatcher struct {
	http.ResponseWriter
	statusCode int
	written    bool
}

func (rw *responseWatcher) WriteHeader(code int) {
	if !rw.written {
		rw.statusCode = code
		rw.written = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (g *NenyaGateway) observeHTTP(h func(http.ResponseWriter)) http.HandlerFunc {
	return g.observeHTTPFunc(func(w http.ResponseWriter, r *http.Request) { h(w) })
}

func (g *NenyaGateway) observeHTTPFunc(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.metrics == nil {
			h(w, r)
			return
		}
		start := time.Now()
		ww := &responseWatcher{ResponseWriter: w, statusCode: 200}
		h(ww, r)
		g.metrics.RecordHTTPRequest(r.Method, normalizeMetricPath(r.URL.Path), ww.statusCode, time.Since(start))
	}
}

func (g *NenyaGateway) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	g.metrics.WritePrometheus(w)
	fmt.Fprintln(w, "# EOF")
}

func normalizeMetricPath(path string) string {
	switch path {
	case "/healthz", "/statsz", "/metrics", "/v1/models", "/v1/chat/completions", "/v1/embeddings":
		return path
	default:
		return "other"
	}
}

func extractHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}
