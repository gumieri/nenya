package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/infra"
)

func TestSelectCredentialForPreferredAccount(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := auth.NewAccountManager(nil)
	mgr.RegisterPool("prov", auth.NewAccountPool("prov", []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
	}))
	bt := billing.NewBillingTracker(logger, nil)
	metrics := infra.NewMetrics()
	g := &NenyaGateway{AccountManager: mgr, BillingTracker: bt, Metrics: metrics, Logger: logger}
	ctx := context.Background()

	// Healthy preferred account resolves through the exported interface method.
	cred, acct, ok := g.SelectCredentialForPreferredAccount(ctx, "prov", "m1", "a1")
	if !ok || acct != "a1" || cred != "key1" {
		t.Fatalf("expected healthy preferred account, got ok=%v acct=%q cred=%q", ok, acct, cred)
	}
	if got := metrics.AccountSelectionCount("prov", "selected_preferred"); got != 1 {
		t.Fatalf("expected 1 'selected_preferred' metric, got %d", got)
	}
	if got := metrics.AccountSelectionCount("prov", "none_available"); got != 0 {
		t.Fatalf("expected no 'none_available' metric on success, got %d", got)
	}

	// Billing-exhausted account is rejected before the pool touch: no
	// LastUsed bump (LRU position preserved) and no misleading miss metric.
	before := mgr.ListAccounts("prov")[0].LastUsed
	bt.MarkExhausted(ctx, "prov", "a1", "quota")
	if _, _, ok := g.SelectCredentialForPreferredAccount(ctx, "prov", "m1", "a1"); ok {
		t.Fatal("expected billing-exhausted account to be rejected")
	}
	after := mgr.ListAccounts("prov")[0].LastUsed
	if !after.Equal(before) {
		t.Fatal("rejected account must not have LastUsed bumped")
	}
	if got := metrics.AccountSelectionCount("prov", "selected_preferred"); got != 1 {
		t.Fatalf("rejection must not bump 'selected_preferred' metric, got %d", got)
	}

	// Unknown and empty account IDs are rejected.
	if _, _, ok := g.SelectCredentialForPreferredAccount(ctx, "prov", "m1", "missing"); ok {
		t.Fatal("expected unknown account to be rejected")
	}
	if _, _, ok := g.SelectCredentialForPreferredAccount(ctx, "prov", "m1", ""); ok {
		t.Fatal("expected empty accountID to be rejected")
	}
	if got := metrics.AccountSelectionCount("prov", "none_available"); got != 0 {
		t.Fatalf("misses must not record account-selection metrics, got %d", got)
	}

	// No account manager configured.
	bare := &NenyaGateway{}
	if _, _, ok := bare.SelectCredentialForPreferredAccount(ctx, "prov", "m1", "a1"); ok {
		t.Fatal("expected not-ok without account manager")
	}
}

func TestSelectCredentialForModel_SkipsExhaustedLRUPick(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := auth.NewAccountManager(nil)
	mgr.RegisterPool("prov", auth.NewAccountPool("prov", []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
		{ID: "a2", CredentialType: config.CredentialTypeAPIKey, Credential: "key2", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
	}))
	bt := billing.NewBillingTracker(logger, nil)
	metrics := infra.NewMetrics()
	g := &NenyaGateway{AccountManager: mgr, BillingTracker: bt, Metrics: metrics, Logger: logger}
	ctx := context.Background()

	// LRU selects a1 first; mark it exhausted afterwards.
	if _, id, ok := g.SelectCredentialForModel(ctx, "prov", "m1"); !ok || id != "a1" {
		t.Fatalf("expected LRU pick a1, got %q, %v", id, ok)
	}
	bt.MarkExhausted(ctx, "prov", "a1", "quota")

	// The LRU path must skip the exhausted account and serve the sibling,
	// recorded as a plain usable selection.
	_, id, ok := g.SelectCredentialForModel(ctx, "prov", "m1")
	if !ok || id != "a2" {
		t.Fatalf("expected exhaustion-aware sibling a2, got %q, %v", id, ok)
	}
	if got := metrics.AccountSelectionCount("prov", "selected"); got != 2 {
		t.Fatalf("expected 2 'selected' metrics (initial pick + healthy sibling), got %d", got)
	}

	// When every account is exhausted the exclusion retry still returns a
	// pick (the pool is billing-agnostic); the metric must be honest that
	// the pick is a doomed fallback (the downstream chat target-build filter
	// drops it), not a usable selection.
	bt.MarkExhausted(ctx, "prov", "a2", "quota")
	_, id, ok = g.SelectCredentialForModel(ctx, "prov", "m1")
	if !ok || (id != "a1" && id != "a2") {
		t.Fatalf("expected some pick when all accounts exhausted, got %q, %v", id, ok)
	}
	if got := metrics.AccountSelectionCount("prov", "selected_exhausted_fallback"); got != 1 {
		t.Fatalf("expected 1 'selected_exhausted_fallback' metric, got %d", got)
	}
	if got := metrics.AccountSelectionCount("prov", "selected"); got != 2 {
		t.Fatalf("exhausted fallback must not bump 'selected', got %d", got)
	}
}

func TestSelectCredentialForModel_SkipsMultipleExhausted(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := auth.NewAccountManager(nil)
	mgr.RegisterPool("prov", auth.NewAccountPool("prov", []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
		{ID: "a2", CredentialType: config.CredentialTypeAPIKey, Credential: "key2", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
		{ID: "a3", CredentialType: config.CredentialTypeAPIKey, Credential: "key3", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
	}))
	bt := billing.NewBillingTracker(logger, nil)
	g := &NenyaGateway{AccountManager: mgr, BillingTracker: bt, Metrics: infra.NewMetrics(), Logger: logger}
	ctx := context.Background()

	// With a1 and a2 billing-exhausted, the exhaustion-aware retry must walk
	// past BOTH and land on the healthy a3 instead of returning a doomed
	// pick that the downstream target filter would drop.
	bt.MarkExhausted(ctx, "prov", "a1", "quota")
	bt.MarkExhausted(ctx, "prov", "a2", "quota")

	cred, id, ok := g.SelectCredentialForModel(ctx, "prov", "m1")
	if !ok || id != "a3" || cred != "key3" {
		t.Fatalf("expected healthy sibling a3 past multiple exhausted accounts, got id=%q cred=%q ok=%v", id, cred, ok)
	}
}
