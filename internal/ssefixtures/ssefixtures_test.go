package ssefixtures

// The Test*Sanity functions are the corpus gate (5a8943f0): every fixture
// must parse and carry the shapes the benchmarks assert on — a fixture that
// drifts into an unparseable shape would silently benchmark nothing.

import (
	"encoding/json"
	"strings"
	"testing"
)

// dataLines splits an SSE wire fixture into its data payloads.
func dataLines(t *testing.T, wire string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(wire, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			lines = append(lines, strings.TrimPrefix(line, "data: "))
		}
	}
	if len(lines) == 0 {
		t.Fatal("fixture carries no data lines")
	}
	return lines
}

func mustParse(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("fixture data line is not JSON: %v (%q)", err, line)
	}
	return m
}

func TestOpenAIToolCallStreamSanity(t *testing.T) {
	wire := OpenAIToolCallStream()
	lines := dataLines(t, wire)
	// role + 3 content + 3×3 tool-call + finish + usage + [DONE]
	if len(lines) != 1+3+9+1+1+1 {
		t.Fatalf("data lines = %d, want 16", len(lines))
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("last line = %q, want [DONE]", lines[len(lines)-1])
	}
	// The role chunk: delta.role must be present (the client's handshake).
	roleChunk := mustParse(t, lines[0])
	roleChoice, ok := roleChunk["choices"].([]any)
	if !ok || len(roleChoice) == 0 {
		t.Fatalf("role chunk carries no choices: %v", roleChunk)
	}
	roleChoiceObj, ok := roleChoice[0].(map[string]any)
	if !ok {
		t.Fatalf("role chunk choice is not an object: %v", roleChoice[0])
	}
	roleDelta, ok := roleChoiceObj["delta"].(map[string]any)
	if !ok {
		t.Fatalf("role chunk delta is not an object: %v", roleChoiceObj)
	}
	if roleDelta["role"] != "assistant" {
		t.Errorf("first delta role = %v, want assistant", roleDelta["role"])
	}

	toolCalls := 0
	var finish string
	var usage map[string]any
	for _, line := range lines[1 : len(lines)-1] {
		chunk := mustParse(t, line)
		choices, ok := chunk["choices"].([]any)
		if !ok || len(choices) == 0 {
			if u, hasUsage := chunk["usage"].(map[string]any); hasUsage {
				usage = u
			}
			continue
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			t.Fatal("choice entry is not an object")
		}
		if fr, hasFinish := choice["finish_reason"].(string); hasFinish && fr != "" {
			finish = fr
		}
		delta, hasDelta := choice["delta"].(map[string]any)
		if !hasDelta {
			continue
		}
		if tcs, hasCalls := delta["tool_calls"].([]any); hasCalls && len(tcs) > 0 {
			tc, ok := tcs[0].(map[string]any)
			if !ok {
				t.Fatal("tool_call entry is not an object")
			}
			if tc["id"] != nil {
				toolCalls++
			}
		}
	}
	if toolCalls != 3 {
		t.Errorf("tool-call opening deltas = %d, want 3", toolCalls)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
	if usage == nil || usage["prompt_tokens"] != float64(812) {
		t.Errorf("usage chunk missing or wrong: %v", usage)
	}
}

func TestResponsesToolStreamSanity(t *testing.T) {
	lines := dataLines(t, ResponsesToolStream())
	// created + in_progress + message added + part added + 3 text deltas +
	// text done + part done + message item done + fc added + 2 arg deltas +
	// fc item done + completed
	if len(lines) != 15 {
		t.Fatalf("data lines = %d, want 15", len(lines))
	}
	completed := mustParse(t, lines[len(lines)-1])
	response, ok := completed["response"].(map[string]any)
	if !ok || response["status"] != "completed" {
		t.Fatalf("terminal event = %v, want a completed response", completed)
	}
	if usage, ok := response["usage"].(map[string]any); !ok || usage["input_tokens"] != float64(812) {
		t.Errorf("terminal usage = %v, want input_tokens 812", response["usage"])
	}
}

func TestAnthropicThinkingToolStreamSanity(t *testing.T) {
	lines := dataLines(t, AnthropicThinkingToolStream())
	// start + 3 block starts + 4 deltas + 3 stops + message_delta + stop
	if len(lines) != 13 {
		t.Fatalf("data lines = %d, want 13", len(lines))
	}
	kinds := map[string]int{}
	for _, line := range lines {
		ev := mustParse(t, line)
		evType, _ := ev["type"].(string)
		kinds[evType]++
		if evType == "content_block_delta" {
			delta, ok := ev["delta"].(map[string]any)
			if !ok {
				t.Fatalf("content_block_delta carries no delta object: %v", ev)
			}
			deltaType, _ := delta["type"].(string)
			kinds[deltaType]++
		}
		if evType == "message_delta" {
			delta, ok := ev["delta"].(map[string]any)
			if !ok {
				t.Fatalf("message_delta carries no delta object: %v", ev)
			}
			if delta["stop_reason"] != "tool_use" {
				t.Errorf("stop_reason = %v, want tool_use", delta["stop_reason"])
			}
		}
	}
	if kinds["input_json_delta"] != 2 {
		t.Errorf("input_json_delta count = %d, want 2 (split arguments)", kinds["input_json_delta"])
	}
	if kinds["thinking_delta"] != 1 {
		t.Errorf("thinking_delta count = %d, want 1", kinds["thinking_delta"])
	}
}

func TestChatPayloadJSONSanity(t *testing.T) {
	body := ChatPayloadJSON(21, 400)
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload does not parse: %v", err)
	}
	msgs, ok := payload["messages"].([]any)
	// 21 = system + 19 alternating + assistant tool_calls + 2 paired results
	if !ok || len(msgs) != 23 {
		t.Fatalf("messages = %d, want 23", len(msgs))
	}
	if payload["reasoning_effort"] != "medium" {
		t.Errorf("reasoning_effort = %v, want medium (the incident shape)", payload["reasoning_effort"])
	}
	tools, hasTools := payload["tools"].([]any)
	if !hasTools || len(tools) != 2 {
		t.Fatalf("tools = %d, want 2", len(tools))
	}
	assistant, isMap := msgs[len(msgs)-3].(map[string]any) // the assistant turn before the paired tool results
	if !isMap {
		t.Fatalf("assistant turn is not an object: %v", msgs[len(msgs)-3])
	}
	if tcs, okCalls := assistant["tool_calls"].([]any); !okCalls || len(tcs) != 2 {
		t.Errorf("assistant turn tool_calls = %v, want 2", assistant["tool_calls"])
	}
	result0, ok := msgs[len(msgs)-2].(map[string]any)
	if !ok {
		t.Fatalf("tool result 0 is not an object: %v", msgs[len(msgs)-2])
	}
	result1, ok := msgs[len(msgs)-1].(map[string]any)
	if !ok {
		t.Fatalf("tool result 1 is not an object: %v", msgs[len(msgs)-1])
	}
	if result0["tool_call_id"] != "call_payload_0" || result1["tool_call_id"] != "call_payload_1" {
		t.Errorf("the tool results must be paired with the calls (call_payload_0/1)")
	}
}
