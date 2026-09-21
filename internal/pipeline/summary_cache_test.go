package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
)

// newSummarizeHarness builds a fake engine upstream returning
// "summary-<n>" per call, plus WindowDeps wired with a summary cache.
func newSummarizeHarness(t *testing.T, cache *SummaryCache, regenRatio float64) (WindowDeps, config.WindowConfig, *atomicCounter) {
	t.Helper()
	var calls atomicCounter
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"summary-` +
			uint64String(n) + `"}}]}`))
	}))
	t.Cleanup(up.Close)

	provider := &config.Provider{URL: up.URL}
	deps := WindowDeps{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Providers:    map[string]*config.Provider{"engine-provider": provider},
		ClientFor:    func(string) *http.Client { return up.Client() },
		InjectAPIKey: func(string, http.Header) error { return nil },
		CountTokens:  func(string) int { return 1 },
		SummaryCache: cache,
	}
	cfg := config.WindowConfig{
		Enabled:         true,
		Mode:            "summarize",
		TriggerRatio:    0.1,
		MaxContext:      100,
		ActiveMessages:  2,
		SummaryMaxRunes: 5000,
		Engine: config.EngineRef{
			ResolvedTargets: []config.EngineTarget{{Provider: provider}},
		},
	}
	if regenRatio > 0 {
		r := regenRatio
		cfg.SummaryRegenRatio = &r
	}
	return deps, cfg, &calls
}

type atomicCounter struct {
	v atomic.Uint64
}

func (c *atomicCounter) Add(delta uint64) uint64 { return c.v.Add(delta) }
func (c *atomicCounter) Load() uint64            { return c.v.Load() }

func uint64String(n uint64) string { return fmt.Sprintf("%d", n) }

// TestWindowSummaryCache_ExactHit pins retry stability: identical history
// compacts to a byte-identical payload with a single engine call.
func TestWindowSummaryCache_ExactHit(t *testing.T) {
	deps, cfg, calls := newSummarizeHarness(t, NewSummaryCache(16), 0.25)
	messages := []interface{}{
		msg("user", "a"), msg("assistant", "b"),
		msg("user", "c"), msg("assistant", "d"),
	}

	payload1 := map[string]interface{}{"messages": messages}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload1, messages, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}
	payload2 := map[string]interface{}{"messages": messages}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload2, messages, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 1 {
		t.Fatalf("expected exactly 1 engine call, got %d", calls.Load())
	}
	if !payloadsEqual(payload1, payload2) {
		t.Error("identical history must produce byte-identical compacted payloads")
	}
}

// TestWindowSummaryCache_HysteresisDelta pins the cross-turn stability: a
// small history growth reuses the stored summary and appends the delta
// messages verbatim — the compacted prefix stays identical.
func TestWindowSummaryCache_HysteresisDelta(t *testing.T) {
	// The history SLICE grows 2 -> 3 messages (50%); the ratio must exceed
	// that for the summary to be reused.
	deps, cfg, calls := newSummarizeHarness(t, NewSummaryCache(16), 0.6)
	base := []interface{}{
		msg("user", "a"), msg("assistant", "b"),
		msg("user", "c"), msg("assistant", "d"),
	}

	payload1 := map[string]interface{}{"messages": base}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload1, base, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	// One more turn: 5 messages vs 4 = 25% growth < 50% ratio.
	grown := append(append([]interface{}{}, base...), msg("user", "e"))
	payload2 := map[string]interface{}{"messages": grown}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload2, grown, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 1 {
		t.Fatalf("expected summary reuse (1 engine call), got %d calls", calls.Load())
	}

	// The compacted HEAD (summary message) must be byte-identical — that
	// is the prompt-cache stability property. Every message from the first
	// compaction must survive, and the grown turn must not be lost.
	m1 := payload1["messages"].([]interface{})
	m2 := payload2["messages"].([]interface{})
	if serializeMsg(m1[0]) != serializeMsg(m2[0]) {
		t.Errorf("summary head diverged:\n%v\nvs\n%v", m1[0], m2[0])
	}
	contents1 := allContents(m1)
	contents2 := allContents(m2)
	has := func(list []string, want string) bool {
		for _, c := range list {
			if c == want {
				return true
			}
		}
		return false
	}
	for _, c := range contents1 {
		if c == "" || has(contents2, c) {
			continue
		}
		t.Errorf("message content %q lost after hysteresis reuse", c)
	}
	if !has(contents2, "e") {
		t.Errorf("grown delta message lost: %v", contents2)
	}
}

// TestWindowSummaryCache_RegenPastHysteresis checks that growth beyond the
// ratio triggers regeneration with the full history.
func TestWindowSummaryCache_RegenPastHysteresis(t *testing.T) {
	deps, cfg, calls := newSummarizeHarness(t, NewSummaryCache(16), 0.1)
	base := []interface{}{
		msg("user", "a"), msg("assistant", "b"),
		msg("user", "c"), msg("assistant", "d"),
	}
	payload1 := map[string]interface{}{"messages": base}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload1, base, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	// History slice grows 2 -> 4 (100%): far past the 10% ratio.
	grown := append(append([]interface{}{}, base...), msg("user", "e"), msg("assistant", "f"), msg("user", "g"), msg("assistant", "h"))
	payload2 := map[string]interface{}{"messages": grown}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload2, grown, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	if calls.Load() != 2 {
		t.Fatalf("expected regeneration (2 engine calls), got %d", calls.Load())
	}
}

// TestWindowSummaryCache_EngineFailureServesCached pins the fail-open
// behavior: once a summary is cached, an engine outage no longer skips
// compaction — the cached summary plus delta is served.
func TestWindowSummaryCache_EngineFailureServesCached(t *testing.T) {
	deps, cfg, _ := newSummarizeHarness(t, NewSummaryCache(16), 0.5)
	base := []interface{}{
		msg("user", "a"), msg("assistant", "b"),
		msg("user", "c"), msg("assistant", "d"),
	}
	payload1 := map[string]interface{}{"messages": base}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload1, base, 500, cfg, 100, func(p map[string]interface{}) int { return 10 }); err != nil {
		t.Fatal(err)
	}

	// Break the engine; the cached summary must still serve.
	deps.Providers["engine-provider"].URL = "http://127.0.0.1:1/nope"
	grown := append(append([]interface{}{}, base...), msg("user", "e"))
	payload2 := map[string]interface{}{"messages": grown}
	ok, err := ApplyWindowCompaction(context.Background(), deps, payload2, grown, 500, cfg, 100, func(p map[string]interface{}) int { return 10 })
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected compaction to proceed from cache despite engine failure")
	}
}

// TestWindowSummaryCache_LRUBound pins bounded memory across lineages.
func TestWindowSummaryCache_LRUBound(t *testing.T) {
	c := NewSummaryCache(4)
	for i := 0; i < 50; i++ {
		key := string(rune('a'+i%26)) + string(rune('a'+i/26))
		c.Store(key, key+"-hash", 10, "s")
	}
	c.mu.Lock()
	size := len(c.entries)
	c.mu.Unlock()
	if size != 4 {
		t.Fatalf("cache exceeded cap: %d", size)
	}
}

func allContents(msgs []interface{}) []string {
	var parts []string
	for _, m := range msgs {
		msg, _ := m.(map[string]interface{})
		c, _ := msg["content"].(string)
		parts = append(parts, c)
	}
	return parts
}

func payloadsEqual(a, b map[string]interface{}) bool {
	ma := a["messages"].([]interface{})
	mb := b["messages"].([]interface{})
	if len(ma) != len(mb) {
		return false
	}
	for i := range ma {
		if serializeMsg(ma[i]) != serializeMsg(mb[i]) {
			return false
		}
	}
	return true
}

func serializeMsg(m interface{}) string {
	b, _ := json.Marshal(m)
	return string(b)
}
