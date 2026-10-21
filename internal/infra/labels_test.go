package infra

import (
	"strings"
	"testing"
	"unsafe"
)

func TestPlausibleModelLabel(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"gpt-4o", true},
		{"anthropic/claude-4-sonnet", true},
		{"glm-4.6", true},
		{"openai/gpt-5-mini:free", true},
		{"", false},
		{"has space", false},
		{"new\nline", false},
		{"tab\there", false},
		{strings.Repeat("a", MaxModelLabelChars+1), false},
		{strings.Repeat("a", MaxModelLabelChars), true},
		{"emoji🚀model", false},
	}
	for _, tc := range cases {
		if got := PlausibleModelLabel(tc.model); got != tc.want {
			t.Fatalf("PlausibleModelLabel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// Hostile model names collapse to the two sentinels: charset/length violations
// to one label, valid-looking newcomers past the series budget to the other.
// The distinct-series space stays bounded no matter how many requests arrive.
func TestModelLabelGuardBoundsCardinality(t *testing.T) {
	g := NewModelLabelGuard(4)

	// Legitimate models pass through and are remembered (budget 4).
	for _, m := range []string{"gpt-4o", "claude-4-sonnet", "gemini-3-pro", "glm-4.6"} {
		out := g.Label(m)
		if out.Label != m || out.Invalid || out.Overflow {
			t.Fatalf("known model %q must pass through: %+v", m, out)
		}
	}

	// Hostile inputs: control characters, spaces, and oversized junk all
	// collapse to the SAME sentinel.
	for _, hostile := range []string{"bad name", "x\ny", strings.Repeat("j", 500), "emoji🚀"} {
		out := g.Label(hostile)
		if out.Label != InvalidModelLabel || !out.Invalid {
			t.Fatalf("hostile model %q must collapse to the invalid sentinel: %+v", hostile, out)
		}
	}

	// The budget is full: every NEW valid-looking id collapses to the SAME
	// overflow sentinel — cardinality stays bounded at max + 2 sentinels.
	for i := 0; i < 100; i++ {
		model := "attacker-model-" + strings.Repeat("x", i%48+1)
		out := g.Label(model)
		if out.Label != OverflowModelLabel || !out.Overflow {
			t.Fatalf("post-budget model must collapse to the overflow sentinel: model=%q plausible=%v len=%d out=%+v", model, PlausibleModelLabel(model), len(model), out)
		}
	}

	// A remembered model still passes after the budget is exhausted.
	if out := g.Label("gpt-4o"); out.Label != "gpt-4o" {
		t.Fatalf("a remembered model must keep its label after exhaustion: %+v", out)
	}
}

func TestNewModelLabelGuardDefaultBudget(t *testing.T) {
	g := NewModelLabelGuard(0)
	out := g.Label("gpt-4o")
	if out.Label != "gpt-4o" {
		t.Fatalf("non-positive max means the default budget: %+v", out)
	}
}

// The usage tracker collapses hostile model keys and surfaces the collapse
// counts, so /statsz cannot grow without bound either.
func TestUsageTrackerBoundsModelKeys(t *testing.T) {
	u := NewUsageTracker()
	for i := 0; i < 20; i++ {
		u.RecordRequest("bad name "+strings.Repeat("y", i), 1)
	}
	u.RecordRequest("gpt-4o", 7)

	snap := u.Snapshot()
	violations, ok := snap["label_violations"].(map[string]interface{})
	invalid := uint64(0)
	if ok {
		invalid, _ = violations["invalid"].(uint64)
	}
	if !ok || invalid != 20 {
		t.Fatalf("all hostile keys must collapse and count: %+v", snap["label_violations"])
	}
	models, _ := snap["models"].(map[string]interface{})
	if len(models) != 2 {
		t.Fatalf("exactly the legitimate key and the sentinel must exist: %+v", models)
	}
	if _, legit := models["gpt-4o"]; !legit {
		t.Fatalf("the legitimate model must keep its name: %+v", models)
	}
	sentinel, ok := models[InvalidModelLabel].(map[string]interface{})
	if !ok {
		t.Fatalf("the collapsed requests must be counted under the sentinel: %+v", models)
	}
	if got, _ := sentinel["requests"].(uint64); got != 20 {
		t.Fatalf("all 20 hostile requests must be counted under the sentinel, got %v", sentinel["requests"])
	}
}

func TestMetricsModelLabelSentinels(t *testing.T) {
	m := NewMetrics()

	if got := m.ModelLabel(""); got != UnknownModelLabel {
		t.Fatalf("an empty model must map to the unknown label: %q", got)
	}
	if got := m.ModelLabel("gpt-4o"); got != "gpt-4o" {
		t.Fatalf("a plausible model passes through: %q", got)
	}
	if got := m.ModelLabel("bad name"); got != InvalidModelLabel {
		t.Fatalf("a hostile model collapses: %q", got)
	}

	var sb strings.Builder
	m.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_model_label_violations_total{kind="invalid"} 1`) {
		t.Fatalf("the violation must be counted:\n%s", sb.String())
	}
}

// The guard must not pin the caller's buffer: a plausible model carved from a
// request body (zero-copy substring) is CLONED before entering the seen set,
// so a 16-byte alias of a large body does not pin the body for the process
// lifetime (NENYA-198 retention class, NENYA-144).
func TestModelLabelGuardDoesNotPinCallerBuffer(t *testing.T) {
	g := NewModelLabelGuard(DefaultMaxModelSeries)

	payload := make([]byte, 1<<20) // a body-sized buffer
	for i := range payload {
		payload[i] = 'a'
	}
	model := unsafe.String(&payload[0], 16) // "aaaaaaaaaaaaaaaa", zero-copy

	out := g.Label(model)
	if out.Label != model[:16] {
		t.Fatalf("a plausible model must pass through: %+v", out)
	}
	g.mu.Lock()
	var stored string
	for k := range g.seen {
		stored = k
	}
	g.mu.Unlock()
	if stored == "" {
		t.Fatal("the guard must remember the label")
	}
	assertNotAliased(t, "guard seen key", stored, model)
}

// TestRecordMCPServerStatusCollapse covers the defensive collapse of an
// unknown status to "other" (NENYA-152).
func TestRecordMCPServerStatusCollapse(t *testing.T) {
	m := NewMetrics()
	m.RecordMCPServerStatus("srv", "bogus")
	var sb strings.Builder
	m.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_mcp_server_status_total{server="srv", status="other"} 1`) {
		t.Fatalf("an unknown status must collapse to other:\n%s", sb.String())
	}
}
