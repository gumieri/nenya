package infra

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

func (rw *responseWatcher) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func ObserveHTTP(m *Metrics, h func(http.ResponseWriter)) http.HandlerFunc {
	return ObserveHTTPFunc(m, func(w http.ResponseWriter, r *http.Request) { h(w) })
}

func ObserveHTTPFunc(m *Metrics, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			h(w, r)
			return
		}
		start := time.Now()
		ww := &responseWatcher{ResponseWriter: w, statusCode: 200}
		h(ww, r)
		m.RecordHTTPRequest(r.Method, NormalizeMetricPath(r.URL.Path), ww.statusCode, time.Since(start))
	}
}

func HandleMetrics(m *Metrics, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.WritePrometheus(w)
	fmt.Fprintln(w, "# EOF")
}

func NormalizeMetricPath(path string) string {
	switch path {
	case "/healthz", "/statsz", "/metrics", "/v1/models", "/v1/chat/completions", "/v1/embeddings":
		return path
	default:
		return "other"
	}
}

func ExtractHost(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return rawURL
}
