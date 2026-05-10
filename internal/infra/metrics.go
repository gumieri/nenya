package infra

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"nenya/internal/version"
)

type Metrics struct {
	startTime time.Time

	tokens    sync.Map
	reqTotal  sync.Map
	errTotal  sync.Map
	httpTotal sync.Map
	httpDur   sync.Map

	redactions    atomic.Uint64
	compactions   atomic.Uint64
	panics        atomic.Uint64
	windowApplied sync.Map
	interceptions sync.Map

	rlRejected    sync.Map
	cooldowns     sync.Map
	exhausted     sync.Map
	streamBlocked sync.Map
	streamStalled sync.Map
	emptyStreams  sync.Map

	upstreamLatency sync.Map
	gatewayProcess  sync.Map
	ollamaBytes     atomic.Uint64
	modelDiscovery  sync.Map
	retryTotal      sync.Map
	inflightReqs    sync.Map

	// MCP metrics
	mcpToolCallsTotal   sync.Map
	mcpToolCallDuration sync.Map
	mcpAutoSearchTotal  sync.Map
	mcpAutoSaveTotal    sync.Map
	mcpLoopIterations   sync.Map
	mcpLoopDuration     sync.Map
	mcpServerReady      sync.Map

	// Auth metrics
	authSuccess sync.Map
	authFailure sync.Map

	// Cache metrics
	cacheHit  sync.Map
	cacheMiss sync.Map

	// Secure memory metrics
	secureMemInitFailures atomic.Uint64
	secureMemSealFailures atomic.Uint64

	overflowGuardTriggers sync.Map
	cbStateTransitions    sync.Map
	mcpActiveGoroutines   atomic.Int64

	RateLimits func() map[string]*RateLimitSnapshot
	Cooldowns  func() (active int)
	CBStates   func() map[string]string
}

type RateLimitSnapshot struct {
	RPM float64
	TPM float64
}

var HTTPDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

type labeledEntry struct {
	labels map[string]string
	value  atomic.Uint64
}

type histogram struct {
	labels  map[string]string
	buckets []float64
	counts  []atomic.Uint64
	sumNS   atomic.Uint64
	count   atomic.Uint64
}

func NewMetrics() *Metrics {
	return &Metrics{
		startTime: time.Now(),
	}
}

func getOrCreateEntry(mmap *sync.Map, labels map[string]string) *labeledEntry {
	key := labelKey(labels)
	if v, ok := mmap.LoadOrStore(key, &labeledEntry{labels: labels}); ok {
		return v.(*labeledEntry)
	}
	v, _ := mmap.Load(key)
	return v.(*labeledEntry)
}

func getOrCreateHist(mmap *sync.Map, labels map[string]string, buckets []float64) *histogram {
	key := labelKey(labels)
	h := &histogram{labels: labels, buckets: buckets, counts: make([]atomic.Uint64, len(buckets))}
	if v, ok := mmap.LoadOrStore(key, h); ok {
		return v.(*histogram)
	}
	v, _ := mmap.Load(key)
	return v.(*histogram)
}

func (h *histogram) Observe(seconds float64) {
	for i, bkt := range h.buckets {
		if seconds <= bkt {
			h.counts[i].Add(1)
		}
	}
	h.sumNS.Add(uint64(seconds * 1e9))
	h.count.Add(1)
}

// labelKey converts a map of labels to a comma-separated string
// sorted by key for consistent hashing.
// Example: map[string]string{"a":"1", "b":"2"} -> "a=1,b=2"
func labelKey(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

// labelStr formats labels for Prometheus output with proper escaping.
// Example: map[string]string{"job":"api-server"} -> {job="api-server"}
// Special characters are escaped: \ -> \\, " -> \", newline -> \n
func labelStr(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		escaped := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
		pairs = append(pairs, k+"=\""+escaped+"\"")
	}
	sort.Strings(pairs)
	return "{" + strings.Join(pairs, ", ") + "}"
}

func (m *Metrics) RecordTokens(direction, model, agent, provider string, count int) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.tokens, map[string]string{
		"direction": direction, "model": model, "agent": agent, "provider": provider,
	})
	e.value.Add(uint64(count))
}

func (m *Metrics) RecordUpstreamRequest(model, agent, provider string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.reqTotal, map[string]string{
		"model": model, "agent": agent, "provider": provider,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordUpstreamError(model, agent, provider string, statusCode int) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.errTotal, map[string]string{
		"model": model, "agent": agent, "provider": provider, "code": strconv.Itoa(statusCode),
	})
	e.value.Add(1)
}

func (m *Metrics) RecordHTTPRequest(method, path string, status int, duration time.Duration) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.httpTotal, map[string]string{
		"method": method, "path": path, "status": strconv.Itoa(status),
	})
	e.value.Add(1)

	h := getOrCreateHist(&m.httpDur, map[string]string{"method": method, "path": path}, HTTPDurationBuckets)
	h.Observe(duration.Seconds())
}

func (m *Metrics) RecordRedaction() {
	if m == nil {
		return
	}
	m.redactions.Add(1)
}
func (m *Metrics) RecordCompaction() {
	if m == nil {
		return
	}
	m.compactions.Add(1)
}
func (m *Metrics) RecordPanic() {
	if m == nil {
		return
	}
	m.panics.Add(1)
}

func (m *Metrics) RecordWindow(mode string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.windowApplied, map[string]string{"mode": mode})
	e.value.Add(1)
}

func (m *Metrics) RecordInterception(reason string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.interceptions, map[string]string{"reason": reason})
	e.value.Add(1)
}

func (m *Metrics) RecordRateLimitRejected(host string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.rlRejected, map[string]string{"host": host})
	e.value.Add(1)
}

func (m *Metrics) RecordCooldown(agent, provider, model string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.cooldowns, map[string]string{
		"agent": agent, "provider": provider, "model": model,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordExhausted(agent string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.exhausted, map[string]string{"agent": agent})
	e.value.Add(1)
}

func (m *Metrics) RecordStreamBlock(model, provider string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.streamBlocked, map[string]string{
		"model": model, "provider": provider,
	})
	e.value.Add(1)
}

// RecordStreamStall increments the stream stall counter for the given model/provider.
func (m *Metrics) RecordStreamStall(model, provider string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.streamStalled, map[string]string{
		"model": model, "provider": provider,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordEmptyStream(model, provider string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.emptyStreams, map[string]string{
		"model": model, "provider": provider,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordMCPToolCall(server, tool, agent string, duration time.Duration, callErr error) {
	if m == nil {
		return
	}
	status := "success"
	if callErr != nil {
		status = "error"
	}
	e := getOrCreateEntry(&m.mcpToolCallsTotal, map[string]string{
		"server": server, "tool": tool, "agent": agent, "status": status,
	})
	e.value.Add(1)

	h := getOrCreateHist(&m.mcpToolCallDuration, map[string]string{
		"server": server, "tool": tool,
	}, HTTPDurationBuckets)
	h.Observe(duration.Seconds())
}

func (m *Metrics) RecordMCPAutoSearch(server, agent string, hit bool, searchErr error) {
	if m == nil {
		return
	}
	status := "miss"
	if searchErr != nil {
		status = "error"
	} else if hit {
		status = "hit"
	}
	e := getOrCreateEntry(&m.mcpAutoSearchTotal, map[string]string{
		"server": server, "agent": agent, "status": status,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordMCPAutoSave(server, agent string, saveErr error) {
	if m == nil {
		return
	}
	status := "success"
	if saveErr != nil {
		status = "error"
	}
	e := getOrCreateEntry(&m.mcpAutoSaveTotal, map[string]string{
		"server": server, "agent": agent, "status": status,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordMCPLoopIteration(agent string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.mcpLoopIterations, map[string]string{"agent": agent})
	e.value.Add(1)
}

func (m *Metrics) RecordMCPLoopDuration(agent string, duration time.Duration) {
	if m == nil {
		return
	}
	h := getOrCreateHist(&m.mcpLoopDuration, map[string]string{"agent": agent}, HTTPDurationBuckets)
	h.Observe(duration.Seconds())
}

func (m *Metrics) SetMCPServerReady(server string, ready bool) {
	if m == nil {
		return
	}
	val := uint64(0)
	if ready {
		val = 1
	}
	e := getOrCreateEntry(&m.mcpServerReady, map[string]string{"server": server})
	e.value.Store(val)
}

func (m *Metrics) RecordAuthSuccess(authType, keyName string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.authSuccess, map[string]string{
		"type": authType, "key": keyName,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordAuthFailure(authType string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.authFailure, map[string]string{
		"type": authType,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordSecureMemInitFailure() {
	if m == nil {
		return
	}
	m.secureMemInitFailures.Add(1)
}

func (m *Metrics) RecordSecureMemSealFailure() {
	if m == nil {
		return
	}
	m.secureMemSealFailures.Add(1)
}

func (m *Metrics) RecordCacheHit(cacheType string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.cacheHit, map[string]string{"type": cacheType})
	e.value.Add(1)
}

func (m *Metrics) RecordCacheMiss(cacheType string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.cacheMiss, map[string]string{"type": cacheType})
	e.value.Add(1)
}

func (m *Metrics) RecordOverflowGuardTrigger(location string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.overflowGuardTriggers, map[string]string{"location": location})
	e.value.Add(1)
}

func (m *Metrics) RecordCBStateTransition(key string, from, to string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.cbStateTransitions, map[string]string{
		"key":  key,
		"from": from,
		"to":   to,
	})
	e.value.Add(1)
}

func (m *Metrics) IncMCPActiveGoroutines() {
	if m == nil {
		return
	}
	m.mcpActiveGoroutines.Add(1)
}

func (m *Metrics) DecMCPActiveGoroutines() {
	if m == nil {
		return
	}
	m.mcpActiveGoroutines.Add(-1)
}

func (m *Metrics) RecordUpstreamLatency(model, agent, provider string, duration time.Duration) {
	if m == nil {
		return
	}
	h := getOrCreateHist(&m.upstreamLatency, map[string]string{
		"model": model, "agent": agent, "provider": provider,
	}, HTTPDurationBuckets)
	h.Observe(duration.Seconds())
}

func (m *Metrics) RecordGatewayProcessing(method, path string, duration time.Duration) {
	if m == nil {
		return
	}
	h := getOrCreateHist(&m.gatewayProcess, map[string]string{
		"method": method, "path": path,
	}, HTTPDurationBuckets)
	h.Observe(duration.Seconds())
}

func (m *Metrics) RecordOllamaSummarizedBytes(n int) {
	if m == nil {
		return
	}
	m.ollamaBytes.Add(uint64(n))
}

func (m *Metrics) RecordTrimmedRequest(model string, savedTokens int) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.interceptions, map[string]string{"model": model})
	e.value.Add(1)
}

func (m *Metrics) RecordModelDiscovery(provider string, err error) {
	if m == nil {
		return
	}
	status := "success"
	if err != nil {
		status = "error"
	}
	e := getOrCreateEntry(&m.modelDiscovery, map[string]string{
		"provider": provider, "status": status,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordRetry(operation, provider string, err error) {
	if m == nil {
		return
	}
	status := "success"
	if err != nil {
		status = "error"
	}
	e := getOrCreateEntry(&m.retryTotal, map[string]string{
		"operation": operation, "provider": provider, "status": status,
	})
	e.value.Add(1)
}

func (m *Metrics) IncInFlight(model, agent, provider string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.inflightReqs, map[string]string{
		"model": model, "agent": agent, "provider": provider,
	})
	e.value.Add(1)
}

func (m *Metrics) DecInFlight(model, agent, provider string) {
	if m == nil {
		return
	}
	e := getOrCreateEntry(&m.inflightReqs, map[string]string{
		"model": model, "agent": agent, "provider": provider,
	})
	for {
		old := e.value.Load()
		if old == 0 {
			return
		}
		if e.value.CompareAndSwap(old, old-1) {
			return
		}
	}
}

func (m *Metrics) WritePrometheus(w io.Writer) {
	if m == nil {
		return
	}
	fprintln := func(format string, args ...interface{}) {
		_, _ = fmt.Fprintf(w, format+"\n", args...)
	}

	fprintln("# HELP nenya_build_info Nenya gateway build information.")
	fprintln("# TYPE nenya_build_info gauge")
	fprintln(`nenya_build_info{version="%s",go_version="%s"} 1`, version.Version, runtime.Version())

	fprintln("# HELP nenya_uptime_seconds Gateway uptime in seconds.")
	fprintln("# TYPE nenya_uptime_seconds gauge")
	fprintln("nenya_uptime_seconds %g", time.Since(m.startTime).Seconds())

	fprintln("# HELP nenya_go_goroutines Number of running goroutines.")
	fprintln("# TYPE nenya_go_goroutines gauge")
	fprintln("nenya_go_goroutines %d", runtime.NumGoroutine())

	m.writeCounterMap(w, "nenya_tokens_estimated_total",
		"Estimated token usage by direction, model, agent, and provider.", &m.tokens)
	m.writeCounterMap(w, "nenya_upstream_requests_total",
		"Total upstream provider requests.", &m.reqTotal)
	m.writeCounterMap(w, "nenya_upstream_errors_total",
		"Total upstream provider errors by status code.", &m.errTotal)
	m.writeCounterMap(w, "nenya_http_requests_total",
		"Total HTTP requests by method, path, and status.", &m.httpTotal)

	m.writeCounterAtomic(w, "nenya_panics_total",
		"Total recovered panics in the request handler.", m.panics.Load())
	m.writeCounterAtomic(w, "nenya_pipeline_redactions_total",
		"Total secret redactions applied by the Tier-0 filter.", m.redactions.Load())
	m.writeCounterAtomic(w, "nenya_pipeline_compaction_applied_total",
		"Total text compaction passes applied.", m.compactions.Load())
	m.writeCounterMap(w, "nenya_pipeline_window_applied_total",
		"Total window compaction passes applied.", &m.windowApplied)
	m.writeCounterMap(w, "nenya_pipeline_interceptions_total",
		"Total Ollama interceptions by trigger reason.", &m.interceptions)
	m.writeCounterMap(w, "nenya_ratelimit_rejected_total",
		"Total requests rejected by rate limiter.", &m.rlRejected)
	m.writeCounterMap(w, "nenya_agent_cooldowns_total",
		"Total agent model cooldowns activated.", &m.cooldowns)
	m.writeCounterMap(w, "nenya_agent_targets_exhausted_total",
		"Total times all agent targets were exhausted.", &m.exhausted)
	m.writeCounterMap(w, "nenya_stream_blocked_total",
		"Total upstream streams killed by execution policy.", &m.streamBlocked)
	m.writeCounterMap(w, "nenya_stream_stalled_total",
		"Total upstream streams killed by idle timeout.", &m.streamStalled)
	m.writeCounterMap(w, "nenya_empty_stream_total",
		"Total upstream streams that returned empty body.", &m.emptyStreams)

	m.writeHistogramMap(w, "nenya_upstream_request_duration_seconds",
		"Upstream provider request duration in seconds.", &m.upstreamLatency)
	m.writeHistogramMap(w, "nenya_gateway_processing_duration_seconds",
		"Gateway processing time (before upstream) in seconds.", &m.gatewayProcess)
	m.writeCounterAtomic(w, "nenya_ollama_summarized_bytes_total",
		"Total bytes sent to Ollama for summarization.", m.ollamaBytes.Load())
	m.writeCounterAtomic(w, "nenya_secure_mem_init_failures_total",
		"Total secure memory allocation failures.", m.secureMemInitFailures.Load())
	m.writeCounterAtomic(w, "nenya_secure_mem_seal_failures_total",
		"Total secure memory seal (mprotect) failures.", m.secureMemSealFailures.Load())
	m.writeCounterMap(w, "nenya_model_discovery_total",
		"Total model discovery fetch attempts by provider.", &m.modelDiscovery)
	m.writeCounterMap(w, "nenya_retries_total",
		"Total retry attempts by operation and provider.", &m.retryTotal)
	m.writeGaugeMap(w, "nenya_inflight_requests",
		"Current in-flight requests by model, agent, and provider.", &m.inflightReqs)

	m.writeHistogramMap(w, "nenya_http_request_duration_seconds",
		"HTTP request duration in seconds.", &m.httpDur)

	m.writeCounterMap(w, "nenya_mcp_tool_calls_total",
		"Total MCP tool call executions.", &m.mcpToolCallsTotal)
	m.writeHistogramMap(w, "nenya_mcp_tool_call_duration_seconds",
		"MCP tool call duration in seconds.", &m.mcpToolCallDuration)
	m.writeCounterMap(w, "nenya_mcp_auto_search_total",
		"Total MCP auto-search attempts by outcome.", &m.mcpAutoSearchTotal)
	m.writeCounterMap(w, "nenya_mcp_auto_save_total",
		"Total MCP auto-save attempts by outcome.", &m.mcpAutoSaveTotal)
	m.writeCounterMap(w, "nenya_mcp_loop_iterations_total",
		"Total MCP multi-turn loop iterations.", &m.mcpLoopIterations)
	m.writeHistogramMap(w, "nenya_mcp_loop_duration_seconds",
		"Total MCP multi-turn loop duration in seconds.", &m.mcpLoopDuration)
	m.writeGaugeMap(w, "nenya_mcp_server_ready",
		"MCP server readiness (1=ready, 0=not ready).", &m.mcpServerReady)
	m.writeCounterMap(w, "nenya_auth_success_total",
		"Total successful authentications by type and key name.", &m.authSuccess)
	m.writeCounterMap(w, "nenya_auth_failure_total",
		"Total failed authentication attempts by type.", &m.authFailure)
	m.writeCounterMap(w, "nenya_cache_hit_total",
		"Total cache hits by cache type.", &m.cacheHit)
	m.writeCounterMap(w, "nenya_cache_miss_total",
		"Total cache misses by cache type.", &m.cacheMiss)
	m.writeCounterMap(w, "nenya_overflow_guard_triggers_total",
		"Total overflow guard triggers by location.", &m.overflowGuardTriggers)
	m.writeCounterMap(w, "nenya_cb_state_transitions_total",
		"Total circuit breaker state transitions.", &m.cbStateTransitions)
	// Note: Using writeCounterAtomic for a gauge metric. Prometheus doesn't
	// distinguish between counters and gauges for display purposes, and
	// this simplifies the implementation while maintaining correct semantics.
	m.writeCounterAtomic(w, "nenya_mcp_active_goroutines",
		"Current number of active MCP transport goroutines.", uint64(m.mcpActiveGoroutines.Load()))

	if m.RateLimits != nil {
		fprintln("# HELP nenya_ratelimit_rpm_available Current RPM bucket available.")
		fprintln("# TYPE nenya_ratelimit_rpm_available gauge")
		fprintln("# HELP nenya_ratelimit_tpm_available Current TPM bucket available.")
		fprintln("# TYPE nenya_ratelimit_tpm_available gauge")
		for host, rl := range m.RateLimits() {
			fprintln(`nenya_ratelimit_rpm_available{host="%s"} %g`, host, rl.RPM)
			fprintln(`nenya_ratelimit_tpm_available{host="%s"} %g`, host, rl.TPM)
		}
	}

	if m.Cooldowns != nil {
		active := m.Cooldowns()
		fprintln("# HELP nenya_agent_active_cooldowns Number of currently active model cooldowns.")
		fprintln("# TYPE nenya_agent_active_cooldowns gauge")
		fprintln("nenya_agent_active_cooldowns %d", active)
	}

	if m.CBStates != nil {
		fprintln("# HELP nenya_cb_state Circuit breaker state per model key.")
		fprintln("# TYPE nenya_cb_state gauge")
		states := m.CBStates()
		keys := make([]string, 0, len(states))
		for k := range states {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, key := range keys {
			stateVal := 0
			switch states[key] {
			case "open":
				stateVal = 1
			case "half_open":
				stateVal = 2
			}
			fprintln(`nenya_cb_state{key="%s",state="%s"} %d`, key, states[key], stateVal)
		}
	}
}

func (m *Metrics) writeCounterMap(w io.Writer, name, help string, mmap *sync.Map) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
	var entries []*labeledEntry
	mmap.Range(func(_, v interface{}) bool {
		entries = append(entries, v.(*labeledEntry))
		return true
	})
	sort.Slice(entries, func(i, j int) bool {
		return labelKey(entries[i].labels) < labelKey(entries[j].labels)
	})
	for _, e := range entries {
		_, _ = fmt.Fprintf(w, "%s%s %d\n", name, labelStr(e.labels), e.value.Load())
	}
}

func (m *Metrics) writeCounterAtomic(w io.Writer, name, help string, value uint64) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
	_, _ = fmt.Fprintf(w, "%s %d\n", name, value)
}

func (m *Metrics) writeGaugeMap(w io.Writer, name, help string, mmap *sync.Map) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	var entries []*labeledEntry
	mmap.Range(func(_, v interface{}) bool {
		entries = append(entries, v.(*labeledEntry))
		return true
	})
	sort.Slice(entries, func(i, j int) bool {
		return labelKey(entries[i].labels) < labelKey(entries[j].labels)
	})
	for _, e := range entries {
		_, _ = fmt.Fprintf(w, "%s%s %d\n", name, labelStr(e.labels), e.value.Load())
	}
}

func (m *Metrics) writeHistogramMap(w io.Writer, name, help string, mmap *sync.Map) {
	_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	_, _ = fmt.Fprintf(w, "# TYPE %s histogram\n", name)
	var hists []*histogram
	mmap.Range(func(_, v interface{}) bool {
		hists = append(hists, v.(*histogram))
		return true
	})
	sort.Slice(hists, func(i, j int) bool {
		return labelKey(hists[i].labels) < labelKey(hists[j].labels)
	})
	for _, h := range hists {
		ls := labelStr(h.labels)
		labelSuffix := stripTrailingBrace(ls)
		for i, bkt := range h.buckets {
			_, _ = fmt.Fprintf(w, "%s_bucket%s,le=\"%g\"} %d\n", name, labelSuffix, bkt, h.counts[i].Load())
		}
		_, _ = fmt.Fprintf(w, "%s_bucket%s,le=\"+Inf\"} %d\n", name, labelSuffix, h.count.Load())
		_, _ = fmt.Fprintf(w, "%s_sum%s %g\n", name, ls, float64(h.sumNS.Load())/1e9)
		_, _ = fmt.Fprintf(w, "%s_count%s %d\n", name, ls, h.count.Load())
	}
}

// stripTrailingBrace removes the trailing '}' from a label string,
// used for constructing Prometheus bucket labels.
// Example: {model="gpt-4"} -> {model="gpt-4"
func stripTrailingBrace(s string) string {
	if s == "" {
		return ""
	}
	return s[:len(s)-1]
}
