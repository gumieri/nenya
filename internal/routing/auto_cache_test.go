package routing

import (
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/pipeline"
)

// NENYA-185: opt-in automatic cache-breakpoint anchoring. The gateway injects
// its own system/message breakpoints ONLY when the client expressed no
// placement of its own, the request has stable history, and the history was
// not rewritten; every outcome is recorded, including the stand-downs.

func autoCacheDeps(t *testing.T, optedIn bool) (TransformDeps, *config.Config) {
	t.Helper()
	deps, _ := newNormalizeDeps(t)
	enabled := optedIn
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"agent-a": {AutoCacheBreakpoints: &enabled},
		},
		PrefixCache: config.PrefixCacheConfig{Enabled: true},
	}
	deps.Config = cfg
	deps.AgentName = "agent-a"
	return deps, cfg
}

func multiTurnPayload() map[string]interface{} {
	return map[string]interface{}{
		"model": "claude-test",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "you are a bot"},
			map[string]interface{}{"role": "user", "content": "first"},
			map[string]interface{}{"role": "assistant", "content": "answer"},
			map[string]interface{}{"role": "user", "content": "second"},
		},
	}
}

func TestAutoCacheBreakpointsInjectedWhenClientSilent(t *testing.T) {
	deps, cfg := autoCacheDeps(t, true)
	opts := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, false)

	if !opts.System || !opts.Messages {
		t.Fatalf("opted-in agent with a silent client and stable history must anchor system+messages: %+v", opts)
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_auto_cache_breakpoints_total{outcome="injected"} 1`) {
		t.Fatalf("injection must be recorded:\n%s", sb.String())
	}
}

func TestAutoCacheBreakpointsStandDownOnClientMarks(t *testing.T) {
	cases := map[string]map[string]interface{}{
		"top-level message block": func() map[string]interface{} {
			p := multiTurnPayload()
			msgs := p["messages"].([]interface{})
			msgs[1].(map[string]interface{})["content"] = []interface{}{
				map[string]interface{}{"type": "text", "text": "first", "cache_control": map[string]interface{}{"type": "ephemeral"}},
			}
			return p
		}(),
		"hidden in extra_body": func() map[string]interface{} {
			p := multiTurnPayload()
			p["extra_body"] = map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"content": []interface{}{map[string]interface{}{"cache_control": map[string]interface{}{"type": "ephemeral"}}}},
				},
			}
			return p
		}(),
		"hidden in a tool schema": func() map[string]interface{} {
			p := multiTurnPayload()
			p["tools"] = []interface{}{
				map[string]interface{}{"type": "function", "function": map[string]interface{}{
					"name":       "t",
					"parameters": map[string]interface{}{"cache_control": map[string]interface{}{"type": "ephemeral"}},
				}},
			}
			return p
		}(),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			deps, cfg := autoCacheDeps(t, true)
			opts := adapter.CacheOpts{}
			applyAutoCacheBreakpoints(deps, payload, cfg.PrefixCache, &opts, false)
			if opts.System || opts.Messages {
				t.Fatalf("a client mark must stand the gateway down: %+v", opts)
			}
			var sb strings.Builder
			deps.Metrics.WritePrometheus(&sb)
			if !strings.Contains(sb.String(), `outcome="skipped_client_marks"`) {
				t.Fatalf("stand-down must be recorded:\n%s", sb.String())
			}
		})
	}
}

func TestAutoCacheBreakpointsSkipReasons(t *testing.T) {
	t.Run("one-shot has nothing stable to anchor", func(t *testing.T) {
		deps, cfg := autoCacheDeps(t, true)
		payload := map[string]interface{}{
			"model":    "claude-test",
			"messages": []interface{}{map[string]interface{}{"role": "user", "content": "only turn"}},
		}
		opts := adapter.CacheOpts{}
		applyAutoCacheBreakpoints(deps, payload, cfg.PrefixCache, &opts, false)
		if opts.System || opts.Messages {
			t.Fatalf("one-shot request must not be anchored: %+v", opts)
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		if !strings.Contains(sb.String(), `outcome="skipped_one_shot"`) {
			t.Fatalf("one-shot skip must be recorded:\n%s", sb.String())
		}
	})

	t.Run("windowed history is not ours to anchor", func(t *testing.T) {
		deps, cfg := autoCacheDeps(t, true)
		deps.WindowCompacted = true
		opts := adapter.CacheOpts{}
		applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, false)
		if opts.System || opts.Messages {
			t.Fatalf("a compacted history must not be anchored: %+v", opts)
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		if !strings.Contains(sb.String(), `outcome="skipped_windowed"`) {
			t.Fatalf("windowed skip must be recorded:\n%s", sb.String())
		}
	})

	t.Run("prefix cache disabled", func(t *testing.T) {
		deps, cfg := autoCacheDeps(t, true)
		cfg.PrefixCache.Enabled = false
		opts := adapter.CacheOpts{}
		applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, false)
		if opts.System || opts.Messages {
			t.Fatalf("anchoring requires the prefix cache to be enabled: %+v", opts)
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		if !strings.Contains(sb.String(), `outcome="skipped_disabled"`) {
			t.Fatalf("disabled skip must be recorded:\n%s", sb.String())
		}
	})

	t.Run("not opted in stays silent", func(t *testing.T) {
		deps, cfg := autoCacheDeps(t, false)
		opts := adapter.CacheOpts{}
		applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, false)
		if opts.System || opts.Messages {
			t.Fatalf("the knob defaults off: %+v", opts)
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		if strings.Contains(sb.String(), "nenya_auto_cache_breakpoints_total{") {
			t.Fatalf("an opted-out agent must not emit outcome telemetry:\n%s", sb.String())
		}
	})
}

// The injected anchors must be byte-stable across turns AND land where the
// policy says: the system block carries a marker and the message marker sits
// on the last stable history message — the new turn never leaks into the
// anchored prefix.
func TestAutoCacheBreakpointsByteStableAcrossTurns(t *testing.T) {
	render := func(secondUserTurn string) map[string]interface{} {
		deps, cfg := autoCacheDeps(t, true)
		opts := adapter.CacheOpts{}
		payload := multiTurnPayload()
		msgs := payload["messages"].([]interface{})
		msgs[3] = map[string]interface{}{"role": "user", "content": secondUserTurn}
		applyAutoCacheBreakpoints(deps, payload, cfg.PrefixCache, &opts, false)
		if !opts.System || !opts.Messages {
			t.Fatalf("fixture must be anchored: %+v", opts)
		}
		opts.SystemTTL = "ephemeral"
		opts.MessagesTTL = "ephemeral"
		return adapter.GetAnthropicAdapter().ConvertOpenAIToAnthropicBody(payload, "claude-test", false, opts)
	}

	first := render("second")
	second := render("a completely different follow-up question")

	sysA := systemText(first)
	sysB := systemText(second)
	if sysA == "" || sysA != sysB {
		t.Fatalf("the anchored system prefix must be byte-stable and non-empty:\n%q\n%q", sysA, sysB)
	}
	if strings.Contains(sysA, "different follow-up") {
		t.Fatal("the new turn leaked into the anchored system prefix")
	}
	if !systemHasCacheControl(first) {
		t.Fatalf("the system block must carry the gateway's cache_control marker: %#v", first["system"])
	}
	// The message marker lands on the last STABLE history message (the
	// last-but-one), never on the new turn.
	markerIdx := messageMarkerIndex(first)
	msgs := first["messages"].([]interface{})
	if markerIdx < 0 {
		t.Fatalf("no message-level cache_control marker: %#v", msgs)
	}
	if markerIdx != len(msgs)-2 {
		t.Fatalf("message marker at index %d of %d, want the last stable history message (%d)", markerIdx, len(msgs), len(msgs)-2)
	}
	if text := messageText(msgs[len(msgs)-1]); strings.Contains(text, "different follow-up") && markerIdx == len(msgs)-1 {
		t.Fatal("the marker landed on the new turn")
	}
}

// NENYA-185 review: a one-shot request stays one-shot even when the gateway
// appended its canary marker message (which is not history).
func TestAutoCacheBreakpointsCanaryTrailerIsNotHistory(t *testing.T) {
	deps, cfg := autoCacheDeps(t, true)
	payload := map[string]interface{}{
		"model": "claude-test",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "only turn"},
			map[string]interface{}{"role": "system", "content": "\n[internal-ref] " + pipelineCanaryMarker()},
		},
	}
	opts := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps, payload, cfg.PrefixCache, &opts, false)
	if opts.System || opts.Messages {
		t.Fatalf("a canary trailer must not turn a one-shot into a multi-turn anchor: %+v", opts)
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `outcome="skipped_one_shot"`) {
		t.Fatalf("one-shot skip must be recorded:\n%s", sb.String())
	}
}

// NENYA-185 review: a trimmed history is gateway-produced at the tail of the
// array too, so the anchor stands down.
func TestAutoCacheBreakpointsStandDownOnTrimmedHistory(t *testing.T) {
	deps, cfg := autoCacheDeps(t, true)
	opts := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, true)
	if opts.System || opts.Messages {
		t.Fatalf("a trimmed history must not be anchored: %+v", opts)
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `outcome="skipped_trimmed"`) {
		t.Fatalf("trim skip must be recorded:\n%s", sb.String())
	}
}

// NENYA-185 review: automatic cache mode emits a single top-level marker and
// forces block markers off, so enabling them would be a recorded no-op.
func TestAutoCacheBreakpointsStandDownInAutomaticMode(t *testing.T) {
	deps, cfg := autoCacheDeps(t, true)
	mode := config.CacheModeAutomatic
	cfg.PrefixCache.CacheMode = &mode
	opts := adapter.CacheOpts{}
	applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, false)
	if opts.System || opts.Messages {
		t.Fatalf("automatic mode must stand the block-marker path down: %+v", opts)
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `outcome="skipped_automatic_mode"`) {
		t.Fatalf("automatic-mode skip must be recorded:\n%s", sb.String())
	}
}

// NENYA-185 review: when the explicit prefix-cache defaults already anchored
// the request, the outcome says so instead of claiming an injection.
func TestAutoCacheBreakpointsAlreadyAnchoredOutcome(t *testing.T) {
	deps, cfg := autoCacheDeps(t, true)
	opts := adapter.CacheOpts{System: true, Messages: true}
	applyAutoCacheBreakpoints(deps, multiTurnPayload(), cfg.PrefixCache, &opts, false)
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `outcome="already_anchored"`) {
		t.Fatalf("already-anchored outcome missing:\n%s", sb.String())
	}
}

func pipelineCanaryMarker() string {
	return pipeline.CanaryMarker("0123456789abcdef0123456789abcdef")
}

func systemText(body map[string]interface{}) string {
	switch sys := body["system"].(type) {
	case string:
		return sys
	case []interface{}:
		var sb strings.Builder
		for _, item := range sys {
			if m, ok := item.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

func systemHasCacheControl(body map[string]interface{}) bool {
	sys, ok := body["system"].([]interface{})
	if !ok {
		return false
	}
	for _, item := range sys {
		if m, ok := item.(map[string]interface{}); ok {
			if _, marked := m["cache_control"]; marked {
				return true
			}
		}
	}
	return false
}

func messageMarkerIndex(body map[string]interface{}) int {
	msgs, ok := body["messages"].([]interface{})
	if !ok {
		return -1
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}
		switch c := msg["content"].(type) {
		case []interface{}:
			for _, part := range c {
				if m, ok := part.(map[string]interface{}); ok {
					if _, marked := m["cache_control"]; marked {
						return i
					}
				}
			}
		}
	}
	return -1
}

func messageText(v interface{}) string {
	msg, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	switch c := msg["content"].(type) {
	case string:
		return c
	case []interface{}:
		var sb strings.Builder
		for _, part := range c {
			if m, ok := part.(map[string]interface{}); ok {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// NENYA-185: the decision is reached through the real dispatch funnel (the
// proxy sets TransformDeps.WindowCompacted), so the funnel test pins the
// windowed stand-down end-to-end.
func TestTransformRequestForUpstreamWindowedStandsDown(t *testing.T) {
	run := func(windowed bool) string {
		deps, cfg := autoCacheDeps(t, true)
		deps.WindowCompacted = windowed
		payload := multiTurnPayload()
		if _, _, err := TransformRequestForUpstream(deps, "prov", "http://up", payload, "claude-test", 0, 0, "anthropic", ""); err != nil {
			t.Fatalf("transform: %v", err)
		}
		var sb strings.Builder
		deps.Metrics.WritePrometheus(&sb)
		_ = cfg
		return sb.String()
	}
	if got := run(true); !strings.Contains(got, `outcome="skipped_windowed"`) || strings.Contains(got, `outcome="injected"`) {
		t.Fatalf("windowed dispatch must stand down:\n%s", got)
	}
	if got := run(false); !strings.Contains(got, `outcome="injected"`) {
		t.Fatalf("clean multi-turn dispatch must anchor:\n%s", got)
	}
}
