package proxy

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nenya/internal/routing"
)

// TestHandleRetryableError429_BYOKLeavesSharedStateUntouched pins AUD-22
// (NENYA-225): a caller-funded (BYOK) request's 429 is the CALLER's account
// hitting its limit. Pre-fix the handler unconditionally evicted the
// provider's pooled connections and recorded a shared cooldown + breaker
// failure — dropping warm connections other tenants share and benching a
// target whose shared credential is perfectly healthy, exactly the
// cross-tenant contamination BYOK exists to prevent (NENYA-195).
func TestHandleRetryableError429_BYOKLeavesSharedStateUntouched(t *testing.T) {
	p, _ := newTestProxy(t)
	gw := p.Gateway()
	gw.Config.Governance.MinQuotaCooldownSeconds = 10

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	byokTarget := routing.UpstreamTarget{
		CoolKey:  "byok:prov:model",
		Provider: "prov",
		Model:    "model",
		BYOK:     true,
	}

	action := upstreamAction{
		resp: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
		},
	}
	body := []byte(`{"error":{"message":"rate limit exceeded for your key"}}`)

	beforeCircuits := len(gw.AgentState.CBSnapshot())
	beforeCooldown := gw.AgentState.ActiveCooldowns()

	delay, isQuota := handleRetryableError429(logger, body, action, 0, byokTarget, "agent", gw)
	_ = delay
	_ = isQuota

	if after := len(gw.AgentState.CBSnapshot()); after != beforeCircuits {
		t.Fatalf("a BYOK 429 must not open/expire a shared circuit (before %d, after %d)", beforeCircuits, after)
	}
	if after := gw.AgentState.ActiveCooldowns(); after != beforeCooldown {
		t.Fatalf("a BYOK 429 must not record a shared cooldown (before %d, after %d)", beforeCooldown, after)
	}
	if !strings.Contains(logBuf.String(), "BYOK request rate-limited") {
		t.Fatalf("expected the stand-down to be logged, got: %s", logBuf.String())
	}
}

// TestHandleRetryableError429_SharedStillCoolsDown is the contrast pin: a
// non-BYOK 429 must still charge the shared account, so the stand-down cannot
// be implemented by disabling the whole path.
func TestHandleRetryableError429_SharedStillCoolsDown(t *testing.T) {
	p, _ := newTestProxy(t)
	gw := p.Gateway()
	gw.Config.Governance.MinQuotaCooldownSeconds = 10

	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	sharedTarget := routing.UpstreamTarget{
		CoolKey:  "shared:prov:model",
		Provider: "prov",
		Model:    "model",
	}
	action := upstreamAction{
		resp: &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{},
		},
	}
	body := []byte(`{"error":{"message":"insufficient_quota"}}`)

	// A non-zero base cooldown: with 0 the breaker opens with a zero expiry and
	// the active-cooldown count stays 0, which would make this contrast vacuous.
	handleRetryableError429(logger, body, action, 30*time.Second, sharedTarget, "agent", gw)

	if gw.AgentState.ActiveCooldowns() == 0 {
		t.Fatal("a shared-account 429 must still record a cooldown")
	}
}
