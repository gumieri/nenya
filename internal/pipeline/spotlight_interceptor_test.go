package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/nenya/config"
)

func newTestSpotlightInterceptor(t *testing.T, cfg *config.SpotlightConfig, agents map[string]config.AgentConfig) *SpotlightInterceptor {
	t.Helper()
	return NewSpotlightInterceptor(cfg, agents, nil)
}

func spotReq(model string, msgs ...map[string]any) *InterceptRequest {
	return &InterceptRequest{
		Payload:  map[string]any{"model": model},
		Messages: msgs,
	}
}

func TestSpotlightInterceptorWrapsToolMessages(t *testing.T) {
	interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{
		Enabled:        config.PtrTo(true),
		HistoryEnabled: config.PtrTo(true),
	}, nil)

	req := spotReq("demo-agent",
		map[string]any{"role": "user", "content": "question"},
		map[string]any{"role": "assistant", "content": "calling tool", "tool_calls": []any{}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "tool output with instructions"},
		map[string]any{"role": "assistant", "content": "answer"},
	)

	res, err := interceptor.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Skip {
		t.Fatal("expected modification")
	}

	toolMsg := req.Messages[2]
	content, _ := toolMsg["content"].(string)
	if !strings.Contains(content, `source="tool-history"`) {
		t.Errorf("expected history envelope on tool message, got %q", content)
	}
	if !strings.Contains(content, SpotlightPreamble) {
		t.Errorf("expected preamble on first tool message, got %q", content)
	}
	// Non-tool messages untouched.
	if user, _ := req.Messages[0]["content"].(string); strings.Contains(user, "untrusted-content") {
		t.Errorf("expected user message untouched, got %q", user)
	}
}

func TestSpotlightInterceptorPreambleOnce(t *testing.T) {
	interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{
		Enabled:        config.PtrTo(true),
		HistoryEnabled: config.PtrTo(true),
	}, nil)

	req := spotReq("demo-agent",
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "first result"},
		map[string]any{"role": "tool", "tool_call_id": "c2", "content": "second result"},
	)

	if _, err := interceptor.Process(context.Background(), req); err != nil {
		t.Fatalf("Process: %v", err)
	}
	first, _ := req.Messages[0]["content"].(string)
	second, _ := req.Messages[1]["content"].(string)
	if strings.Count(first, SpotlightPreamble)+strings.Count(second, SpotlightPreamble) != 1 {
		t.Errorf("expected preamble exactly once, first=%q second=%q", first, second)
	}
}

func TestSpotlightInterceptorContentArrays(t *testing.T) {
	interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{
		Enabled:        config.PtrTo(true),
		HistoryEnabled: config.PtrTo(true),
	}, nil)

	req := spotReq("demo-agent",
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": []any{
			map[string]any{"type": "text", "text": "part one"},
			map[string]any{"type": "image_url", "url": "http://example.com/x.png"},
		}},
	)

	if _, err := interceptor.Process(context.Background(), req); err != nil {
		t.Fatalf("Process: %v", err)
	}
	part := req.Messages[0]["content"].([]any)[0].(map[string]any)
	if !strings.Contains(part["text"].(string), `source="tool-history"`) {
		t.Errorf("expected array text part enveloped, got %v", part["text"])
	}
	img := req.Messages[0]["content"].([]any)[1].(map[string]any)
	if img["url"] != "http://example.com/x.png" {
		t.Errorf("expected non-text part untouched, got %v", img["url"])
	}
}

func TestSpotlightInterceptorReEnvelopes(t *testing.T) {
	interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{
		Enabled:        config.PtrTo(true),
		HistoryEnabled: config.PtrTo(true),
	}, nil)

	req := spotReq("demo-agent",
		map[string]any{"role": "tool", "content": "result"},
	)
	if _, err := interceptor.Process(context.Background(), req); err != nil {
		t.Fatalf("first Process: %v", err)
	}
	once := req.Messages[0]["content"].(string)
	// A second pass (fresh history resend) re-envelopes around the
	// defanged prior envelope — content never escapes the outer tag.
	res, err := interceptor.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("second Process: %v", err)
	}
	if res.Skip {
		t.Error("expected re-wrap, not Skip")
	}
	twice := req.Messages[0]["content"].(string)
	if !strings.Contains(twice, `<untrusted-content source="tool-history">`) {
		t.Errorf("expected fresh live envelope, got %q", twice)
	}
	if !strings.Contains(twice, spotlightDefangClose) || strings.Count(twice, spotlightClose) != 1 {
		t.Errorf("expected prior envelope defanged with single live close, got %q", twice)
	}
	if len(twice) <= len(once) {
		t.Errorf("expected growth from re-wrap, got %d vs %d", len(twice), len(once))
	}
}

func TestSpotlightInterceptorDisabled(t *testing.T) {
	interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{Enabled: config.PtrTo(false)}, nil)
	req := spotReq("demo-agent", map[string]any{"role": "tool", "content": "result"})
	if interceptor.CanHandle(context.Background(), req) {
		t.Fatal("expected CanHandle false when history disabled")
	}
	res, err := interceptor.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Skip {
		t.Error("expected Skip when disabled")
	}
}

func TestSpotlightInterceptorPerAgentOverride(t *testing.T) {
	agents := map[string]config.AgentConfig{
		"wrapped-agent": {Spotlight: &config.SpotlightConfig{Enabled: config.PtrTo(true), HistoryEnabled: config.PtrTo(true)}},
	}
	interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{}, agents)

	t.Run("agent override enables", func(t *testing.T) {
		req := spotReq("wrapped-agent", map[string]any{"role": "tool", "content": "result"})
		if !interceptor.CanHandle(context.Background(), req) {
			t.Fatal("expected CanHandle true for overridden agent")
		}
		if _, err := interceptor.Process(context.Background(), req); err != nil {
			t.Fatalf("Process: %v", err)
		}
		if !strings.Contains(req.Messages[0]["content"].(string), "tool-history") {
			t.Errorf("expected envelope, got %q", req.Messages[0]["content"])
		}
	})

	t.Run("other agents unaffected", func(t *testing.T) {
		req := spotReq("other-agent", map[string]any{"role": "tool", "content": "result"})
		if interceptor.CanHandle(context.Background(), req) {
			t.Error("expected CanHandle false without override")
		}
	})

	t.Run("agent enable implies history", func(t *testing.T) {
		interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{}, map[string]config.AgentConfig{
			"inheriting-agent": {Spotlight: &config.SpotlightConfig{Enabled: config.PtrTo(true)}},
		})
		req := spotReq("inheriting-agent", map[string]any{"role": "tool", "content": "result"})
		if !interceptor.CanHandle(context.Background(), req) {
			t.Fatal("expected CanHandle true: enabled agent inherits history")
		}
	})

	t.Run("explicit agent disable wins over global", func(t *testing.T) {
		interceptor := newTestSpotlightInterceptor(t, &config.SpotlightConfig{Enabled: config.PtrTo(true), HistoryEnabled: config.PtrTo(true)},
			map[string]config.AgentConfig{
				"disabled-agent": {Spotlight: &config.SpotlightConfig{Enabled: config.PtrTo(false)}},
			})
		req := spotReq("disabled-agent", map[string]any{"role": "tool", "content": "result"})
		if interceptor.CanHandle(context.Background(), req) {
			t.Error("expected CanHandle false: explicit agent disable beats global enable")
		}
	})
}

func TestSpotlightRegistrationRequired(t *testing.T) {
	t.Run("nothing configured", func(t *testing.T) {
		if (&SpotlightInterceptor{global: config.SpotlightConfig{}}).RegistrationRequired() {
			t.Error("expected false")
		}
	})
	t.Run("global enabled", func(t *testing.T) {
		i := NewSpotlightInterceptor(&config.SpotlightConfig{Enabled: config.PtrTo(true)}, nil, nil)
		if !i.RegistrationRequired() {
			t.Error("expected true for global enabled")
		}
	})
	t.Run("agent enabled", func(t *testing.T) {
		i := NewSpotlightInterceptor(&config.SpotlightConfig{}, map[string]config.AgentConfig{
			"a": {Spotlight: &config.SpotlightConfig{HistoryEnabled: config.PtrTo(true)}},
		}, nil)
		if !i.RegistrationRequired() {
			t.Error("expected true for agent history override")
		}
	})
}
