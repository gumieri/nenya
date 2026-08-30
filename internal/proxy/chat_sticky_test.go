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
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw, agent))
	if out[0].Provider != "zai" {
		t.Fatalf("expected first target pinned, got %+v", out[0])
	}
	state, ok := gw.SessionRouter.Lookup("unused")
	if ok {
		t.Fatalf("pin should not be creatable without a stable key, got %+v", state)
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
	gw.SessionRouter.Pin(routing.SessionKey("opencode", "", "first turn"), "zai", "zai-free", "acct", 0)

	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", CoolKey: "opencode:zai:zai-free"},
		{Provider: "deepseek", Model: "deepseek-free", CoolKey: "opencode:deepseek:deepseek-free"},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw, agent))
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
	key := routing.SessionKey("opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct", 0)

	targets := []routing.UpstreamTarget{
		{Provider: "deepseek", Model: "deepseek-free", CoolKey: "opencode:deepseek:deepseek-free"},
		{Provider: "zai", Model: "zai-free", CoolKey: "opencode:zai:zai-free", Cooling: true},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw, agent))
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
	key := routing.SessionKey("opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

	// Pinned account was cooling/exhausted at build time; the LRU fallback
	// selected a sibling account for the same provider/model target.
	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", AccountName: "acct-b", CoolKey: "opencode:zai:zai-free"},
	}
	out := applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw, agent))
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
	key := routing.SessionKey("opencode", "", "first turn")
	gw.SessionRouter.Pin(key, "zai", "zai-free", "acct-a", 0)

	targets := []routing.UpstreamTarget{
		{Provider: "zai", Model: "zai-free", AccountName: "acct-a", CoolKey: "opencode:zai:zai-free"},
	}
	before, ok := gw.SessionRouter.Lookup(key)
	if !ok {
		t.Fatal("expected pin before apply")
	}
	applyStickyRouting(req, gw, agent, targets, resolveStickyPin(req, gw, agent))
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
