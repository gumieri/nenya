package main

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
	windowApplied sync.Map
	interceptions sync.Map

	rlRejected    sync.Map
	cooldowns     sync.Map
	exhausted     sync.Map
	streamBlocked sync.Map

	gateway *NenyaGateway
}

var httpDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

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

func NewMetrics(gw *NenyaGateway) *Metrics {
	return &Metrics{
		startTime: time.Now(),
		gateway:   gw,
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
	e := getOrCreateEntry(&m.tokens, map[string]string{
		"direction": direction, "model": model, "agent": agent, "provider": provider,
	})
	e.value.Add(uint64(count))
}

func (m *Metrics) RecordUpstreamRequest(model, agent, provider string) {
	e := getOrCreateEntry(&m.reqTotal, map[string]string{
		"model": model, "agent": agent, "provider": provider,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordUpstreamError(model, agent, provider string, statusCode int) {
	e := getOrCreateEntry(&m.errTotal, map[string]string{
		"model": model, "agent": agent, "provider": provider, "code": strconv.Itoa(statusCode),
	})
	e.value.Add(1)
}

func (m *Metrics) RecordHTTPRequest(method, path string, status int, duration time.Duration) {
	e := getOrCreateEntry(&m.httpTotal, map[string]string{
		"method": method, "path": path, "status": strconv.Itoa(status),
	})
	e.value.Add(1)

	h := getOrCreateHist(&m.httpDur, map[string]string{"method": method, "path": path}, httpDurationBuckets)
	h.Observe(duration.Seconds())
}

func (m *Metrics) RecordRedaction()  { m.redactions.Add(1) }
func (m *Metrics) RecordCompaction() { m.compactions.Add(1) }

func (m *Metrics) RecordWindow(mode string) {
	e := getOrCreateEntry(&m.windowApplied, map[string]string{"mode": mode})
	e.value.Add(1)
}

func (m *Metrics) RecordInterception(reason string) {
	e := getOrCreateEntry(&m.interceptions, map[string]string{"reason": reason})
	e.value.Add(1)
}

func (m *Metrics) RecordRateLimitRejected(host string) {
	e := getOrCreateEntry(&m.rlRejected, map[string]string{"host": host})
	e.value.Add(1)
}

func (m *Metrics) RecordCooldown(agent, provider, model string) {
	e := getOrCreateEntry(&m.cooldowns, map[string]string{
		"agent": agent, "provider": provider, "model": model,
	})
	e.value.Add(1)
}

func (m *Metrics) RecordExhausted(agent string) {
	e := getOrCreateEntry(&m.exhausted, map[string]string{"agent": agent})
	e.value.Add(1)
}

func (m *Metrics) RecordStreamBlock(model, provider string) {
	e := getOrCreateEntry(&m.streamBlocked, map[string]string{
		"model": model, "provider": provider,
	})
	e.value.Add(1)
}

func (m *Metrics) WritePrometheus(w io.Writer) {
	fprintln := func(format string, args ...interface{}) {
		fmt.Fprintf(w, format+"\n", args...)
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

	if m.gateway != nil {
		m.gateway.rlMu.Lock()
		for host, rl := range m.gateway.rateLimits {
			rl.mu.Lock()
			rpm := rl.rpmBucket
			tpm := rl.tpmBucket
			rl.mu.Unlock()
			fprintln("# HELP nenya_ratelimit_rpm_available Current RPM bucket available.")
			fprintln("# TYPE nenya_ratelimit_rpm_available gauge")
			fprintln(`nenya_ratelimit_rpm_available{host="%s"} %g`, host, rpm)
			fprintln("# HELP nenya_ratelimit_tpm_available Current TPM bucket available.")
			fprintln("# TYPE nenya_ratelimit_tpm_available gauge")
			fprintln(`nenya_ratelimit_tpm_available{host="%s"} %g`, host, tpm)
		}
		m.gateway.rlMu.Unlock()

		m.gateway.agentMu.Lock()
		active := 0
		now := time.Now()
		for _, expiry := range m.gateway.modelCooldowns {
			if expiry.After(now) {
				active++
			}
		}
		m.gateway.agentMu.Unlock()
		fprintln("# HELP nenya_agent_active_cooldowns Number of currently active model cooldowns.")
		fprintln("# TYPE nenya_agent_active_cooldowns gauge")
		fprintln("nenya_agent_active_cooldowns %d", active)
	}
}

func (m *Metrics) writeCounterMap(w io.Writer, name, help string, mmap *sync.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	var entries []*labeledEntry
	mmap.Range(func(_, v interface{}) bool {
		entries = append(entries, v.(*labeledEntry))
		return true
	})
	sort.Slice(entries, func(i, j int) bool {
		return labelKey(entries[i].labels) < labelKey(entries[j].labels)
	})
	for _, e := range entries {
		fmt.Fprintf(w, "%s%s %d\n", name, labelStr(e.labels), e.value.Load())
	}
}

func (m *Metrics) writeCounterAtomic(w io.Writer, name, help string, value uint64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)
	fmt.Fprintf(w, "%s %d\n", name, value)
}

func (m *Metrics) writeHistogramMap(w io.Writer, name, help string, mmap *sync.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)
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
			fmt.Fprintf(w, "%s_bucket%s,le=\"%g\"} %d\n", name, ls[:len(ls)-1], bkt, h.counts[i].Load())
		}
		fmt.Fprintf(w, "%s_bucket%s,le=\"+Inf\"} %d\n", name, ls[:len(ls)-1], h.count.Load())
		fmt.Fprintf(w, "%s_sum%s %g\n", name, ls, float64(h.sumNS.Load())/1e9)
		fmt.Fprintf(w, "%s_count%s %d\n", name, ls, h.count.Load())
	}
}
