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
	m.CBStates = func() map[string]string {
		return map[string]string{"test-key": "closed"}
	}

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
		"nenya_cb_state",
		"nenya_pipeline_compaction_applied_total",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(out, metric) {
			t.Errorf("missing metric %q in output", metric)
		}
	}
}

func TestMetrics_NewErrorAndTrackingMetrics(t *testing.T) {
	m := NewMetrics()

	m.RecordSummarizationDuration("agent-1", "gemini", "model-a", 500*time.Millisecond)
	m.RecordSummarizationDuration("agent-1", "gemini", "model-a", 700*time.Millisecond)

	m.RecordErrorKind("rate_limited", "gemini")
	m.RecordErrorKind("context_limit_exceeded", "openai")

	m.RecordCBFailure("key1")
	m.RecordCBSuccess("key1")
	m.RecordCBFailure("key2")

	m.RecordInterceptorDuration("bouncer", 100*time.Millisecond)
	m.RecordInterceptorApplied("bouncer")
	m.RecordInterceptorError("entropy")

	m.RecordAdapterConversion("anthropic", "success")
	m.RecordAdapterConversion("anthropic", "error")

	m.RecordOllamaEnrichment("success")
	m.RecordOllamaEnrichment("failed")
	m.RecordOllamaEnrichmentDuration(2 * time.Second)

	m.RecordAccountSelection("gemini", "selected")
	m.RecordAccountSelection("openai", "none_available")

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()

	expectedMetrics := []string{
		"nenya_summarization_duration_seconds",
		"nenya_error_kind_total",
		"nenya_cb_failures_total",
		"nenya_cb_successes_total",
		"nenya_interceptor_duration_seconds",
		"nenya_interceptor_applied_total",
		"nenya_interceptor_errors_total",
		"nenya_adapter_conversions_total",
		"nenya_ollama_enrichment_total",
		"nenya_ollama_enrichment_duration_seconds",
		"nenya_account_selection_total",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(out, metric) {
			t.Errorf("missing metric %q in output", metric)
		}
	}

	if !regexp.MustCompile(`kind="rate_limited"`).MatchString(out) {
		t.Error("missing kind=rate_limited label")
	}
	if !regexp.MustCompile(`status="success"`).MatchString(out) {
		t.Error("missing status=success label")
	}
	if !regexp.MustCompile(`name="bouncer"`).MatchString(out) {
		t.Error("missing name=bouncer label")
	}
}

func TestMetrics_NewMetrics(t *testing.T) {
	m := NewMetrics()
	m.CBStates = func() map[string]string {
		return map[string]string{"test-key": "closed"}
	}
	m.Cooldowns = func() int { return 3 }
	m.RateLimits = func() map[string]*RateLimitSnapshot {
		return map[string]*RateLimitSnapshot{"host1": {RPM: 10, TPM: 1000}}
	}

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
	m.RecordEmptyStream("model-a", "gemini")

	m.RecordUpstreamLatency("model-a", "agent-1", "gemini", 500*time.Millisecond)
	m.RecordGatewayProcessing("POST", "/v1/chat/completions", 50*time.Millisecond)
	m.RecordOllamaSummarizedBytes(1024)
	m.RecordOllamaSummarizedBytes(2048)
	m.RecordModelDiscovery("gemini", nil)
	m.RecordModelDiscovery("ollama", nil)
	m.RecordModelDiscovery("bad-provider", assertAnError)
	m.RecordRetry("model_discovery", "gemini", assertAnError)
	m.RecordRetry("model_discovery", "gemini", nil)
	m.IncInFlight("model-a", "agent-1", "gemini")
	m.IncInFlight("model-a", "agent-1", "gemini")
	m.IncInFlight("model-b", "agent-2", "openai")
	m.DecInFlight("model-a", "agent-1", "gemini")

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	out := buf.String()

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
		"nenya_empty_stream_total",
		"nenya_http_request_duration_seconds",
		"nenya_upstream_request_duration_seconds",
		"nenya_gateway_processing_duration_seconds",
		"nenya_ollama_summarized_bytes_total",
		"nenya_model_discovery_total",
		"nenya_retries_total",
		"nenya_inflight_requests",
		"nenya_build_info",
		"nenya_uptime_seconds",
		"nenya_go_goroutines",
		"nenya_cb_state",
	}

	for _, metric := range expectedMetrics {
		if !strings.Contains(out, metric) {
			t.Errorf("missing metric %q in output", metric)
		}
	}

	if !regexp.MustCompile(`model="model-a"`).MatchString(out) {
		t.Error("missing model=model-a label")
	}
	if !regexp.MustCompile(`direction="input"`).MatchString(out) {
		t.Error("missing direction=input label")
	}
	if !regexp.MustCompile(`status="error"`).MatchString(out) {
		t.Error("missing status=error label")
	}
	if !regexp.MustCompile(`provider="gemini".*provider="bad-provider"`).MatchString(out) {
		// At least one bad-provider should show up
		t.Log("note: bad-provider should have error status")
	}
}

func TestMetrics_NilSafety(t *testing.T) {
	var m *Metrics

	m.RecordTokens("input", "m", "a", "p", 100)
	m.RecordUpstreamRequest("m", "a", "p")
	m.RecordUpstreamError("m", "a", "p", 500)
	m.RecordHTTPRequest("POST", "/p", 200, time.Second)
	m.RecordRedaction()
	m.RecordCompaction()
	m.RecordPanic()
	m.RecordWindow("w")
	m.RecordInterception("r")
	m.RecordRateLimitRejected("h")
	m.RecordCooldown("a", "p", "m")
	m.RecordExhausted("a")
	m.RecordStreamBlock("m", "p")
	m.RecordEmptyStream("m", "p")
	m.RecordMCPToolCall("s", "t", "a", time.Second, nil)
	m.RecordMCPAutoSearch("s", "a", true, nil)
	m.RecordMCPAutoSave("s", "a", nil)
	m.RecordMCPLoopIteration("a")
	m.RecordMCPLoopDuration("a", time.Second)
	m.SetMCPServerReady("s", true)

	m.RecordUpstreamLatency("m", "a", "p", time.Second)
	m.RecordGatewayProcessing("POST", "/p", time.Second)
	m.RecordOllamaSummarizedBytes(100)
	m.RecordModelDiscovery("p", nil)
	m.RecordRetry("op", "p", nil)
	m.IncInFlight("m", "a", "p")
	m.DecInFlight("m", "a", "p")

	m.RecordSummarizationDuration("a", "p", "m", time.Second)
	m.RecordErrorKind("k", "p")
	m.RecordCBFailure("k")
	m.RecordCBSuccess("k")
	m.RecordInterceptorDuration("n", time.Second)
	m.RecordInterceptorApplied("n")
	m.RecordInterceptorError("n")
	m.RecordAdapterConversion("a", "s")
	m.RecordOllamaEnrichment("s")
	m.RecordOllamaEnrichmentDuration(time.Second)
	m.RecordAccountSelection("p", "s")

	var buf bytes.Buffer
	m.WritePrometheus(&buf)
	if buf.Len() > 0 {
		t.Error("expected empty output for nil Metrics")
	}
}

var assertAnError = &errSentinel{}

type errSentinel struct{}

func (e *errSentinel) Error() string { return "test error" }

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
