package pipeline

import (
	"testing"

	"nenya/internal/config"
)

func TestPruneThoughts_Disabled(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: false,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "<think reasoning </think answer",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); mutated {
		t.Fatalf("expected false when pruning disabled, got true")
	}
}

func TestPruneThoughts_NoAssistantMessages(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "user",
				"content": "hello",
			},
			map[string]interface{}{
				"role":    "system",
				"content": "system message",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); mutated {
		t.Fatalf("expected false when no assistant messages, got true")
	}
}

func TestPruneThoughts_ReasoningContentField(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":             "assistant",
				"content":          "final answer",
				"reasoning_content": "massive reasoning block here",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	if _, exists := msg["reasoning_content"]; exists {
		t.Fatalf("expected reasoning_content to be removed")
	}
}

func TestPruneThoughts_EmptyReasoningContent(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":             "assistant",
				"content":          "final answer",
				"reasoning_content": "",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); mutated {
		t.Fatalf("expected false for empty reasoning_content, got true")
	}
}

func TestPruneThoughts_SimpleThoughtTags(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "<think This is the reasoning </think Final answer here",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)
	
	// The result should be the marker followed by the answer (no reasoning)
	// Note: The stripThoughtBlocks function returns the marker when all reasoning is stripped
	if content != "[Reasoning pruned by gateway] Final answer here" {
		t.Fatalf("expected '[Reasoning pruned by gateway] Final answer here', got %q", content)
	}
}

func TestPruneThoughts_ThoughtTagsWithSurroundingText(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "Let me think: <think complex reasoning steps </think The answer is 42.",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)
	
	expected := "Let me think: [Reasoning pruned by gateway] The answer is 42."
	if content != expected {
		t.Fatalf("expected %q, got %q", expected, content)
	}
}

func TestPruneThoughts_MultipleThoughtBlocks(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "<think first reasoning block </think intermediate <think second reasoning </think final answer",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)
	
	// Both blocks should be replaced with markers, and markers should be concatenated
	// Result: marker + " intermediate " + marker + " final answer"
	expected := "[Reasoning pruned by gateway] intermediate [Reasoning pruned by gateway] final answer"
	if content != expected {
		t.Fatalf("expected %q, got %q", expected, content)
	}
}

func TestPruneThoughts_UnclosedThoughtTag(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "<think reasoning that never ends due to stream interruption",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)
	
	// Everything from the opening tag onward should be replaced with the marker
	if content != "[Reasoning pruned by gateway]" {
		t.Fatalf("expected '[Reasoning pruned by gateway]', got %q", content)
	}
}

func TestPruneThoughts_NoThoughtTags(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "Just a regular answer with no reasoning tags.",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); mutated {
		t.Fatalf("expected false when no thought tags present, got true")
	}
}

func TestPruneThoughts_OnlyOpenTag(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "Starting reasoning <think",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)
	
	// The open tag and everything after should be replaced with marker
	if content != "Starting reasoning [Reasoning pruned by gateway]" {
		t.Fatalf("expected 'Starting reasoning [Reasoning pruned by gateway]', got %q", content)
	}
}

func TestPruneThoughts_OnlyCloseTag(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "Just some text with a stray </think closing tag",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); mutated {
		t.Fatalf("expected false for stray closing tag only, got true")
	}
}

func TestPruneThoughts_AdjacentThoughtTags(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "<think</thinkNo reasoning between tags",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)

	expected := "[Reasoning pruned by gateway]No reasoning between tags"
	if content != expected {
		t.Fatalf("expected %q, got %q", expected, content)
	}
}

func TestPruneThoughts_EmptyReasoningBetweenTags(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":    "assistant",
				"content": "<think </think Just whitespace reasoning",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	content := msg["content"].(string)
	
	expected := "[Reasoning pruned by gateway] Just whitespace reasoning"
	if content != expected {
		t.Fatalf("expected %q, got %q", expected, content)
	}
}

func TestPruneThoughts_ReasoningContentAndTagsBoth(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role":             "assistant",
				"content":          "<think reasoning in content </think final answer",
				"reasoning_content": "massive structured reasoning block",
			},
		},
	}

	if mutated := PruneThoughts(payload, cfg); !mutated {
		t.Fatalf("expected true, got false")
	}

	messages := payload["messages"].([]interface{})
	msg := messages[0].(map[string]interface{})
	
	// reasoning_content should be removed
	if _, exists := msg["reasoning_content"]; exists {
		t.Fatalf("expected reasoning_content to be removed")
	}
	
	// content should have tags stripped
	content := msg["content"].(string)
	expected := "[Reasoning pruned by gateway] final answer"
	if content != expected {
		t.Fatalf("expected %q, got %q", expected, content)
	}
}

func TestPruneThoughts_NilMessages(t *testing.T) {
	cfg := config.CompactionConfig{
		PruneThoughts: true,
	}

	payload := map[string]interface{}{}

	if mutated := PruneThoughts(payload, cfg); mutated {
		t.Fatalf("expected false for nil messages, got true")
	}
}