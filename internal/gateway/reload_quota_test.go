package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/testutil"
)

// newQuotaTestGateway builds a gateway whose provider polls a fake balance
// endpoint (quota_source=api, simple_json extraction, 50ms interval — the
// first poll fires immediately via the poll loop's zero timer).
func newQuotaTestGateway(t *testing.T, ctx context.Context, balance *atomic.Int64, startQuota bool) *NenyaGateway {
	t.Helper()

	quotaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"balance": ` + strconv.FormatInt(balance.Load(), 10) + `}`))
	}))
	t.Cleanup(quotaSrv.Close)

	cfg := testutil.MinimalConfig()
	cfg.Providers = map[string]config.ProviderConfig{
		"prov": {
			URL:       "http://localhost:1/v1/chat/completions", // unused by the poller
			AuthStyle: "none",
			Billing: &config.BillingConfig{
				QuotaSource:   config.QuotaSourceAPI,
				QuotaURL:      quotaSrv.URL,
				QuotaInterval: "50ms",
				QuotaExtraction: &config.QuotaExtractionConfig{
					Mode:        config.ExtractionModeSimpleJSON,
					BalancePath: "balance",
				},
			},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := newGateway(ctx, *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)), startQuota, nil)
	t.Cleanup(gw.Close)
	return gw
}

// waitForExhaustion polls until the account shows exhausted on the tracker.
func waitForExhaustion(t *testing.T, bt *billing.BillingTracker, provider, account string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !bt.IsExhausted(provider, account) {
		if time.Now().After(deadline) {
			t.Fatalf("account %s/%s never showed exhausted on the billing tracker", provider, account)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestReloadQuotaPollerTracksCarriedTracker pins the AUD-08 fix (NENYA-216):
// the reload's quota poller must feed the CARRIED billing tracker. Pre-fix,
// New() started the poller with the fresh tracker and Reload then replaced
// newGW.BillingTracker with the carried one — the poller kept writing the
// orphaned tracker, and once the old gateway drained (stopping the old
// poller) the live tracker had NO poller: polled quota exhaustion went
// dead until the next reload.
func TestReloadQuotaPollerTracksCarriedTracker(t *testing.T) {
	ctx := context.Background()
	var balance atomic.Int64
	balance.Store(100)

	g1 := newQuotaTestGateway(t, ctx, &balance, true)

	// Sanity: with a positive balance the first (immediate) poll marks
	// nothing on the tracker.
	time.Sleep(150 * time.Millisecond)
	if g1.BillingTracker.IsExhausted("prov", "default") {
		t.Fatal("a positive balance must not mark the account exhausted")
	}

	g2 := g1.Reload(ctx, g1.Config, g1.Secrets)
	t.Cleanup(func() { g2.Close() })
	if g2.BillingTracker != g1.BillingTracker {
		t.Fatal("reload must carry the billing tracker")
	}

	// Complete the production sequence: the old gateway drains (its poller
	// stops with it). Only g2's poller is alive from here on.
	g1.CloseWhenDrained(context.Background())

	// Flip the balance to zero: g2's poller must mark the CARRIED tracker.
	balance.Store(0)
	waitForExhaustion(t, g1.BillingTracker, "prov", "default")
}

// TestNewWithoutQuotaStartDoesNotPoll pins the newGateway switch: a gateway
// built with startQuota=false runs no poller (the Reload path relies on
// this — the explicit post-carry Start is the only poller it may have).
func TestNewWithoutQuotaStartDoesNotPoll(t *testing.T) {
	ctx := context.Background()
	var balance atomic.Int64
	balance.Store(0) // would mark exhausted on the first poll, if polling

	gw := newQuotaTestGateway(t, ctx, &balance, false)

	// Give a would-be poller several intervals to fire.
	time.Sleep(200 * time.Millisecond)
	if gw.BillingTracker.IsExhausted("prov", "default") {
		t.Fatal("a gateway built with startQuota=false must not poll quotas")
	}
}
