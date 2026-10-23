package pipeline

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
)

// TestWindowSummaryFallbackNotCached pins the AUD-11 fix (NENYA-219): when
// the summarize-mode engine is unusable, the deterministic-truncation
// FALLBACK must not be cached as if it were a summary. Pre-fix the fallback
// was stored against the conversation lineage, so a later turn of the same
// conversation got an instant cache hit and replayed truncated history
// instead of retrying the engine — one transient failure permanently
// downgraded the conversation.
func TestWindowSummaryFallbackNotCached(t *testing.T) {
	cache := NewSummaryCache(16)
	var calls atomicCounter
	var failing atomic.Bool
	failing.Store(true) // engine is down for the first compaction

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"engine down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"summary-` +
			uint64String(n) + `"}}]}`))
	}))
	defer up.Close()

	provider := &config.Provider{URL: up.URL, MaxRetryAttempts: 1}
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
		SummaryMaxRunes: 64,
		Engine: config.EngineRef{
			ResolvedTargets: []config.EngineTarget{{Provider: provider}},
		},
	}
	messages := []interface{}{
		msg("user", "alpha"), msg("assistant", "bravo"),
		msg("user", "charlie"), msg("assistant", "delta"),
	}
	serialize := func(p map[string]interface{}) string {
		b, _ := json.Marshal(p)
		return string(b)
	}

	// First compaction: the engine fails, the deterministic fallback covers
	// the history.
	payload1 := map[string]interface{}{"messages": append([]interface{}{}, messages...)}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload1, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 }); err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	firstCalls := calls.Load()
	if firstCalls == 0 {
		t.Fatal("the engine was never consulted")
	}
	cache.mu.Lock()
	gotEntries := len(cache.entries)
	cache.mu.Unlock()
	if gotEntries != 0 {
		t.Fatalf("the deterministic fallback was cached as a summary (%d cache entries)", gotEntries)
	}
	// SummaryMaxRunes is small enough that the fallback is genuinely
	// truncated: it must not claim to be a summary.
	if strings.Contains(serialize(payload1), "summary-") {
		t.Fatal("the fallback compaction must not carry an engine summary")
	}

	// Engine recovers: the next compaction of the SAME conversation must
	// retry it (cache miss — nothing was cached) and cache the real summary.
	// Pre-fix this was an instant cache hit on the truncated fallback and
	// the engine was never consulted again.
	failing.Store(false)
	payload2 := map[string]interface{}{"messages": append([]interface{}{}, messages...)}
	if _, err := ApplyWindowCompaction(context.Background(), deps, payload2, messages, 500, cfg, 100, func(map[string]interface{}) int { return 10 }); err != nil {
		t.Fatalf("second compaction: %v", err)
	}
	if calls.Load() <= firstCalls {
		t.Fatalf("the engine was not retried after the fallback (calls stayed at %d)", firstCalls)
	}
	cache.mu.Lock()
	gotEntries = len(cache.entries)
	cache.mu.Unlock()
	if gotEntries == 0 {
		t.Fatal("the recovered engine summary was not cached")
	}
	// The cached head is the ENGINE summary (the compaction wraps it in the
	// summary envelope, so assert on the cache entry itself).
	cache.mu.Lock()
	var cachedSummary string
	for _, elem := range cache.entries {
		cachedSummary = elem.Value.(*summaryCacheEntry).summary
	}
	cache.mu.Unlock()
	if !strings.Contains(cachedSummary, "summary-") {
		t.Fatalf("the cached head is not the engine summary: %q", cachedSummary)
	}
	if strings.Contains(serialize(payload2), "alpha") {
		t.Fatalf("the engine summary must replace truncated history: %s", serialize(payload2))
	}
}
