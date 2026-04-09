package pipeline

import (
	"testing"

	"nenya/internal/config"
)

func msg(role, content string) map[string]interface{} {
	return map[string]interface{}{"role": role, "content": content}
}

func TestPinSystemMessages(t *testing.T) {
	tests := []struct {
		name      string
		input     []interface{}
		wantPin   bool
		wantRoles []string
	}{
		{
			name:      "already pinned system first",
			input:     []interface{}{msg("system", "s1"), msg("user", "u1"), msg("assistant", "a1")},
			wantPin:   false,
			wantRoles: []string{"system", "user", "assistant"},
		},
		{
			name:      "system scattered in middle",
			input:     []interface{}{msg("user", "u1"), msg("system", "s1"), msg("user", "u2")},
			wantPin:   true,
			wantRoles: []string{"system", "user", "user"},
		},
		{
			name:      "no system messages",
			input:     []interface{}{msg("user", "u1"), msg("assistant", "a1")},
			wantPin:   false,
			wantRoles: []string{"user", "assistant"},
		},
		{
			name:      "single message no change",
			input:     []interface{}{msg("system", "s1")},
			wantPin:   false,
			wantRoles: []string{"system"},
		},
		{
			name:      "empty",
			input:     []interface{}{},
			wantPin:   false,
			wantRoles: nil,
		},
		{
			name: "multiple system messages",
			input: []interface{}{
				msg("user", "u1"),
				msg("system", "s1"),
				msg("user", "u2"),
				msg("system", "s2"),
				msg("assistant", "a1"),
			},
			wantPin:   true,
			wantRoles: []string{"system", "system", "user", "user", "assistant"},
		},
		{
			name:      "non-map entries preserved",
			input:     []interface{}{"not-a-map", msg("system", "s1"), msg("user", "u1")},
			wantPin:   true,
			wantRoles: []string{"system", "not-a-map", "user"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PinSystemMessages(tt.input)
			if got != tt.wantPin {
				t.Errorf("PinSystemMessages() = %v, want %v", got, tt.wantPin)
			}
			if tt.wantRoles != nil {
				for i, want := range tt.wantRoles {
					var gotRole string
					if m, ok := tt.input[i].(map[string]interface{}); ok {
						gotRole, _ = m["role"].(string)
					} else if s, ok := tt.input[i].(string); ok {
						gotRole = s
					}
					if gotRole != want {
						t.Errorf("messages[%d].role = %q, want %q", i, gotRole, want)
					}
				}
			}
		})
	}
}

func TestStabilizeTools(t *testing.T) {
	tool := func(name string) map[string]interface{} {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": "tool " + name,
			},
		}
	}

	tests := []struct {
		name      string
		payload   map[string]interface{}
		want      bool
		wantNames []string
	}{
		{
			name:    "nil tools",
			payload: map[string]interface{}{},
			want:    false,
		},
		{
			name:    "missing tools key",
			payload: map[string]interface{}{"model": "gpt-4"},
			want:    false,
		},
		{
			name: "single tool no sort needed",
			payload: map[string]interface{}{
				"tools": []interface{}{tool("alpha")},
			},
			want:      false,
			wantNames: []string{"alpha"},
		},
		{
			name: "already sorted",
			payload: map[string]interface{}{
				"tools": []interface{}{tool("alpha"), tool("beta"), tool("gamma")},
			},
			want:      true,
			wantNames: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "unsorted needs sorting",
			payload: map[string]interface{}{
				"tools": []interface{}{tool("gamma"), tool("alpha"), tool("beta")},
			},
			want:      true,
			wantNames: []string{"alpha", "beta", "gamma"},
		},
		{
			name: "missing function key",
			payload: map[string]interface{}{
				"tools": []interface{}{
					map[string]interface{}{"type": "function", "no_function": true},
					map[string]interface{}{"type": "function", "no_function": true},
				},
			},
			want:      true,
			wantNames: []string{"", ""},
		},
		{
			name: "non-map entries",
			payload: map[string]interface{}{
				"tools": []interface{}{"string-tool", 42},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StabilizeTools(tt.payload)
			if got != tt.want {
				t.Errorf("StabilizeTools() = %v, want %v", got, tt.want)
			}
			if tt.wantNames != nil {
				tools, _ := tt.payload["tools"].([]interface{})
				for i, want := range tt.wantNames {
					got := ToolSortKey(tools[i])
					if got != want {
						t.Errorf("tools[%d] sort key = %q, want %q", i, got, want)
					}
				}
			}
		})
	}
}

func TestToolSortKey(t *testing.T) {
	tests := []struct {
		name string
		tool interface{}
		want string
	}{
		{
			name: "normal tool with function name",
			tool: map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "read_file",
					"description": "reads a file",
				},
			},
			want: "read_file",
		},
		{
			name: "missing function key",
			tool: map[string]interface{}{"type": "function"},
			want: "",
		},
		{
			name: "function without name",
			tool: map[string]interface{}{"function": map[string]interface{}{"description": "no name"}},
			want: "",
		},
		{
			name: "non-map input",
			tool: "not-a-map",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ToolSortKey(tt.tool)
			if got != tt.want {
				t.Errorf("ToolSortKey() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestShouldSkipRedaction(t *testing.T) {
	tests := []struct {
		name string
		msg  map[string]interface{}
		cfg  config.PrefixCacheConfig
		want bool
	}{
		{
			name: "enabled skip on system system role",
			msg:  map[string]interface{}{"role": "system", "content": "instructions"},
			cfg:  config.PrefixCacheConfig{Enabled: true, SkipRedactionOnSystem: true},
			want: true,
		},
		{
			name: "enabled skip on system user role",
			msg:  map[string]interface{}{"role": "user", "content": "hello"},
			cfg:  config.PrefixCacheConfig{Enabled: true, SkipRedactionOnSystem: true},
			want: false,
		},
		{
			name: "disabled",
			msg:  map[string]interface{}{"role": "system", "content": "instructions"},
			cfg:  config.PrefixCacheConfig{Enabled: false, SkipRedactionOnSystem: true},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldSkipRedaction(tt.msg, tt.cfg)
			if got != tt.want {
				t.Errorf("ShouldSkipRedaction() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyPrefixCacheOptimizations(t *testing.T) {
	t.Run("disabled no mutation", func(t *testing.T) {
		payload := map[string]interface{}{
			"tools": []interface{}{
				map[string]interface{}{
					"type":     "function",
					"function": map[string]interface{}{"name": "z_tool"},
				},
				map[string]interface{}{
					"type":     "function",
					"function": map[string]interface{}{"name": "a_tool"},
				},
			},
		}
		messages := []interface{}{
			msg("user", "u1"),
			msg("system", "s1"),
		}
		cfg := config.PrefixCacheConfig{Enabled: false, PinSystemFirst: true, StableTools: true}

		ApplyPrefixCacheOptimizations(payload, messages, cfg)

		tools, _ := payload["tools"].([]interface{})
		if ToolSortKey(tools[0]) != "z_tool" {
			t.Error("tools were sorted despite disabled config")
		}
		if messages[0].(map[string]interface{})["role"] != "user" {
			t.Error("messages were reordered despite disabled config")
		}
	})

	t.Run("enabled with pin and stable both applied", func(t *testing.T) {
		payload := map[string]interface{}{
			"tools": []interface{}{
				map[string]interface{}{
					"type":     "function",
					"function": map[string]interface{}{"name": "z_tool"},
				},
				map[string]interface{}{
					"type":     "function",
					"function": map[string]interface{}{"name": "a_tool"},
				},
			},
		}
		messages := []interface{}{
			msg("user", "u1"),
			msg("system", "s1"),
		}
		cfg := config.PrefixCacheConfig{Enabled: true, PinSystemFirst: true, StableTools: true}

		ApplyPrefixCacheOptimizations(payload, messages, cfg)

		if messages[0].(map[string]interface{})["role"] != "system" {
			t.Error("system messages not pinned to front")
		}
		tools, _ := payload["tools"].([]interface{})
		if ToolSortKey(tools[0]) != "a_tool" {
			t.Error("tools not sorted")
		}
	})
}
