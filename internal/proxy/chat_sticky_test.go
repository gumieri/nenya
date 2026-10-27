package proxy

import (
	"log/slog"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func testLog(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{}))
}

func TestFirstUserMessage_Basic(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "sys"},
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	text, ok := firstUserMessage(payload)
	if !ok || text != "hello" {
		t.Fatalf("got %q, %v", text, ok)
	}
}

func TestFirstUserMessage_ToolResultFirst(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "cluster info"},
			}},
		},
	}
	text, ok := firstUserMessage(payload)
	if !ok || text != "cluster info" {
		t.Fatalf("got %q, %v", text, ok)
	}
}

func TestFirstUserMessage_MissingUserFallsBackToFirst(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "content": "earlier"},
		},
	}
	text, ok := firstUserMessage(payload)
	if !ok || text != "earlier" {
		t.Fatalf("got %q, %v", text, ok)
	}
}

func TestFirstUserMessage_Empty(t *testing.T) {
	if _, ok := firstUserMessage(map[string]any{}); ok {
		t.Fatal("expected ok=false for empty payload")
	}
}

func TestFirstUserMessage_ToolCallObject(t *testing.T) {
	payload := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "content": "42"},
			}},
		},
	}
	text, ok := firstUserMessage(payload)
	if ok || text != "" {
		t.Fatalf("expected empty fallback for content without text, got %q, %v", text, ok)
	}
}

func TestFirstActiveTarget_SkipsCooling(t *testing.T) {
	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "m1", Cooling: true},
		{Provider: "deepseek", Model: "m2"},
	}
	got, ok := firstActiveTarget(targets)
	if !ok || got.Provider != "deepseek" {
		t.Fatalf("got %+v, %v", got, ok)
	}
}

func TestFirstActiveTarget_AllCooling(t *testing.T) {
	targets := []routing.UpstreamTarget{{Provider: "zai", Model: "m1", Cooling: true}}
	got, ok := firstActiveTarget(targets)
	if !ok {
		t.Fatal("expected fallback to first target")
	}
	if got.Provider != "zai" {
		t.Fatalf("got %+v", got)
	}
}

func TestIndexOfActiveTarget_SkipsCooling(t *testing.T) {
	targets := []routing.UpstreamTarget{
		{Provider: "deepseek", Model: "m2"},
		{Provider: "zai", Model: "m1", Cooling: true},
		{Provider: "zai", Model: "m1"},
	}
	if i := indexOfActiveTarget(targets, "zai", "m1"); i != 2 {
		t.Fatalf("expected index 2 (cooling skipped), got %d", i)
	}
	if i := indexOfActiveTarget(targets, "missing", "m1"); i != -1 {
		t.Fatalf("expected -1, got %d", i)
	}
}

func TestMoveToFront(t *testing.T) {
	targets := []routing.UpstreamTarget{
		{Provider: "a", Model: "m1"},
		{Provider: "b", Model: "m2"},
		{Provider: "c", Model: "m3"},
	}
	got := moveToFront(targets, 2)
	if got[0].Provider != "c" || got[1].Provider != "a" || got[2].Provider != "b" {
		t.Fatalf("got %+v", got)
	}
}

func TestApplyStickyRouting_NewPin(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	agent := config.AgentConfig{Strategy: "sticky"}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", MaxContext: 256000},
		{Provider: "deepseek", Model: "deepseek-free", MaxContext: 64000},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
	if out[0].Provider != "zai" {
		t.Fatalf("expected first target pinned, got %+v", out[0])
	}
	key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
	state, ok := gw.SessionRouter.Lookup(key)
	if !ok || state.Provider != "zai" || state.Model != "zai-free" {
		t.Fatalf("expected new pin under the real session key, got %+v, %v", state, ok)
	}
	if gw.SessionRouter.Len() != 1 {
		t.Fatalf("expected exactly 1 pin, got %d", gw.SessionRouter.Len())
	}
}

func TestApplyStickyRouting_EmptyAccountDoesNotWipePin(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	agent := config.AgentConfig{Strategy: "sticky"}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

	// Pinned account unavailable AND LRU fallback failed entirely: the front
	// target carries no account. The pin's account must be preserved so the
	// session can return to it once accounts recover.
	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", CoolKey: "opencode:zai:zai-free"},
	}
	before, ok := gw.SessionRouter.Lookup(key)
	if !ok {
		t.Fatal("expected pin before apply")
	}
	applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
	after, ok := gw.SessionRouter.Lookup(key)
	if !ok || after.Account != "acct-a" || !after.Since.Equal(before.Since) {
		t.Fatalf("expected pin account preserved untouched, got %+v (before Since %v)", after, before.Since)
	}
}

func TestApplyStickyRouting_ReusePin(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	agent := config.AgentConfig{Strategy: "sticky"}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	gw.SessionRouter.Pin("k", "deepseek", "deepseek-free", "acct", 0)
	gw.SessionRouter.Pin(routing.SessionKeyForSubject("", "opencode", "", "first turn"), "zai", "zai-free", "acct", 0)

	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", CoolKey: "opencode:zai:zai-free"},
		{Provider: "deepseek", Model: "deepseek-free", CoolKey: "opencode:deepseek:deepseek-free"},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
	if out[0].Provider != "zai" || out[0].Model != "zai-free" {
		t.Fatalf("expected pinned target promoted to front, got %+v", out[0])
	}
}

func TestApplyStickyRouting_CoolingPinPromoted(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	agent := config.AgentConfig{Strategy: "sticky"}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct", 0)

	targets := []routing.UpstreamTarget{
		{Provider: "deepseek", Model: "deepseek-free", CoolKey: "opencode:deepseek:deepseek-free"},
		{Provider: "zai", Model: "zai-free", CoolKey: "opencode:zai:zai-free", Cooling: true},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
	if out[0].Provider != "deepseek" {
		t.Fatalf("expected first active target at front, got %+v", out[0])
	}
	state, ok := gw.SessionRouter.Lookup(key)
	if !ok || state.Provider != "deepseek" {
		t.Fatalf("pin not promoted, got %+v, %v", state, ok)
	}
}

func TestApplyStickyRouting_AccountDriftPromotesToSibling(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	agent := config.AgentConfig{Strategy: "sticky"}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

	// Pinned account was cooling/exhausted at build time; the LRU fallback
	// selected a sibling account for the same provider/model target.
	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", AccountName: "acct-b", CoolKey: "opencode:zai:zai-free"},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
	if out[0].AccountName != "acct-b" {
		t.Fatalf("expected sibling-account target at front, got %+v", out[0])
	}
	state, ok := gw.SessionRouter.Lookup(key)
	if !ok || state.Account != "acct-b" {
		t.Fatalf("pin not re-pointed to sibling account, got %+v, %v", state, ok)
	}
	if state.Provider != "zai" || state.Model != "zai-free" {
		t.Fatalf("sibling promotion must stay on the pinned provider/model, got %+v", state)
	}
}

func TestApplyStickyRouting_ValidFrontPinNotRePinned(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger:     testLog(t),
		AgentState: routing.NewAgentState(testLog(t), nil),
	}
	gw.SessionRouter = gw.AgentState.SessionRouter
	agent := config.AgentConfig{Strategy: "sticky"}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", AccountName: "acct-a", CoolKey: "opencode:zai:zai-free"},
	}
	before, ok := gw.SessionRouter.Lookup(key)
	if !ok {
		t.Fatal("expected pin before apply")
	}
	applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
	after, ok := gw.SessionRouter.Lookup(key)
	if !ok {
		t.Fatal("expected pin after apply")
	}
	// No drift: the pin must be untouched — Since is only reset by Promote,
	// so equality proves no spurious failover was recorded.
	if after.Account != "acct-a" || !after.Since.Equal(before.Since) {
		t.Fatalf("expected unchanged pin, got %+v (before Since %v)", after, before.Since)
	}
}

// NENYA-202 follow-up: for an agent that opted into cache economics
// (cache_aware force, or auto with a cache-capable pinned provider), a pin
// whose target is TRANSIENTLY benched (breaker-open, listed Cooling; or
// model-locked) is held rather than migrated: the fallback serves the turn,
// and the session snaps back to the warm target when the bench lifts.
// Migrating (the old unconditional promotion) converts the bench into a full
// prefix re-ingest AND abandons the warm target after recovery.
func TestApplyStickyRouting_PinHoldThroughCooldown(t *testing.T) {
	newGW := func(t *testing.T) *gateway.NenyaGateway {
		gw := &gateway.NenyaGateway{
			Logger:     testLog(t),
			AgentState: routing.NewAgentState(testLog(t), nil),
		}
		gw.SessionRouter = gw.AgentState.SessionRouter
		return gw
	}
	req := &chatRequest{ModelName: "opencode", Payload: map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "first turn"}},
	}}
	// The pinned target is present but flagged Cooling (breaker open at
	// build); the fallback deepseek target is active and serves the turn.
	coolingTargets := func() []routing.UpstreamTarget {
		return []routing.UpstreamTarget{
			{Provider: "deepseek", Model: "deepseek-free"},
			{Provider: "zai", Model: "zai-free", Cooling: true, CoolKey: "opencode:zai:zai-free"},
		}
	}

	t.Run("force holds the pin through a cooling target", func(t *testing.T) {
		gw := newGW(t)
		agent := config.AgentConfig{Strategy: "sticky", CacheAware: config.CacheAwareForce}
		key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
		gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

		out := applyStickyRouting(req, gw, agent, coolingTargets(), resolveStickyPin(req, gw), true)
		if out[0].Provider != "deepseek" {
			t.Fatalf("the fallback must serve the turn, got %+v", out[0])
		}
		state, ok := gw.SessionRouter.Lookup(key)
		if !ok || state.Provider != "zai" || state.Model != "zai-free" {
			t.Fatalf("the pin must be held on the benched warm target, got %+v, %v", state, ok)
		}
	})

	t.Run("auto holds only for a cache-capable pinned provider", func(t *testing.T) {
		gw := newGW(t)
		agent := config.AgentConfig{Strategy: "sticky", CacheAware: config.CacheAwareAuto}
		key := routing.SessionKeyForSubject("", "opencode", "", "first turn")

		// deepseek is a built-in automatic-prefix-cache provider: hold.
		gw.SessionRouter.Pin(key, "deepseek", "deepseek-free", "acct-a", 0)
		targets := []routing.UpstreamTarget{
			{Provider: "zai", Model: "zai-free"},
			{Provider: "deepseek", Model: "deepseek-free", Cooling: true, CoolKey: "opencode:deepseek:deepseek-free"},
		}
		applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
		if state, _ := gw.SessionRouter.Lookup(key); state.Provider != "deepseek" {
			t.Fatalf("auto must hold the pin on a cache-capable provider, got %+v", state)
		}

		// zai is not: the pin migrates (pre-existing behavior).
		gw2 := newGW(t)
		gw2.SessionRouter = gw2.AgentState.SessionRouter
		gw2.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)
		targets2 := []routing.UpstreamTarget{
			{Provider: "deepseek", Model: "deepseek-free"},
			{Provider: "zai", Model: "zai-free", Cooling: true, CoolKey: "opencode:zai:zai-free"},
		}
		applyStickyRouting(req, gw2, agent, targets2, resolveStickyPin(req, gw2), true)
		if state, _ := gw2.SessionRouter.Lookup(key); state.Provider == "zai" {
			t.Fatalf("auto must NOT hold the pin on a non-cache-capable provider")
		}
	})

	t.Run("cache_aware off promotes as before", func(t *testing.T) {
		gw := newGW(t)
		agent := config.AgentConfig{Strategy: "sticky"}
		key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
		gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

		out := applyStickyRouting(req, gw, agent, coolingTargets(), resolveStickyPin(req, gw), true)
		if out[0].Provider != "deepseek" {
			t.Fatalf("expected the fallback promoted to front, got %+v", out[0])
		}
		if state, _ := gw.SessionRouter.Lookup(key); state.Provider != "deepseek" {
			t.Fatalf("off must keep the pre-existing migration behavior, got %+v", state)
		}
	})

	t.Run("a target absent from the chain still promotes", func(t *testing.T) {
		gw := newGW(t)
		agent := config.AgentConfig{Strategy: "sticky", CacheAware: config.CacheAwareForce}
		key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
		gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

		// The pinned provider appears nowhere: removed (or fully filtered),
		// not benched — the pin must migrate.
		targets := []routing.UpstreamTarget{
			{Provider: "deepseek", Model: "deepseek-free"},
		}
		applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
		if state, _ := gw.SessionRouter.Lookup(key); state.Provider != "deepseek" {
			t.Fatalf("a removed target must not hold the pin, got %+v", state)
		}
	})

	t.Run("a model-locked pinned target holds the pin", func(t *testing.T) {
		gw := newGW(t)
		agent := config.AgentConfig{Strategy: "sticky", CacheAware: config.CacheAwareForce}
		key := routing.SessionKeyForSubject("", "opencode", "", "first turn")
		gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

		coolKey := "opencode:zai:zai-free"
		// A model-scoped lock (NENYA-170): the build drops the target
		// entirely, so the hold must consult the breaker.
		gw.AgentState.CB.RecordFailureWithStatus(coolKey, 429, "Model `zai-free` is rate limited (model-scoped quota)")
		if !gw.AgentState.CB.IsModelLocked(coolKey) {
			t.Skip("classifier did not produce a model lock for this body; lock-path covered by the Cooling case")
		}
		targets := []routing.UpstreamTarget{
			{Provider: "deepseek", Model: "deepseek-free"},
		}
		applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw), true)
		if state, _ := gw.SessionRouter.Lookup(key); state.Provider != "zai" {
			t.Fatalf("a model-locked target must hold the pin, got %+v", state)
		}
	})
}
