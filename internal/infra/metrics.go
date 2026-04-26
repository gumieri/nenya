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

	// MCP metrics
	mcpToolCallsTotal   sync.Map // labels: server, tool, agent, status (success/error)
	mcpToolCallDuration sync.Map // labels: server, tool (histogram)
	mcpAutoSearchTotal  sync.Map // labels: server, agent, status (hit/miss/error)
	mcpAutoSaveTotal    sync.Map // labels: server, agent, status (success/error)
	mcpLoopIterations   sync.Map // labels: agent
	mcpLoopDuration     sync.Map // labels: agent (histogram)
	mcpServerReady      sync.Map // labels: server (gauge: 1=ready, 0=not ready)

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

func labelKey(labels map[string]string) string {
	pairs := make([]string, 0, len(labels))
	for k, v := range labels {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

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

func (m *Metrics) RecordRedaction()  { if m == nil { return }; m.redactions.Add(1) }
func (m *Metrics) RecordCompaction() { if m == nil { return }; m.compactions.Add(1) }
func (m *Metrics) RecordPanic()      { if m == nil { return }; m.panics.Add(1) }

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

func (m *Metrics) WritePrometheus(w io.Writer) {
	if m == nil {
		return
	}
	fprintln := func(format string, args ...interface{}) {
		_, _ = fmt.Fprintf(w, format+"\n", args...)
	}

	fprintln("# HELP nenya_build_info Nenya gateway build information.")
	fprintln("# TYPE nenya_build_info gauge")
	fprintln(`nenya_build_info{version="dev",go_version="%s"} 1`, runtime.Version())

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
		for i, bkt := range h.buckets {
			_, _ = fmt.Fprintf(w, "%s_bucket%s,le=\"%g\"} %d\n", name, ls[:len(ls)-1], bkt, h.counts[i].Load())
		}
		_, _ = fmt.Fprintf(w, "%s_bucket%s,le=\"+Inf\"} %d\n", name, ls[:len(ls)-1], h.count.Load())
		_, _ = fmt.Fprintf(w, "%s_sum%s %g\n", name, ls, float64(h.sumNS.Load())/1e9)
		_, _ = fmt.Fprintf(w, "%s_count%s %d\n", name, ls, h.count.Load())
	}
}
