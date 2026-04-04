package main

import (
	"log/slog"
	"testing"
)

func TestPinSystemMessages(t *testing.T) {
	cfg := Config{
		PrefixCache: PrefixCacheConfig{Enabled: true, PinSystemFirst: true},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		name string
		in   []interface{}
		want []string
	}{
		{
			name: "already pinned",
			in: []interface{}{
				map[string]interface{}{"role": "system", "content": "sys"},
				map[string]interface{}{"role": "user", "content": "hi"},
			},
			want: []string{"sys", "hi"},
		},
		{
			name: "system scattered",
			in: []interface{}{
				map[string]interface{}{"role": "user", "content": "a"},
				map[string]interface{}{"role": "system", "content": "sys"},
				map[string]interface{}{"role": "assistant", "content": "b"},
			},
			want: []string{"sys", "a", "b"},
		},
		{
			name: "no system messages",
			in: []interface{}{
				map[string]interface{}{"role": "user", "content": "a"},
				map[string]interface{}{"role": "assistant", "content": "b"},
			},
			want: []string{"a", "b"},
		},
		{
			name: "single message",
			in: []interface{}{
				map[string]interface{}{"role": "user", "content": "a"},
			},
			want: []string{"a"},
		},
		{
			name: "empty",
			in:   []interface{}{},
			want: []string{},
		},
		{
			name: "multiple system messages",
			in: []interface{}{
				map[string]interface{}{"role": "user", "content": "a"},
				map[string]interface{}{"role": "system", "content": "s1"},
				map[string]interface{}{"role": "assistant", "content": "b"},
				map[string]interface{}{"role": "system", "content": "s2"},
			},
			want: []string{"s1", "s2", "a", "b"},
		},
		{
			name: "non-map entries preserved",
			in: []interface{}{
				"not a map",
				map[string]interface{}{"role": "system", "content": "sys"},
				map[string]interface{}{"role": "user", "content": "u"},
			},
			want: []string{"sys", "", "u"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			messages := make([]interface{}, len(tt.in))
			copy(messages, tt.in)
			g.pinSystemMessages(messages)
			if len(messages) != len(tt.want) {
				t.Fatalf("expected %d messages, got %d", len(tt.want), len(messages))
			}
			for i, want := range tt.want {
				msg, ok := messages[i].(map[string]interface{})
				if !ok {
					if want != "" {
						t.Errorf("message %d: expected content %q, got non-map", i, want)
					}
					continue
				}
				got, _ := msg["content"].(string)
				if got != want {
					t.Errorf("message %d: expected content %q, got %q", i, want, got)
				}
			}
		})
	}
}

func TestStabilizeTools(t *testing.T) {
	cfg := Config{
		PrefixCache: PrefixCacheConfig{Enabled: true, StableTools: true},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	tests := []struct {
		name      string
		tools     []interface{}
		wantNames []string
		wantMut   bool
	}{
		{
			name:      "empty tools",
			tools:     nil,
			wantNames: nil,
			wantMut:   false,
		},
		{
			name: "single tool",
			tools: []interface{}{
				map[string]interface{}{"function": map[string]interface{}{"name": "bash"}},
			},
			wantNames: []string{"bash"},
			wantMut:   false,
		},
		{
			name: "already sorted",
			tools: []interface{}{
				map[string]interface{}{"function": map[string]interface{}{"name": "alpha"}},
				map[string]interface{}{"function": map[string]interface{}{"name": "beta"}},
			},
			wantNames: []string{"alpha", "beta"},
			wantMut:   true,
		},
		{
			name: "unsorted",
			tools: []interface{}{
				map[string]interface{}{"function": map[string]interface{}{"name": "charlie"}},
				map[string]interface{}{"function": map[string]interface{}{"name": "alpha"}},
				map[string]interface{}{"function": map[string]interface{}{"name": "bravo"}},
			},
			wantNames: []string{"alpha", "bravo", "charlie"},
			wantMut:   true,
		},
		{
			name: "missing function key",
			tools: []interface{}{
				map[string]interface{}{"type": "code"},
				map[string]interface{}{"function": map[string]interface{}{"name": "alpha"}},
			},
			wantNames: []string{"", "alpha"},
			wantMut:   true,
		},
		{
			name: "non-map entries",
			tools: []interface{}{
				"not a map",
				map[string]interface{}{"function": map[string]interface{}{"name": "alpha"}},
			},
			wantNames: []string{"", "alpha"},
			wantMut:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]interface{}{"tools": tt.tools}
			mutated := g.stabilizeTools(payload)
			if mutated != tt.wantMut {
				t.Errorf("expected mutated=%v, got %v", tt.wantMut, mutated)
			}
			result, _ := payload["tools"].([]interface{})
			if len(result) != len(tt.wantNames) {
				t.Fatalf("expected %d tools, got %d", len(tt.wantNames), len(result))
			}
			for i, wantName := range tt.wantNames {
				tool, ok := result[i].(map[string]interface{})
				if !ok {
					if wantName != "" {
						t.Errorf("tool %d: expected name %q, got non-map", i, wantName)
					}
					continue
				}
				fn, _ := tool["function"].(map[string]interface{})
				if fn == nil {
					if wantName != "" {
						t.Errorf("tool %d: expected name %q, got no function", i, wantName)
					}
					continue
				}
				gotName, _ := fn["name"].(string)
				if gotName != wantName {
					t.Errorf("tool %d: expected name %q, got %q", i, wantName, gotName)
				}
			}
		})
	}
}

func TestToolSortKey(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  string
	}{
		{"nil", nil, ""},
		{"string", "not a map", ""},
		{"no function key", map[string]interface{}{"type": "code"}, ""},
		{"function not a map", map[string]interface{}{"function": "not a map"}, ""},
		{"no name in function", map[string]interface{}{"function": map[string]interface{}{}}, ""},
		{"valid", map[string]interface{}{"function": map[string]interface{}{"name": "bash"}}, "bash"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := toolSortKey(tt.input)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldSkipRedaction(t *testing.T) {
	tests := []struct {
		name     string
		cfg      PrefixCacheConfig
		role     string
		wantSkip bool
	}{
		{"system with flag enabled", PrefixCacheConfig{Enabled: true, SkipRedactionOnSystem: true}, "system", true},
		{"user with flag enabled", PrefixCacheConfig{Enabled: true, SkipRedactionOnSystem: true}, "user", false},
		{"assistant with flag enabled", PrefixCacheConfig{Enabled: true, SkipRedactionOnSystem: true}, "assistant", false},
		{"system with flag disabled", PrefixCacheConfig{Enabled: true, SkipRedactionOnSystem: false}, "system", false},
		{"user with prefix cache disabled", PrefixCacheConfig{Enabled: false}, "user", false},
		{"system with prefix cache disabled", PrefixCacheConfig{Enabled: false}, "system", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{PrefixCache: tt.cfg}
			g := NewNenyaGateway(cfg, &SecretsConfig{}, slog.Default())
			msg := map[string]interface{}{"role": tt.role}
			got := g.shouldSkipRedaction(msg)
			if got != tt.wantSkip {
				t.Errorf("got %v, want %v", got, tt.wantSkip)
			}
		})
	}
}

func TestApplyPrefixCacheOptimizations(t *testing.T) {
	cfg := Config{
		PrefixCache: PrefixCacheConfig{
			Enabled:               true,
			PinSystemFirst:        true,
			StableTools:           true,
			SkipRedactionOnSystem: true,
		},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "system", "content": "sys"},
	}
	tools := []interface{}{
		map[string]interface{}{"function": map[string]interface{}{"name": "beta"}},
		map[string]interface{}{"function": map[string]interface{}{"name": "alpha"}},
	}
	payload := map[string]interface{}{
		"messages": messages,
		"tools":    tools,
	}

	g.applyPrefixCacheOptimizations(payload, messages)

	resultMsgs := payload["messages"].([]interface{})
	role, _ := resultMsgs[0].(map[string]interface{})["role"].(string)
	if role != "system" {
		t.Errorf("expected first message role=system, got %q", role)
	}

	resultTools := payload["tools"].([]interface{})
	name0 := toolSortKey(resultTools[0])
	name1 := toolSortKey(resultTools[1])
	if name0 > name1 {
		t.Errorf("tools not sorted: %q > %q", name0, name1)
	}
}

func TestApplyPrefixCacheOptimizationsDisabled(t *testing.T) {
	cfg := Config{
		PrefixCache: PrefixCacheConfig{Enabled: false},
	}
	secrets := &SecretsConfig{}
	g := NewNenyaGateway(cfg, secrets, slog.Default())

	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{"role": "system", "content": "sys"},
	}
	tools := []interface{}{
		map[string]interface{}{"function": map[string]interface{}{"name": "beta"}},
		map[string]interface{}{"function": map[string]interface{}{"name": "alpha"}},
	}
	payload := map[string]interface{}{
		"messages": messages,
		"tools":    tools,
	}

	g.applyPrefixCacheOptimizations(payload, messages)

	resultMsgs := payload["messages"].([]interface{})
	role, _ := resultMsgs[0].(map[string]interface{})["role"].(string)
	if role != "user" {
		t.Errorf("expected first message role=user (no reorder), got %q", role)
	}

	resultTools := payload["tools"].([]interface{})
	name0 := toolSortKey(resultTools[0])
	if name0 != "beta" {
		t.Errorf("expected tools not reordered when disabled, first=%q", name0)
	}
}
