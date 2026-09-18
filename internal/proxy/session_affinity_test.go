package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
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

// TestSessionIdentityForkE2E pins NENYA-39 end to end: conversations sharing
// an opening turn but diverging afterwards get distinct synthesized session
// identities (so they stop sharing a pin), while a genuine continuation
// keeps the identity stable across turns.
func TestSessionIdentityForkE2E(t *testing.T) {
	var lastHeader atomicstring
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		lastHeader.Store(r.Header.Get("X-Opencode-Session"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: up.URL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider-0", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	do := func(msgs ...string) string {
		parts := make([]string, len(msgs))
		roles := []string{"user", "assistant", "user", "assistant"}
		for i, m := range msgs {
			parts[i] = fmt.Sprintf(`{"role":%q,"content":%q}`, roles[i%2], m)
		}
		body := `{"model":"test-agent","stream":false,"messages":[` + strings.Join(parts, ",") + `]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %v: got %d: %s", msgs, rec.Code, rec.Body.String())
		}
		header := lastHeader.Load()
		lastHeader.Store("")
		return header
	}

	// Turn 1 and its continuation share an identity.
	h1 := do("hi")
	h1b := do("hi", "answer", "keep going")
	if h1 == "" || h1 != h1b {
		t.Fatalf("continuation must keep the session identity: %q vs %q", h1, h1b)
	}

	// A different conversation with the same opener forks a new identity.
	h2 := do("hi", "totally different")
	if h2 == "" || h2 == h1 {
		t.Fatalf("opener collision must fork the session identity: %q vs %q", h2, h1)
	}

	// The fork stays stable on its next turn.
	h2b := do("hi", "totally different", "more")
	if h2b != h2 {
		t.Fatalf("fork identity must be stable: %q vs %q", h2b, h2)
	}
}
