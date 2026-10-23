package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
)

// TestReloadResponseCacheSharesCarriedMetrics pins the AUD-17 fix: a reload
// carries the process-wide Metrics instance, and the new gateway's
// ResponseCache must report into THAT instance. Pre-fix `newGateway` built a
// fresh infra.Metrics for the cache and Reload then overwrote gw.Metrics with
// the carried one — the cache's hit/miss counters went to a discarded
// instance, so they silently stopped moving after every SIGHUP.
func TestReloadResponseCacheSharesCarriedMetrics(t *testing.T) {
	cfg := testutil.MinimalConfig()
	cfg.ResponseCache.Enabled = config.PtrTo(true)
	cfg.ResponseCache.MaxEntries = 8
	cfg.ResponseCache.TTLSeconds = 60
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	g1 := New(context.Background(), *cfg, secrets, logger)
	t.Cleanup(g1.Close)
	if g1.ResponseCache == nil {
		t.Fatal("expected a response cache with ResponseCache.Enabled")
	}

	carriedMetrics := g1.Metrics
	g2 := g1.Reload(context.Background(), g1.Config, g1.Secrets)
	t.Cleanup(func() { g2.Close() })

	if g2.Metrics != carriedMetrics {
		t.Fatal("reload must carry the process-wide metrics instance")
	}
	if g2.ResponseCache == nil {
		t.Fatal("expected a response cache on the reloaded gateway")
	}

	// Drive cache activity on the RELOADED gateway: a store then a lookup
	// that misses (different key). Both must land in the carried instance.
	g2.ResponseCache.Store("hit-key", "test-model", false, []byte("body"), nil)
	if _, ok, _ := g2.ResponseCache.Lookup("hit-key", "test-model", false, nil); !ok {
		t.Fatal("stored entry must be retrievable")
	}
	if _, ok, _ := g2.ResponseCache.Lookup("miss-key", "test-model", false, nil); ok {
		t.Fatal("unknown key must miss")
	}

	// The cache must hold the CARRIED instance itself, not merely produce
	// counters that happen to reach it.
	if !g2.ResponseCache.UsesMetrics(carriedMetrics) {
		t.Fatal("the reloaded gateway's response cache is wired to a DIFFERENT metrics instance (orphaned metrics)")
	}
}
