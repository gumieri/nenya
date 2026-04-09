package pipeline

import (
	"context"
	"log/slog"
	"testing"

	"nenya/internal/config"
)

func TestSerializeMessages(t *testing.T) {
	tests := []struct {
		name     string
		messages []interface{}
		want     string
	}{
		{
			name: "normal messages",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": "hello"},
				map[string]interface{}{"role": "assistant", "content": "hi"},
			},
			want: "user:\nhello\n\nassistant:\nhi\n\n",
		},
		{
			name: "skips empty content",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": ""},
				map[string]interface{}{"role": "assistant", "content": "hi"},
			},
			want: "assistant:\nhi\n\n",
		},
		{
			name: "skips nil content",
			messages: []interface{}{
				map[string]interface{}{"role": "user", "content": nil},
				map[string]interface{}{"role": "assistant", "content": "hi"},
			},
			want: "assistant:\nhi\n\n",
		},
		{
			name: "skips non-map entries",
			messages: []interface{}{
				"not-a-map",
				42,
				map[string]interface{}{"role": "user", "content": "hello"},
			},
			want: "user:\nhello\n\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SerializeMessages(tt.messages)
			if got != tt.want {
				t.Errorf("SerializeMessages() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractContentText(t *testing.T) {
	tests := []struct {
		name string
		msg  map[string]interface{}
		want string
	}{
		{
			name: "string content",
			msg:  map[string]interface{}{"content": "hello world"},
			want: "hello world",
		},
		{
			name: "array content multi-part",
			msg: map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "part1"},
					map[string]interface{}{"type": "text", "text": "part2"},
				},
			},
			want: "part1part2",
		},
		{
			name: "nil content",
			msg:  map[string]interface{}{"content": nil},
			want: "",
		},
		{
			name: "missing content key",
			msg:  map[string]interface{}{"role": "user"},
			want: "",
		},
		{
			name: "non-string non-array content",
			msg:  map[string]interface{}{"content": 42},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractContentText(tt.msg)
			if got != tt.want {
				t.Errorf("ExtractContentText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateHistory(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		maxRunes  int
		wantShort bool
	}{
		{
			name:      "short text no truncation",
			text:      "short",
			maxRunes:  100,
			wantShort: true,
		},
		{
			name:      "long text truncated with separator",
			text:      string(make([]rune, 10000)),
			maxRunes:  1000,
			wantShort: false,
		},
		{
			name:      "maxRunes zero defaults to 4000",
			text:      string(make([]rune, 10000)),
			maxRunes:  0,
			wantShort: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TruncateHistory(tt.text, tt.maxRunes)
			if tt.wantShort {
				if got != tt.text {
					t.Errorf("TruncateHistory() changed short text")
				}
			} else {
				if len(got) >= len(tt.text) {
					t.Errorf("TruncateHistory() did not truncate long text")
				}
			}
		})
	}

	t.Run("separator present in truncated output", func(t *testing.T) {
		text := string(make([]rune, 10000))
		got := TruncateHistory(text, 1000)
		expected := "[NENYA: HISTORY TRUNCATED]"
		for _, r := range expected {
			found := false
			for _, g := range got {
				if g == r {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("TruncateHistory() separator not found in output")
				return
			}
		}
	})
}

func TestApplyWindowCompaction(t *testing.T) {
	logger := slog.Default()
	noOpCount := func(p map[string]interface{}) int { return 0 }

	t.Run("disabled returns false", func(t *testing.T) {
		cfg := config.WindowConfig{Enabled: false}
		deps := WindowDeps{Logger: logger}
		payload := map[string]interface{}{"messages": []interface{}{}}
		messages := []interface{}{msg("user", "hello")}

		compacted, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 1000, cfg, 0, noOpCount)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compacted {
			t.Error("expected false for disabled config")
		}
	})

	t.Run("below threshold returns false", func(t *testing.T) {
		cfg := config.WindowConfig{
			Enabled:      true,
			TriggerRatio: 0.8,
			MaxContext:   10000,
			ActiveMessages: 2,
		}
		deps := WindowDeps{Logger: logger}
		payload := map[string]interface{}{"messages": []interface{}{}}
		messages := []interface{}{
			msg("user", "a"), msg("assistant", "b"),
			msg("user", "c"), msg("assistant", "d"),
		}

		compacted, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 1000, cfg, 10000, noOpCount)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compacted {
			t.Error("expected false below threshold")
		}
	})

	t.Run("not enough messages below active_messages", func(t *testing.T) {
		cfg := config.WindowConfig{
			Enabled:      true,
			TriggerRatio: 0.1,
			MaxContext:   100,
			ActiveMessages: 10,
		}
		deps := WindowDeps{Logger: logger}
		payload := map[string]interface{}{"messages": []interface{}{}}
		messages := []interface{}{
			msg("user", "a"), msg("assistant", "b"),
			msg("user", "c"), msg("assistant", "d"),
		}

		compacted, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, noOpCount)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compacted {
			t.Error("expected false when messages <= active_messages")
		}
	})

	t.Run("truncate mode compacts and creates summary", func(t *testing.T) {
		cfg := config.WindowConfig{
			Enabled:        true,
			Mode:           "truncate",
			TriggerRatio:   0.1,
			MaxContext:     100,
			ActiveMessages: 2,
			SummaryMaxRunes: 500,
		}
		deps := WindowDeps{Logger: logger}
		payload := map[string]interface{}{"messages": []interface{}{}}
		messages := []interface{}{
			msg("user", "old message 1"),
			msg("assistant", "old reply 1"),
			msg("user", "recent message"),
			msg("assistant", "recent reply"),
		}

		compacted, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, noOpCount)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !compacted {
			t.Error("expected compaction for truncate mode")
		}

		newMsgs, ok := payload["messages"].([]interface{})
		if !ok {
			t.Fatal("payload messages not set")
		}
		if len(newMsgs) < 3 {
			t.Errorf("expected at least 3 messages, got %d", len(newMsgs))
		}
		firstMsg, ok := newMsgs[0].(map[string]interface{})
		if !ok {
			t.Fatal("first message is not a map")
		}
		if firstMsg["role"] != "system" {
			t.Errorf("first message role = %q, want system", firstMsg["role"])
		}
		content, _ := firstMsg["content"].(string)
		if content == "" {
			t.Error("summary content is empty")
		}
	})

	t.Run("zero maxContext returns false", func(t *testing.T) {
		cfg := config.WindowConfig{
			Enabled:      true,
			TriggerRatio: 0.1,
			MaxContext:   0,
		}
		deps := WindowDeps{Logger: logger}
		payload := map[string]interface{}{"messages": []interface{}{}}
		messages := []interface{}{
			msg("user", "a"), msg("assistant", "b"),
			msg("user", "c"), msg("assistant", "d"),
		}

		compacted, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 0, noOpCount)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compacted {
			t.Error("expected false for zero maxContext")
		}
	})

	t.Run("unknown mode returns false", func(t *testing.T) {
		cfg := config.WindowConfig{
			Enabled:        true,
			Mode:           "unknown_mode",
			TriggerRatio:   0.1,
			MaxContext:     100,
			ActiveMessages: 2,
		}
		deps := WindowDeps{Logger: logger}
		payload := map[string]interface{}{"messages": []interface{}{}}
		messages := []interface{}{
			msg("user", "a"), msg("assistant", "b"),
			msg("user", "c"), msg("assistant", "d"),
		}

		compacted, err := ApplyWindowCompaction(context.Background(), deps, payload, messages, 500, cfg, 100, noOpCount)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if compacted {
			t.Error("expected false for unknown mode")
		}
	})
}
