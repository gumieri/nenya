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

// tfidfBlocksText builds n distinct keyword blocks separated by blank lines so
// different queries select different middle blocks.
func tfidfBlocksText(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "keyword%d filler%d %s", i, i, strings.Repeat("x", 60))
	}
	return b.String()
}

func tfidfSelectionCfg() config.ContextConfig {
	return config.ContextConfig{TruncationKeepFirstPct: 10, TruncationKeepLastPct: 10}
}

// TestTfidfSelectionCache_FreezesAcrossQueries pins the Phase 014 property: the
// first keep/drop decision for a piece of content is reused for later queries,
// so prior_messages-mode output stops churning as the query evolves.
func TestTfidfSelectionCache_FreezesAcrossQueries(t *testing.T) {
	text := tfidfBlocksText(10)
	cfg := tfidfSelectionCfg()
	const maxSize = 300

	base1 := TruncateTFIDFWithRescue(text, maxSize, "keyword3", cfg, nil)
	base2 := TruncateTFIDFWithRescue(text, maxSize, "keyword7", cfg, nil)
	if base1 == base2 {
		t.Fatalf("test fixture not discriminating: query change did not change the selection")
	}

	memo := NewTfidfSelectionCache(8)
	got1 := TruncateTFIDFWithRescueMemo(text, maxSize, "keyword3", cfg, nil, "", memo)
	if memo.size() != 1 {
		t.Fatalf("expected one memo entry after first scoring, got %d", memo.size())
	}
	got2 := TruncateTFIDFWithRescueMemo(text, maxSize, "keyword7", cfg, nil, "", memo)

	if got1 != base1 {
		t.Errorf("first scoring did not match the memo-less result")
	}
	if got2 != got1 {
		t.Errorf("selection changed across queries despite the memo:\nfirst  %q\nsecond %q", got1, got2)
	}
	if memo.size() != 1 {
		t.Errorf("a new query must not create a new memo entry; got %d", memo.size())
	}
}

// TestTfidfSelectionCache_ScopeSeparates verifies the scope (agent/judge
// identity) is part of the key, so one agent's selection is never replayed to
// another.
func TestTfidfSelectionCache_ScopeSeparates(t *testing.T) {
	text := tfidfBlocksText(10)
	cfg := tfidfSelectionCfg()
	memo := NewTfidfSelectionCache(8)

	TruncateTFIDFWithRescueMemo(text, 300, "keyword3", cfg, nil, "agent-a", memo)
	TruncateTFIDFWithRescueMemo(text, 300, "keyword7", cfg, nil, "agent-b", memo)
	if memo.size() != 2 {
		t.Errorf("different scopes must be distinct memo entries; got %d", memo.size())
	}
}

// TestTfidfSelectionCache_BudgetClassSeparates verifies a different rune budget
// is a distinct selection class (not reused).
func TestTfidfSelectionCache_BudgetClassSeparates(t *testing.T) {
	text := tfidfBlocksText(10)
	cfg := tfidfSelectionCfg()
	memo := NewTfidfSelectionCache(8)

	TruncateTFIDFWithRescueMemo(text, 300, "keyword3", cfg, nil, "", memo)
	TruncateTFIDFWithRescueMemo(text, 360, "keyword3", cfg, nil, "", memo)
	if memo.size() != 2 {
		t.Errorf("different budgets must be distinct memo entries; got %d", memo.size())
	}
}

// TestTfidfSelectionCache_RescorerPresenceSeparates verifies an armed rescue
// (non-nil Rescorer) hashes distinctly, while a nil rescue and a rescue with no
// Rescorer (identical behavior) share a key.
func TestTfidfSelectionCache_RescorerPresenceSeparates(t *testing.T) {
	text := tfidfBlocksText(10)
	cfg := tfidfSelectionCfg()
	unarmed := &TfidfRescueParams{Band: 0.2, MaxBlocks: 4, MaxBytes: 400}
	armed := &TfidfRescueParams{Band: 0.2, MaxBlocks: 4, MaxBytes: 400, Rescorer: func(string, []Block) map[int]bool { return nil }}

	if tfidfSelectionKey("", text, 300, cfg, nil) != tfidfSelectionKey("", text, 300, cfg, unarmed) {
		t.Errorf("nil rescue and no-Rescorer rescue must share a key (same behavior)")
	}
	if tfidfSelectionKey("", text, 300, cfg, nil) == tfidfSelectionKey("", text, 300, cfg, armed) {
		t.Errorf("an armed rescue must hash distinctly from a deterministic pass")
	}
}

// TestTfidfSelectionCache_LRUBound verifies the memo is size-capped and evicts
// the least-recently-used entry.
func TestTfidfSelectionCache_LRUBound(t *testing.T) {
	cfg := tfidfSelectionCfg()
	memo := NewTfidfSelectionCache(2)

	for i := 0; i < 3; i++ {
		text := tfidfBlocksText(10) + fmt.Sprintf(" unique%d", i)
		TruncateTFIDFWithRescueMemo(text, 300, "keyword3", cfg, nil, "", memo)
	}
	if memo.size() != 2 {
		t.Errorf("memo exceeded its bound: %d entries", memo.size())
	}
	if _, ok := memo.get(tfidfSelectionKey("", tfidfBlocksText(10)+" unique0", 300, cfg, nil)); ok {
		t.Errorf("oldest entry was not evicted")
	}
	// The two most recent entries must still be resident (catches a FIFO or
	// insertion-order regression that would also pass the negative check).
	if _, ok := memo.get(tfidfSelectionKey("", tfidfBlocksText(10)+" unique2", 300, cfg, nil)); !ok {
		t.Errorf("newest entry was evicted")
	}
	if _, ok := memo.get(tfidfSelectionKey("", tfidfBlocksText(10)+" unique1", 300, cfg, nil)); !ok {
		t.Errorf("second-newest entry was evicted")
	}
}

// TestTfidfSelectionCache_Concurrent exercises the memo under the race
// detector, verifies the bound holds under concurrency, and that resident
// content keeps its frozen selection regardless of query.
func TestTfidfSelectionCache_Concurrent(t *testing.T) {
	cfg := tfidfSelectionCfg()
	const bound = 16
	memo := NewTfidfSelectionCache(bound)

	// 24 distinct contents exceed the bound, exercising eviction.
	texts := make([]string, 24)
	for i := range texts {
		texts[i] = tfidfBlocksText(10) + fmt.Sprintf(" distinct%d", i)
	}
	ref := make([]string, len(texts))
	for i, text := range texts {
		ref[i] = TruncateTFIDFWithRescueMemo(text, 300, "warm", cfg, nil, "shared", memo)
	}
	if memo.size() != bound {
		t.Fatalf("memo should hold exactly the bound after overflow (fixture must produce selections): got %d", memo.size())
	}

	// Only the last `bound` contents remain resident; concurrent reads must
	// reuse their frozen selection for any query.
	resident := texts[len(texts)-bound:]
	residentRef := ref[len(ref)-bound:]
	const workers = 24
	outs := make([]string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx := i % len(resident)
			outs[i] = TruncateTFIDFWithRescueMemo(resident[idx], 300, fmt.Sprintf("keyword%d", i%12), cfg, nil, "shared", memo)
		}(i)
	}
	wg.Wait()

	if memo.size() > bound {
		t.Errorf("memo exceeded its bound under concurrency: %d", memo.size())
	}
	for i := range workers {
		if want := residentRef[i%len(resident)]; outs[i] != want {
			t.Errorf("worker %d did not reuse the frozen selection:\ngot  %q\nwant %q", i, outs[i], want)
		}
	}
}

// TestTfidfSelectionCache_NilDisabled verifies a nil memo is safe (memoization
// off).
func TestTfidfSelectionCache_NilDisabled(t *testing.T) {
	text := tfidfBlocksText(10)
	cfg := tfidfSelectionCfg()
	if got := TruncateTFIDFWithRescueMemo(text, 300, "keyword3", cfg, nil, "", nil); got != TruncateTFIDFWithRescue(text, 300, "keyword3", cfg, nil) {
		t.Errorf("nil memo changed the result")
	}
}

// TestTFIDFInterceptor_MemoFreezesAcrossPriorMessages exercises the wired
// interceptor path: the last message's pruned content must stay identical as
// the prior-messages query changes.
func TestTFIDFInterceptor_MemoFreezesAcrossPriorMessages(t *testing.T) {
	cfg := tfidfSelectionCfg()
	last := tfidfBlocksText(10)

	interceptorReq := func(prior string) *InterceptRequest {
		lastMsg := map[string]any{"role": "user", "content": last}
		return &InterceptRequest{
			Payload: map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": prior},
				lastMsg,
			}},
			Messages: []map[string]any{
				{"role": "user", "content": prior},
				lastMsg,
			},
			SoftLimit:  50,
			HardLimit:  100,
			TokenCount: 500,
		}
	}
	run := func(ic *TFIDFInterceptor, prior string) string {
		req := interceptorReq(prior)
		if _, err := ic.Process(context.Background(), req); err != nil {
			t.Fatalf("Process: %v", err)
		}
		return req.Messages[1]["content"].(string)
	}

	// Without a memo the two prior queries must select differently, proving the
	// fixture is discriminating.
	discardLogger := slog.New(slog.NewTextHandler(io.Discard, nil))
	noMemo := NewTFIDFInterceptor(TFIDFInterceptorOpts{QuerySource: "prior_messages", ContextCfg: cfg, Logger: discardLogger})
	if run(noMemo, "keyword3") == run(noMemo, "keyword7") {
		t.Fatalf("test fixture not discriminating at the interceptor level")
	}

	memo := NewTfidfSelectionCache(8)
	ic := NewTFIDFInterceptor(TFIDFInterceptorOpts{QuerySource: "prior_messages", ContextCfg: cfg, Logger: discardLogger, SelectionCache: memo})
	if got1, got2 := run(ic, "keyword3"), run(ic, "keyword7"); got1 != got2 {
		t.Errorf("interceptor output changed across prior messages despite the memo:\nfirst  %q\nsecond %q", got1, got2)
	}
}

// TestTfidfSelectionCache_FailedRescueNotFrozen verifies a transient rescue
// failure is never memoized, so the next call re-judges instead of freezing the
// fallback selection.
func TestTfidfSelectionCache_FailedRescueNotFrozen(t *testing.T) {
	text := tfidfBlocksText(10)
	cfg := tfidfSelectionCfg()
	memo := NewTfidfSelectionCache(8)

	calls := 0
	rescue := &TfidfRescueParams{Band: 100, MaxBlocks: 100, MaxBytes: 100000}
	rescue.Rescorer = func(string, []Block) map[int]bool {
		calls++
		rescue.Failed = true
		return nil
	}

	TruncateTFIDFWithRescueMemo(text, 300, "keyword3", cfg, rescue, "", memo)
	if memo.size() != 0 {
		t.Fatalf("a failed rescue was memoized")
	}
	TruncateTFIDFWithRescueMemo(text, 300, "keyword3", cfg, rescue, "", memo)
	if calls < 2 {
		t.Errorf("failed rescue was not re-attempted on the next call: calls=%d", calls)
	}
}
