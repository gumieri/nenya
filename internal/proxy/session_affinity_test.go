package proxy

import (
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

func newAffinityTestGateway(t *testing.T, providers map[string]*config.Provider) *gateway.NenyaGateway {
	t.Helper()
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
		Providers:  providers,
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	return gw
}

func affinityRequest(agent string, firstTurn string) *chatRequest {
	return &chatRequest{ModelName: agent, Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": firstTurn}},
	}}
}

// TestApplyStickyRouting_NonStickyPinsWithoutReorder pins NENYA-29: a
// fallback-strategy agent records the front target's provider/model/account
// as the session pin WITHOUT reordering the operator-defined failover chain.
func TestApplyStickyRouting_NonStickyPinsWithoutReorder(t *testing.T) {
	gw := newAffinityTestGateway(t, nil)
	agent := config.AgentConfig{Strategy: "fallback"}
	req := affinityRequest("opencode", "first turn")
	targets := []routing.UpstreamTarget{
		{Provider: "prov-a", Model: "model-a", AccountName: "acct-1"},
		{Provider: "prov-b", Model: "model-b", AccountName: "acct-2"},
	}

	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), false)

	// Order untouched: prov-a still first (the pin would reorder under the
	// sticky strategy).
	if out[0].Provider != "prov-a" || out[1].Provider != "prov-b" {
		t.Fatalf("non-sticky routing must not reorder, got %+v", out)
	}
	key := routing.SessionKey("opencode", "", "first turn")
	state, ok := gw.SessionRouter.Lookup(key)
	if !ok || state.Provider != "prov-a" || state.Account != "acct-1" {
		t.Fatalf("expected front target pinned, got %+v ok=%v", state, ok)
	}
}

// TestApplyStickyRouting_NonStickyPrefersPinnedAccount pins the credential
// affinity loop: a pinned account steers the matching front target on the
// next request (drift promotion) while the chain order stays intact.
func TestApplyStickyRouting_NonStickyPrefersPinnedAccount(t *testing.T) {
	gw := newAffinityTestGateway(t, nil)
	agent := config.AgentConfig{Strategy: "round-robin"}
	req := affinityRequest("opencode", "first turn")
	key := routing.SessionKey("opencode", "", "first turn")

	// Session already pinned to prov-a/acct-1 (from a previous request).
	gw.SessionRouter.Pin(key, "prov-a", "model-a", "acct-1", 0)

	// Front target served a sibling account (pinned acct was busy during
	// build): the pin must follow the sibling so the next turn's requests
	// target the same upstream credential.
	targets := []routing.UpstreamTarget{
		{Provider: "prov-a", Model: "model-a", AccountName: "acct-sibling"},
	}
	_ = applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), false)

	state, ok := gw.SessionRouter.Lookup(key)
	if !ok || state.Account != "acct-sibling" {
		t.Fatalf("drift promotion must follow the sibling account, got %+v ok=%v", state, ok)
	}
}

// TestApplyStickyRouting_SessionStickyKeysOptOut pins the per-provider
// opt-out: session_sticky_keys=false providers never pin and never promote.
func TestApplyStickyRouting_SessionStickyKeysOptOut(t *testing.T) {
	optedOut := config.Provider{
		Name:              "prov-a",
		SessionStickyKeys: config.PtrTo(false),
	}
	gw := newAffinityTestGateway(t, map[string]*config.Provider{"prov-a": &optedOut})
	agent := config.AgentConfig{Strategy: "fallback"}
	req := affinityRequest("opencode", "first turn")
	targets := []routing.UpstreamTarget{
		{Provider: "prov-a", Model: "model-a", AccountName: "acct-1"},
	}

	_ = applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), false)

	key := routing.SessionKey("opencode", "", "first turn")
	if _, ok := gw.SessionRouter.Lookup(key); ok {
		t.Fatal("opted-out provider must not create a session pin")
	}

	// A pre-existing pin for the opted-out provider must not be promoted.
	gw.SessionRouter.Pin(key, "prov-a", "model-a", "acct-old", 0)
	targets[0].AccountName = "acct-new"
	_ = applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), false)
	if state, _ := gw.SessionRouter.Lookup(key); state.Account != "acct-old" {
		t.Fatalf("opted-out provider must not promote pins, got %+v", state)
	}
}

// TestApplyStickyRouting_SessionStickyKeysExplicitTrue keeps pinning enabled
// when a provider explicitly sets session_sticky_keys=true.
func TestApplyStickyRouting_SessionStickyKeysExplicitTrue(t *testing.T) {
	explicit := config.Provider{
		Name:              "prov-a",
		SessionStickyKeys: config.PtrTo(true),
	}
	gw := newAffinityTestGateway(t, map[string]*config.Provider{"prov-a": &explicit})
	agent := config.AgentConfig{Strategy: "fallback"}
	req := affinityRequest("opencode", "first turn")
	targets := []routing.UpstreamTarget{
		{Provider: "prov-a", Model: "model-a", AccountName: "acct-1"},
	}

	_ = applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), false)

	key := routing.SessionKey("opencode", "", "first turn")
	if state, ok := gw.SessionRouter.Lookup(key); !ok || state.Account != "acct-1" {
		t.Fatalf("explicit opt-in must pin, got %+v ok=%v", state, ok)
	}
}
