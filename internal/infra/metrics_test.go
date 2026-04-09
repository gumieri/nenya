package infra

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMetrics_RecordAndWritePrometheus(t *testing.T) {
	m := NewMetrics()

	m.RecordTokens("input", "model-a", "agent-1", "gemini", 100)
	m.RecordTokens("output", "model-a", "agent-1", "gemini", 42)
	m.RecordUpstreamRequest("model-a", "agent-1", "gemini")
	m.RecordUpstreamError("model-a", "agent-1", "gemini", 429)
	m.RecordHTTPRequest("POST", "/v1/chat/completions", 200, 150*time.Millisecond)
	m.RecordHTTPRequest("POST", "/v1/chat/completions", 200, 50*time.Millisecond)
	m.RecordRedaction()
	m.RecordCompaction()
	m.RecordWindow("summarize")
	m.RecordInterception("soft_limit")
	m.RecordRateLimitRejected("example.com")
	m.RecordCooldown("agent-1", "gemini", "model-a")
	m.RecordExhausted("agent-1")
	m.RecordStreamBlock("model-a", "gemini")

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()

	// Check for expected Prometheus metric names
	expectedMetrics := []string{
		"nenya_tokens_estimated_total",
		"nenya_upstream_requests_total",
		"nenya_upstream_errors_total",
		"nenya_http_requests_total",
		"nenya_pipeline_redactions_total",
		"nenya_pipeline_compaction_applied_total",
		"nenya_pipeline_window_applied_total",
		"nenya_pipeline_interceptions_total",
		"nenya_ratelimit_rejected_total",
		"nenya_agent_cooldowns_total",
		"nenya_agent_targets_exhausted_total",
		"nenya_stream_blocked_total",
		"nenya_http_request_duration_seconds",
		"nenya_build_info",
		"nenya_uptime_seconds",
		"nenya_go_goroutines",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(out, metric) {
			t.Errorf("missing metric %q in output", metric)
		}
	}

	// Check specific label values
	if !regexp.MustCompile(`direction="input"`).MatchString(out) {
		t.Error("missing direction=input label")
	}
	if !regexp.MustCompile(`model="model-a"`).MatchString(out) {
		t.Error("missing model=model-a label")
	}
}

func TestNormalizeMetricPath(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/healthz", "/healthz"},
		{"/statsz", "/statsz"},
		{"/metrics", "/metrics"},
		{"/v1/models", "/v1/models"},
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"/v1/embeddings", "/v1/embeddings"},
		{"/api/other", "other"},
		{"/random/path", "other"},
		{"/", "other"},
	}
	for _, tt := range tests {
		got := NormalizeMetricPath(tt.path)
		if got != tt.want {
			t.Errorf("NormalizeMetricPath(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestExtractHost(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://api.example.com/v1/chat/completions", "api.example.com"},
		{"http://localhost:8080/api", "localhost:8080"},
		{"not-a-url", "not-a-url"},
		{"", ""},
	}
	for _, tt := range tests {
		got := ExtractHost(tt.url)
		if got != tt.want {
			t.Errorf("ExtractHost(%q) = %q, want %q", tt.url, got, tt.want)
		}
	}
}
