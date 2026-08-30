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

func TestSelectPreferredAccountKey_BillingGate(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := auth.NewAccountManager(nil)
	mgr.RegisterPool("prov", auth.NewAccountPool("prov", []*config.ProviderAccount{
		{ID: "a1", CredentialType: config.CredentialTypeAPIKey, Credential: "key1", Status: config.AccountStatusActive, ModelLocks: make(map[string]time.Time), CreatedAt: time.Now()},
	}))
	bt := billing.NewBillingTracker(logger, nil)
	g := &NenyaGateway{AccountManager: mgr, BillingTracker: bt, Metrics: infra.NewMetrics()}
	ctx := context.Background()

	key, acct, ok := g.selectPreferredAccountKey(ctx, "prov", "m1", "a1")
	if !ok || acct != "a1" || string(key) != "key1" {
		t.Fatalf("expected healthy preferred account, got ok=%v acct=%q key=%q", ok, acct, string(key))
	}

	bt.MarkExhausted(ctx, "prov", "a1", "quota")
	if _, _, ok := g.selectPreferredAccountKey(ctx, "prov", "m1", "a1"); ok {
		t.Fatal("expected billing-exhausted account to be rejected")
	}

	if _, _, ok := g.selectPreferredAccountKey(ctx, "prov", "m1", ""); ok {
		t.Fatal("expected empty accountID to be rejected")
	}
	if _, _, ok := g.selectPreferredAccountKey(ctx, "prov", "m1", "missing"); ok {
		t.Fatal("expected unknown account to be rejected")
	}

	bare := &NenyaGateway{}
	if _, _, ok := bare.selectPreferredAccountKey(ctx, "prov", "m1", "a1"); ok {
		t.Fatal("expected not-ok without account manager")
	}
}
