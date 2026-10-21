package infra

import (
	"runtime"
	"testing"
	"time"
	"unsafe"
	"weak"
)

// aliasedLabel returns a 16-byte string that aliases buf's backing array with
// zero copying, standing in for a model name parsed straight out of a request
// body (zero-copy JSON parsing can produce such strings). The 16-byte
// non-zero length is required: unsafe.StringData is unspecified for
// zero-length strings, and the aliasing assertions below rely on it.
func aliasedLabel(t *testing.T, buf []byte) string {
	t.Helper()
	if len(buf) < 16 {
		t.Fatalf("test buffer too small: %d bytes", len(buf))
	}
	// The first 16 bytes must form a PLAUSIBLE model label (NENYA-144): the
	// guard collapses implausible ids to a shared sentinel, which would make
	// the aliasing assertion below vacuous.
	for i := 0; i < 16; i++ {
		if buf[i] == 0 {
			buf[i] = 'a'
		}
	}
	return unsafe.String(&buf[0], 16)
}

// assertNotAliased fails the test when got does not equal alias or when got
// shares alias's backing storage, i.e. the retained string still points into
// the request buffer instead of being a fresh copy.
func assertNotAliased(t *testing.T, what, got, alias string) {
	t.Helper()
	if got != alias {
		t.Fatalf("%s: stored %q, want %q", what, got, alias)
	}
	if unsafe.StringData(got) == unsafe.StringData(alias) {
		t.Fatalf("%s retains the request buffer; label values must be cloned before retention", what)
	}
}

// TestCloneLabelsCopiesValues verifies the metrics-boundary helper produces a
// value-equal but independently backed copy, and that nil stays nil.
func TestCloneLabelsCopiesValues(t *testing.T) {
	payload := make([]byte, 64)
	alias := aliasedLabel(t, payload)

	got := cloneLabels(map[string]string{"model": alias})
	if got["model"] != alias {
		t.Fatalf("clone value = %q, want %q", got["model"], alias)
	}
	assertNotAliased(t, "cloneLabels output", got["model"], alias)

	if c := cloneLabels(nil); c != nil {
		t.Fatalf("cloneLabels(nil) = %v, want nil", c)
	}
}

// TestMetricLabelsDoNotAliasRequestBuffers asserts that a metric recorded with
// an aliasing model name stores a fresh copy, not the caller's string. This is
// the deterministic (GC-independent) half of the leak coverage.
func TestMetricLabelsDoNotAliasRequestBuffers(t *testing.T) {
	payload := make([]byte, 256)
	alias := aliasedLabel(t, payload)

	m := NewMetrics()
	// Pass the aliasing string in every label slot so the all-labels assertion
	// below covers each one.
	m.RecordTokens(alias, alias, alias, alias, 1)

	var stored *labeledEntry
	m.tokens.Range(func(_, v any) bool {
		stored = v.(*labeledEntry)
		return false
	})
	if stored == nil {
		t.Fatal("no metric entry recorded")
	}
	if len(stored.labels) != 4 {
		t.Fatalf("metric entry labels = %d, want 4", len(stored.labels))
	}
	for _, k := range []string{"direction", "model", "agent", "provider"} {
		if _, ok := stored.labels[k]; !ok {
			t.Fatalf("metric entry missing label %q", k)
		}
	}
	for k, v := range stored.labels {
		assertNotAliased(t, "metric label "+k, v, alias)
	}
}

// gcPinRounds bounds the collectibility wait in the pin test.
const gcPinRounds = 8

// TestMetricHistogramsDoNotAliasRequestBuffers covers the histogram retention
// path: RecordUpstreamLatency keys a retained histogram by model/agent/
// provider labels, which must be cloned like counter labels.
func TestMetricHistogramsDoNotAliasRequestBuffers(t *testing.T) {
	payload := make([]byte, 256)
	alias := aliasedLabel(t, payload)

	m := NewMetrics()
	m.RecordUpstreamLatency(alias, alias, alias, 25*time.Millisecond)

	var stored *histogram
	m.upstreamLatency.Range(func(_, v any) bool {
		stored = v.(*histogram)
		return false
	})
	if stored == nil {
		t.Fatal("no histogram recorded")
	}
	if len(stored.labels) != 3 {
		t.Fatalf("histogram labels = %d, want 3", len(stored.labels))
	}
	for _, k := range []string{"model", "agent", "provider"} {
		if _, ok := stored.labels[k]; !ok {
			t.Fatalf("histogram missing label %q", k)
		}
	}
	for k, v := range stored.labels {
		assertNotAliased(t, "histogram label "+k, v, alias)
	}
}

// TestTrackersDoNotAliasModelNames covers the non-metrics retention paths that
// key or store caller-supplied model names (all surfaced through /statsz or
// routing): the usage tracker, cost tracker, latency tracker, and calibration
// tracker.
func TestTrackersDoNotAliasModelNames(t *testing.T) {
	payload := make([]byte, 256)
	alias := aliasedLabel(t, payload)
	provAlias := aliasedLabel(t, payload[16:])

	lt := NewLatencyTracker()
	lt.Record(alias, provAlias, time.Millisecond)
	snap := lt.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("latency snapshot entries = %d, want 1", len(snap))
	}
	for _, ml := range snap {
		assertNotAliased(t, "latency Model", ml.Model, alias)
		assertNotAliased(t, "latency Provider", ml.Provider, provAlias)
	}

	ut := NewUsageTracker()
	ut.RecordRequest(alias, 10)
	if len(ut.models) != 1 {
		t.Fatalf("usage tracker keys = %d, want 1", len(ut.models))
	}
	for k := range ut.models {
		assertNotAliased(t, "usage tracker key", k, alias)
	}

	ct := NewCostTracker()
	ct.RecordUsage(alias, 0.5)
	ct.RecordError(alias)
	if len(ct.costs) != 1 || len(ct.errors) != 1 {
		t.Fatalf("cost tracker keys = %d/%d, want 1/1", len(ct.costs), len(ct.errors))
	}
	for k := range ct.costs {
		assertNotAliased(t, "cost tracker key", k, alias)
	}
	for k := range ct.errors {
		assertNotAliased(t, "cost error key", k, alias)
	}

	cal := NewCalibrationTracker(CalibrationParams{Enabled: true})
	cal.RecordEstimate(alias, 10)
	if len(cal.models) != 1 {
		t.Fatalf("calibration keys = %d, want 1", len(cal.models))
	}
	for k := range cal.models {
		assertNotAliased(t, "calibration key", k, alias)
	}
}

// TestMetricLabelsDoNotPinRequestBuffers is the end-to-end leak assertion: a
// metric recorded with a model label aliasing a large request buffer must not
// keep that buffer alive. The payload is built in a //go:noinline helper so
// the buffer's only possible reference after the frame pops is the label
// stored inside the still-live metrics entry (kept alive via
// runtime.KeepAlive) — with verbatim retention the label pins the full
// megabyte for the process lifetime (NENYA-198; upstream agent-router
// #2767). Regression-guarded: reverting the cloneLabels call sites makes this
// test fail.
func TestMetricLabelsDoNotPinRequestBuffers(t *testing.T) {
	m, wp := recordAliasedTokens()
	// Sweep laziness and conservative stack scanning can transiently keep the
	// buffer reachable, so collect in bounded rounds and stop at the first
	// collection that observes the buffer as garbage.
	for i := 0; i < gcPinRounds; i++ {
		runtime.GC()
		if wp.Value() == nil {
			runtime.KeepAlive(m)
			return
		}
	}
	t.Fatalf("request buffer was not observed as collected within %d GC rounds; if this persists across runs the label may be pinning the payload (buffer-pinning leak, NENYA-198)", gcPinRounds)
}

// recordAliasedTokens builds a megabyte request-buffer stand-in, records a
// metric whose labels alias it, and returns the live tracker plus a weak
// pointer into the buffer. Marked //go:noinline so payload and alias become
// dead deterministically when the frame returns.
//
//go:noinline
func recordAliasedTokens() (*Metrics, weak.Pointer[byte]) {
	payload := make([]byte, 1<<20)
	first := &payload[0] // pointer into the buffer's single allocation
	wp := weak.Make(first)
	alias := unsafe.String(first, 16)

	m := NewMetrics()
	m.RecordTokens(alias, alias, alias, alias, 1)
	return m, wp
}
