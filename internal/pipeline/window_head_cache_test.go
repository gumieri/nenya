package pipeline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nenya/config"
)

func whHistory(contents ...string) []interface{} {
	out := make([]interface{}, len(contents))
	for i, c := range contents {
		out[i] = msg("user", c)
	}
	return out
}

// TestWindowHeadCache_NonContentMutationKeepsHead pins the intentional
// semantics: the covered region is replaced by the head, so non-content fields
// (tool_calls, reasoning_content, …) never reach the model through it and do
// not invalidate the head. Only content changes do.
func TestWindowHeadCache_NonContentMutationKeepsHead(t *testing.T) {
	c := NewWindowHeadCache(8)
	assistant := map[string]interface{}{
		"role":    "assistant",
		"content": "hello",
		"tool_calls": []interface{}{
			map[string]interface{}{"id": "call-1"},
		},
	}
	h := []interface{}{msg("user", "a"), assistant, msg("user", "b")}
	c.Store("L", SerializeMessages(h), len(h), 0, "HEAD")

	// Same content, different tool_calls id: SerializeMessages is unchanged,
	// so the head is still valid.
	assistant2 := map[string]interface{}{
		"role":    "assistant",
		"content": "hello",
		"tool_calls": []interface{}{
			map[string]interface{}{"id": "call-2"},
		},
	}
	h2 := []interface{}{msg("user", "a"), assistant2, msg("user", "b")}
	if _, ok := c.Lookup("L", SerializeMessages(h2), len(h2), 0.25); !ok {
		t.Error("non-content mutation inside the covered region must not invalidate the head")
	}
}

// TestWindowHeadCache_ExactReuse pins retry stability: identical history
// reuses the stored head in full.
func TestWindowHeadCache_ExactReuse(t *testing.T) {
	c := NewWindowHeadCache(8)
	h := whHistory("a", "b", "c")
	c.Store("L", SerializeMessages(h), len(h), 0, "HEAD")

	r, ok := c.Lookup("L", SerializeMessages(h), len(h), 0.25)
	if !ok {
		t.Fatal("expected exact reuse")
	}
	if r.Head != "HEAD" || !r.Exact || r.DeltaStart != 3 {
		t.Errorf("unexpected reuse: %+v", r)
	}
}

// TestWindowHeadCache_AppendOnlyHysteresis pins the core property: append-only
// growth inside the regeneration ratio reuses the head and only fills the
// delta.
func TestWindowHeadCache_AppendOnlyHysteresis(t *testing.T) {
	c := NewWindowHeadCache(8)
	h := whHistory("a", "b", "c", "d", "e", "f", "g", "h", "i", "j") // 10
	c.Store("L", SerializeMessages(h), len(h), 0, "HEAD")

	grown := append(append([]interface{}{}, h...), msg("user", "k")) // 11 → +10% < 25%
	r, ok := c.Lookup("L", SerializeMessages(grown), len(grown), 0.25)
	if !ok {
		t.Fatal("expected hysteresis reuse for append-only growth")
	}
	if r.Exact || r.DeltaStart != 10 || r.Head != "HEAD" {
		t.Errorf("unexpected reuse: %+v", r)
	}
	if hyst := c.Stats().HystHits; hyst != 1 {
		t.Errorf("expected 1 hysteresis hit, got %d", hyst)
	}
}

// TestWindowHeadCache_MutationInvalidates pins the region check: mutating a
// message inside the frozen prefix forces a recompute (never a stale head
// appended to changed content).
func TestWindowHeadCache_MutationInvalidates(t *testing.T) {
	c := NewWindowHeadCache(8)
	h := whHistory("a", "b", "c", "d")
	c.Store("L", SerializeMessages(h), len(h), 0, "HEAD")

	mutated := whHistory("a", "MUTATED", "c", "d") // same length, changed member
	if _, ok := c.Lookup("L", SerializeMessages(mutated), len(mutated), 0.25); ok {
		t.Fatal("mutation inside the covered region must invalidate the head")
	}
	if inv := c.Stats().Invalidated; inv != 1 {
		t.Errorf("expected 1 invalidation, got %d", inv)
	}
}

// TestWindowHeadCache_ShrinkInvalidates pins that a history shorter than the
// covered region cannot reuse the head.
func TestWindowHeadCache_ShrinkInvalidates(t *testing.T) {
	c := NewWindowHeadCache(8)
	h := whHistory("a", "b", "c", "d")
	c.Store("L", SerializeMessages(h), len(h), 0, "HEAD")

	if _, ok := c.Lookup("L", SerializeMessages(h[:3]), 3, 0.25); ok {
		t.Fatal("shrunken history must invalidate the head")
	}
}

// TestWindowHeadCache_RegenRatioTrips pins that growth past the ratio
// recomputes instead of reusing.
func TestWindowHeadCache_RegenRatioTrips(t *testing.T) {
	c := NewWindowHeadCache(8)
	h := whHistory("a", "b", "c", "d") // 4
	c.Store("L", SerializeMessages(h), len(h), 0, "HEAD")

	grown := append(append([]interface{}{}, h...), whHistory("e", "f", "g", "h")...) // 8 → +100%
	if _, ok := c.Lookup("L", SerializeMessages(grown), len(grown), 0.25); ok {
		t.Fatal("growth past the regen ratio must miss")
	}
}

// TestWindowHeadCache_LRUBound pins bounded memory across lineages.
func TestWindowHeadCache_LRUBound(t *testing.T) {
	c := NewWindowHeadCache(2)
	for i := 0; i < 3; i++ {
		h := whHistory(fmt.Sprintf("lineage%d", i))
		c.Store(fmt.Sprintf("L%d", i), SerializeMessages(h), len(h), 0, "HEAD")
	}
	if c.size() != 2 {
		t.Fatalf("cache exceeded cap: %d", c.size())
	}
}

// TestWindowHeadCache_Concurrent exercises the cache under the race detector.
func TestWindowHeadCache_Concurrent(t *testing.T) {
	c := NewWindowHeadCache(8)
	h := whHistory("a", "b", "c", "d")
	text := SerializeMessages(h)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Store("L", text, len(h), 0, "HEAD")
			_, _ = c.Lookup("L", SerializeMessages(h), len(h), 0.25)
		}()
	}
	wg.Wait()
	if c.size() != 1 {
		t.Errorf("expected a single lineage entry, got %d", c.size())
	}
}

// TestApplyWindowCompaction_TfidfHeadStableAcrossTurns pins the end-to-end
// property: the compacted head is byte-identical as the history grows by
// appends, and the appended delta survives verbatim.
func TestApplyWindowCompaction_TfidfHeadStableAcrossTurns(t *testing.T) {
	deps := WindowDeps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		HeadCache:   NewWindowHeadCache(8),
		CountTokens: func(text string) int { return len(text) / 4 },
	}
	cfg := config.WindowConfig{
		Enabled:         true,
		Mode:            "tfidf",
		TriggerRatio:    0.1,
		MaxContext:      100,
		ActiveMessages:  2,
		SummaryMaxRunes: 200,
		KeepFirstPct:    15,
		KeepLastPct:     25,
	}
	active := []interface{}{msg("user", "current question"), msg("assistant", "answer")}

	hist := func(n int) []interface{} {
		out := []interface{}{msg("system", "sys")}
		for i := 1; i <= n; i++ {
			out = append(out, msg("user", fmt.Sprintf("block%d\n\n%s", i, strings.Repeat("x", 80))))
		}
		return out
	}
	apply := func(h []interface{}) []interface{} {
		all := append(append([]interface{}{}, h...), active...)
		payload := map[string]interface{}{"messages": all}
		ok, err := ApplyWindowCompaction(context.Background(), deps, payload, all, 500, cfg, 100, func(map[string]interface{}) int { return 50 })
		if err != nil || !ok {
			t.Fatalf("apply: ok=%v err=%v", ok, err)
		}
		return payload["messages"].([]interface{})
	}

	m1 := apply(hist(6)) // history: 6 messages
	m2 := apply(hist(7)) // history: 7 messages (append-only growth)

	head1 := m1[1].(map[string]interface{})["content"].(string)
	head2 := m2[1].(map[string]interface{})["content"].(string)
	if head1 != head2 {
		t.Errorf("tfidf head diverged across appended turns:\n%q\nvs\n%q", head1, head2)
	}
	if !strings.Contains(head1, "Nenya Window Summary") {
		t.Errorf("expected a window summary head, got %q", head1)
	}

	// Prove turn 2 reused the frozen head rather than coincidentally
	// regenerating the same text.
	if st := deps.HeadCache.Stats(); st.HystHits != 1 || st.Regens != 1 {
		t.Errorf("expected 1 regen + 1 hysteresis reuse, got regens=%d hyst=%d", st.Regens, st.HystHits)
	}

	// The appended delta message must survive verbatim.
	found := false
	for _, m := range m2 {
		if c, _ := m.(map[string]interface{})["content"].(string); strings.Contains(c, "block7") && strings.Contains(c, "x") {
			found = true
		}
	}
	if !found {
		t.Error("appended delta message lost after hysteresis reuse")
	}
}

// TestApplyWindowCompaction_TfidfNilHeadCache pins that a nil head cache keeps
// the pre-feature behavior (per-request recompute) without panicking.
func TestApplyWindowCompaction_TfidfNilHeadCache(t *testing.T) {
	deps := WindowDeps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cfg := config.WindowConfig{Enabled: true, Mode: "tfidf", TriggerRatio: 0.1, MaxContext: 100, ActiveMessages: 2, SummaryMaxRunes: 200}
	h := whHistory("a", "b", "c", "d", "e", "f")
	all := append(append([]interface{}{}, h...), msg("user", "q"), msg("assistant", "a"))
	payload := map[string]interface{}{"messages": all}
	ok, err := ApplyWindowCompaction(context.Background(), deps, payload, all, 500, cfg, 100, func(map[string]interface{}) int { return 50 })
	if err != nil || !ok {
		t.Fatalf("nil head cache: ok=%v err=%v", ok, err)
	}
}

// TestApplyWindowCompaction_TfidfMutationRefreezes pins that a content mutation
// inside the frozen prefix forces a recompute rather than a stale reuse.
func TestApplyWindowCompaction_TfidfMutationRefreezes(t *testing.T) {
	deps := WindowDeps{
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		HeadCache:   NewWindowHeadCache(8),
		CountTokens: func(text string) int { return len(text) / 4 },
	}
	cfg := config.WindowConfig{Enabled: true, Mode: "tfidf", TriggerRatio: 0.1, MaxContext: 100, ActiveMessages: 2, SummaryMaxRunes: 200, KeepFirstPct: 15, KeepLastPct: 25}
	active := []interface{}{msg("user", "current question"), msg("assistant", "answer")}

	hist := func(mutate bool) []interface{} {
		out := []interface{}{msg("system", "sys")}
		for i := 1; i <= 6; i++ {
			body := fmt.Sprintf("block%d\n\n%s", i, strings.Repeat("x", 80))
			if mutate && i == 2 {
				body = "MUTATED block\n\n" + strings.Repeat("y", 80)
			}
			out = append(out, msg("user", body))
		}
		return out
	}
	apply := func(h []interface{}) {
		all := append(append([]interface{}{}, h...), active...)
		payload := map[string]interface{}{"messages": all}
		if ok, err := ApplyWindowCompaction(context.Background(), deps, payload, all, 500, cfg, 100, func(map[string]interface{}) int { return 50 }); err != nil || !ok {
			t.Fatalf("apply: ok=%v err=%v", ok, err)
		}
	}

	apply(hist(false)) // regen + store
	apply(hist(true))  // same message count, mutated covered content

	if st := deps.HeadCache.Stats(); st.Regens != 2 || st.Invalidated != 1 {
		t.Errorf("expected mutation to refreeze (regens=2, invalidated=1), got regens=%d invalidated=%d", st.Regens, st.Invalidated)
	}
}
