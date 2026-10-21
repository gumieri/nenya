package proxy

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
)

func byokGateway(t *testing.T, enabled bool, allow map[string]bool, header string) *gateway.NenyaGateway {
	t.Helper()
	providers := map[string]*config.Provider{}
	for name, allow := range allow {
		providers[name] = &config.Provider{Name: name, AllowBYOK: allow}
	}
	return &gateway.NenyaGateway{
		Logger:    slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Metrics:   infra.NewMetrics(),
		Providers: providers,
		Config: config.Config{
			Governance: config.GovernanceConfig{
				BYOK: &config.BYOKConfig{Enabled: enabled, Header: header},
			},
		},
	}
}

// apply runs the real front door (intent resolution, then application).
func applyBYOK(t *testing.T, gw *gateway.NenyaGateway, r *http.Request, req *chatRequest) *httpError {
	t.Helper()
	(&Proxy{}).resolveBYOKIntent(gw, r, req)
	return (&Proxy{}).applyBYOKCredential(gw, r, req)
}

func byokRequest(t *testing.T, header, value string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	if header != "" && value != "" {
		r.Header.Set(header, value)
	}
	return r
}

func byokChatRequest(providers ...string) *chatRequest {
	targets := make([]routing.UpstreamTarget, 0, len(providers))
	for _, p := range providers {
		targets = append(targets, routing.UpstreamTarget{
			Provider: p, Model: "m", CoolKey: "agent:" + p + ":m", Credential: "shared-" + p,
		})
	}
	return &chatRequest{Targets: targets, KeyRef: "k1", CacheKey: "fingerprint"}
}

func adminKey() *config.ApiKey {
	return &config.ApiKey{Name: "k1", Roles: []string{"admin"}, Enabled: true}
}

func TestApplyBYOKCredentialDisabledIsNoOp(t *testing.T) {
	gw := byokGateway(t, false, map[string]bool{"anthropic": true}, "")
	req := byokChatRequest("anthropic")
	if herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req); herr != nil {
		t.Fatalf("disabled BYOK must be a no-op: %v", herr)
	}
	if req.BYOK || req.Targets[0].Credential != "shared-anthropic" {
		t.Fatal("disabled BYOK must not touch the targets")
	}
}

func TestApplyBYOKCredentialAbsentHeaderIsNoOp(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")
	req := byokChatRequest("anthropic")
	if herr := applyBYOK(t, gw, byokRequest(t, "", ""), req); herr != nil {
		t.Fatalf("no credential presented must be a no-op: %v", herr)
	}
	if req.BYOK {
		t.Fatal("an absent header must not mark the request BYOK")
	}
}

func TestApplyBYOKCredentialPermissionAndValidation(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")

	t.Run("key without the byok permission is denied", func(t *testing.T) {
		req := byokChatRequest("anthropic")
		req.apiKey = &config.ApiKey{Name: "user-key", Roles: []string{"user"}, Enabled: true}
		herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req)
		if herr == nil || herr.Code != http.StatusForbidden {
			t.Fatalf("want 403 for a key without the byok permission, got %+v", herr)
		}
		if req.BYOK || req.Targets[0].Credential != "shared-anthropic" {
			t.Fatal("a denied BYOK must not alter the targets (fail closed)")
		}
	})

	t.Run("user key with permissions.byok may", func(t *testing.T) {
		req := byokChatRequest("anthropic")
		req.apiKey = &config.ApiKey{
			Name: "user-key", Roles: []string{"user"}, Enabled: true,
			Permissions: map[string]any{"byok": true},
		}
		if herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req); herr != nil {
			t.Fatalf("opted-in user key must be allowed: %v", herr)
		}
	})

	t.Run("malformed credential is rejected, never silently replaced", func(t *testing.T) {
		for _, bad := range []string{"has space", "tab\there", "café", strings.Repeat("a", maxBYOKCredentialLen+1)} {
			req := byokChatRequest("anthropic")
			req.apiKey = adminKey()
			herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, bad), req)
			if herr == nil || herr.Code != http.StatusBadRequest {
				t.Fatalf("want 400 for %q, got %+v", bad, herr)
			}
			if req.Targets[0].Credential != "shared-anthropic" {
				t.Fatalf("a rejected credential must not fall back silently for %q", bad)
			}
		}
	})
}

func TestApplyBYOKCredentialAppliesAndAudits(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")
	req := byokChatRequest("anthropic", "openai")
	req.apiKey = adminKey()

	if herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req); herr != nil {
		t.Fatalf("BYOK: %v", herr)
	}
	if !req.BYOK {
		t.Fatal("request must be marked BYOK")
	}
	if len(req.Targets) != 1 || req.Targets[0].Provider != "anthropic" {
		t.Fatalf("only opt-in providers may serve a BYOK request: %+v", req.Targets)
	}
	if req.Targets[0].Credential != "caller-key" || !req.Targets[0].BYOK {
		t.Fatalf("the caller credential must replace the shared one and mark the target: %+v", req.Targets[0])
	}
	if req.CacheKey != "" {
		t.Fatal("a BYOK request must be excluded from the response cache")
	}
	var sb strings.Builder
	gw.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_byok_requests_total{provider="anthropic"} 1`) {
		t.Fatalf("BYOK must be audited by provider, never by value:\n%s", sb.String())
	}
	if strings.Contains(sb.String(), "caller-key") {
		t.Fatal("the credential value must never reach a metric")
	}
}

func TestApplyBYOKCredentialNoEligibleProvider(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": false}, "")
	req := byokChatRequest("anthropic")
	req.apiKey = adminKey()
	herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req)
	if herr == nil || herr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a BYOK request with no opt-in target must fail closed with 422, got %+v", herr)
	}
	if req.Targets[0].Credential != "shared-anthropic" {
		t.Fatal("no silent fallback to the shared key")
	}
}

// A custom header name is honored, and the default is not consulted then.
func TestApplyBYOKCredentialCustomHeader(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "X-Corp-Upstream-Key")
	req := byokChatRequest("anthropic")
	req.apiKey = adminKey()
	if herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req); herr != nil {
		t.Fatalf("the default header must be ignored when a custom one is configured: %v", herr)
	}
	if req.BYOK || req.BYOKRequested {
		t.Fatal("the default header must not be read when a custom header is configured")
	}
	if herr := applyBYOK(t, gw, byokRequest(t, "X-Corp-Upstream-Key", "caller-key"), req); herr != nil {
		t.Fatalf("custom header: %v", herr)
	}
	if !req.BYOK {
		t.Fatal("the configured header must be honored")
	}
}

// BYOK targets never write shared-account breaker state (NENYA-195).
func TestBYOKTargetsDoNotWriteSharedAccountState(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	state := routing.NewAgentState(logger, infra.NewMetrics())
	shared := routing.UpstreamTarget{Provider: "anthropic", Model: "m", CoolKey: "agent:anthropic:m", AccountName: "acct-1"}
	byok := shared
	byok.BYOK = true

	state.RecordFailure(byok, 60)
	state.ActivateCooldown(byok, 60)
	if d := state.RecordFailureWithStatus(byok, 500, "boom"); d.ShouldLock {
		t.Fatal("a BYOK failure must not cool down the shared account")
	}
	if state.CB.State(shared.CoolKey) != 0 {
		t.Fatalf("a BYOK failure must not move the shared account's breaker (state %v)", state.CB.State(shared.CoolKey))
	}

	// The guard is narrow: a non-BYOK target on the same key still charges it.
	state.ActivateCooldown(shared, 60)
	if state.CB.State(shared.CoolKey) == 0 {
		t.Fatal("a shared-account failure must still charge the breaker")
	}
}

// NENYA-195 review: a caller-funded dispatch is never charged to the shared
// billing account (that would poison its exhaustion/quota state and surface
// caller traffic as the shared account's usage).
func TestBYOKDispatchDoesNotChargeSharedBillingAccount(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")
	req := byokChatRequest("anthropic")
	req.apiKey = adminKey()
	if herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req); herr != nil {
		t.Fatalf("BYOK: %v", herr)
	}
	target := req.Targets[0]
	if !target.BYOK {
		t.Fatal("fixture must be marked BYOK")
	}

	tracker := billing.NewBillingTracker(nil, nil)
	gw.BillingTracker = tracker
	before := tracker.GetTotalSpend(target.Provider, target.AccountName)

	recordCostAndBilling(context.Background(), gw, target, "", 1000, 0, 0, 500)
	if after := tracker.GetTotalSpend(target.Provider, target.AccountName); after != before {
		t.Fatalf("a BYOK dispatch must not charge the shared account (%v -> %v)", before, after)
	}

	if accounts := tracker.GetAllAccounts(); len(accounts) != 0 {
		t.Fatalf("a BYOK dispatch must not create a shared-account billing entry: %+v", accounts)
	}

	// The guard is narrow: the same dispatch on a non-BYOK target does charge.
	shared := target
	shared.BYOK = false
	recordCostAndBilling(context.Background(), gw, shared, "", 1000, 0, 0, 500)
	if accounts := tracker.GetAllAccounts(); len(accounts) == 0 {
		t.Fatal("a shared-account dispatch must still be recorded")
	}
}

// NENYA-195 review: a BYOK request must not write session affinity state, and
// the intent must be known BEFORE routing (the pin is written inside it).
func TestBYOKRequestSkipsSessionAffinity(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")
	gw.AgentState = routing.NewAgentState(slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), infra.NewMetrics())
	req := &chatRequest{}
	(&Proxy{}).resolveBYOKIntent(gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req)
	if !req.BYOKRequested {
		t.Fatal("the intent must be resolved from the presented header")
	}
	targets := []routing.UpstreamTarget{{Provider: "anthropic", Model: "m", AccountName: "acct-1"}}
	out := applyStickyRouting(req, gw, config.AgentConfig{}, targets, &stickyPin{key: "session-1"}, true)
	if len(out) != 1 || out[0].AccountName != "acct-1" {
		t.Fatalf("a BYOK request must leave the target order untouched: %+v", out)
	}
	if _, found := gw.AgentState.SessionRouter.Peek("session-1"); found {
		t.Fatal("a BYOK request must not write a session pin")
	}
}

// The caller credential actually reaches the outbound attempt's auth header.
func TestBYOKCredentialReachesUpstreamAuthHeader(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"openai": true}, "")
	gw.Providers["openai"] = &config.Provider{Name: "openai", AuthStyle: "bearer", AllowBYOK: true}
	req := byokChatRequest("openai")
	req.apiKey = adminKey()
	if herr := applyBYOK(t, gw, byokRequest(t, config.DefaultBYOKHeader, "caller-key"), req); herr != nil {
		t.Fatalf("BYOK: %v", herr)
	}
	target := req.Targets[0]
	out, err := (&Proxy{}).buildUpstreamRequest(gw, context.Background(), http.MethodPost,
		target.URL, []byte(`{}`), target.Provider, target.Model, target.Credential, http.Header{})
	if err != nil {
		t.Fatalf("build upstream request: %v", err)
	}
	if got := out.Header.Get("Authorization"); got != "Bearer caller-key" {
		t.Fatalf("the caller credential must reach the upstream auth header, got %q", got)
	}
}

// NENYA-195 review: the streaming billing stand-down.
func TestBYOKStreamingSpendDoesNotChargeSharedAccount(t *testing.T) {
	gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")
	tracker := billing.NewBillingTracker(nil, nil)
	gw.BillingTracker = tracker
	target := routing.UpstreamTarget{Provider: "anthropic", AccountName: "acct-shared", Model: "m", BYOK: true}

	claimed := false
	recordStreamSpend(context.Background(), gw, target, 0.5, 100, 50, &claimed, true)
	if accounts := tracker.GetAllAccounts(); len(accounts) != 0 {
		t.Fatalf("a BYOK stream must not claim a shared billing request: %+v", accounts)
	}

	shared := target
	shared.BYOK = false
	claimed = false
	recordStreamSpend(context.Background(), gw, shared, 0.5, 100, 50, &claimed, true)
	if accounts := tracker.GetAllAccounts(); len(accounts) == 0 {
		t.Fatal("a shared stream must still claim the billing request")
	}
}

// NENYA-195 review: an exhausted SHARED account must not drop a target whose
// provider opted into BYOK, and must still drop it otherwise.
func TestFilterExhaustedTargetsExemptsBYOKEligibleProviders(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	tracker := billing.NewBillingTracker(nil, nil)
	providers := map[string]*config.Provider{"anthropic": {Name: "anthropic", AllowBYOK: true}}

	exhaust := func() {
		tracker.MarkExhausted(context.Background(), "anthropic", "acct-shared", "quota")
	}
	targets := []routing.UpstreamTarget{{Provider: "anthropic", Model: "m", AccountName: "acct-shared", CoolKey: "agent:anthropic:m"}}

	exhaust()
	if got := filterExhaustedTargets(targets, tracker, providers, logger, false); len(got) != 0 {
		t.Fatalf("a non-BYOK request must skip an exhausted account: %+v", got)
	}
	exhaust()
	if got := filterExhaustedTargets(targets, tracker, providers, logger, true); len(got) != 1 {
		t.Fatalf("a BYOK request must keep an opted-in provider despite shared exhaustion: %+v", got)
	}

	// A provider that did not opt in is still filtered under BYOK.
	other := map[string]*config.Provider{"anthropic": {Name: "anthropic"}}
	exhaust()
	if got := filterExhaustedTargets(targets, tracker, other, logger, true); len(got) != 0 {
		t.Fatalf("a non-opt-in provider must still be filtered: %+v", got)
	}
}

// NENYA-208: the two shared-money pre-dispatch guards (the max-cost estimate
// and the per-agent budget over shared-account spend) stand down for a BYOK
// target — a caller-funded dispatch draws no shared money — while the same
// guards still skip a shared-account target armed identically.
func TestCheckPreDispatchGuardsStandDownOnBYOK(t *testing.T) {
	priced := discovery.NewModelCatalog()
	priced.Add(discovery.DiscoveredModel{
		ID:      "priced-model",
		Pricing: &discovery.PricingEntry{InputCostPer1M: 1000, OutputCostPer1M: 5000},
	})

	newGW := func(t *testing.T) *gateway.NenyaGateway {
		t.Helper()
		gw := byokGateway(t, true, map[string]bool{"anthropic": true}, "")
		gw.RateLimiter = infra.NewRateLimiter(0, 0)
		gw.ModelCatalog = priced
		gw.BillingTracker = billing.NewBillingTracker(nil, nil)
		gw.Config.Governance.MaxCostPerRequest = 0.000001 // any priced dispatch exceeds it
		gw.Config.Agents = map[string]config.AgentConfig{
			"agent-a": {BudgetLimitUSD: 1.0},
		}
		// Shared-account spend well over the agent budget.
		gw.BillingTracker.RecordSpend(context.Background(), billing.SpendEntry{
			ProviderName: "anthropic", AccountName: "acct-shared", CostUSD: 5.0,
		})
		return gw
	}
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	cases := []struct {
		name     string
		byok     bool
		wantSkip bool
	}{
		{"shared target is skipped by the armed guards", false, true},
		{"BYOK target dispatches (guards stand down)", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newGW(t)
			target := routing.UpstreamTarget{Provider: "anthropic", Model: "priced-model", AccountName: "acct-shared", BYOK: tc.byok}
			action := (&Proxy{}).checkPreDispatchGuards(gw, logger, target, 10000, "agent-a", nil)
			if tc.wantSkip {
				if action == nil || action.kind != actionContinue {
					t.Fatalf("the armed guards must skip the shared target: %+v", action)
				}
				return
			}
			if action != nil {
				t.Fatalf("the guards must stand down for a BYOK target: %+v", action)
			}
		})
	}
}
