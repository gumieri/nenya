package proxy

import (
	"strings"
	"testing"

	mcpproxy "github.com/nenya/internal/proxy/mcp"

	"github.com/nenya/internal/routing"
)

// feedLines drives the accumulator with wire-format SSE data lines.
func feedLines(acc *sseAccumulator, lines []string) {
	for _, l := range lines {
		acc.ProcessLine(l)
	}
}

func anthropicToolLines() []string {
	return []string{
		`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"checking "}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Par"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"is\",\"unit\":\"c\"}"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null}}`,
	}
}

// TestSSEAccumulatorAnthropicToolCalls pins the AUD-10 fix (NENYA-218): an
// Anthropic-format target's tool_use blocks — id/name at content_block_start,
// arguments joined from input_json_delta fragments — must accumulate into
// tool calls the MCP loop can execute. Pre-fix the loop parsed OpenAI deltas
// only, so non-OpenAI-format chains replayed tool calls unexecuted.
func TestSSEAccumulatorAnthropicToolCalls(t *testing.T) {
	acc := newSSEAccumulator(nil)
	acc.Format = "anthropic"
	feedLines(acc, anthropicToolLines())

	buf := acc.Result()
	if len(buf.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %d, want 1", len(buf.ToolCalls))
	}
	call := buf.ToolCalls[0]
	if call.ID != "toolu_1" || call.Name != "get_weather" {
		t.Fatalf("call identity = %q/%q, want toolu_1/get_weather", call.ID, call.Name)
	}
	if city, _ := call.Arguments["city"].(string); city != "Paris" {
		t.Fatalf("args city = %v, want Paris (fragments must join into valid JSON)", call.Arguments)
	}
	if unit, _ := call.Arguments["unit"].(string); unit != "c" {
		t.Fatalf("args unit = %v, want c", call.Arguments)
	}
	if buf.FinishReason != "tool_use" {
		t.Fatalf("finishReason = %q, want tool_use", buf.FinishReason)
	}
	if !buf.HasContent {
		t.Fatal("text_delta must accumulate content")
	}
	if !strings.Contains(acc.Content.String(), "checking") {
		t.Fatalf("content = %q, want the text delta", acc.Content.String())
	}
	if acc.Model != "claude-x" {
		t.Fatalf("model = %q, want claude-x (message_start)", acc.Model)
	}
}

// TestSSEAccumulatorGeminiToolCalls pins the Gemini-native shape: complete
// functionCall parts get synthetic indices, args arrive parsed, and the
// part's thoughtSignature rides into ExtraContent (NENYA-51 parity).
func TestSSEAccumulatorGeminiToolCalls(t *testing.T) {
	acc := newSSEAccumulator(nil)
	acc.Format = "gemini"
	feedLines(acc, []string{
		`data: {"candidates":[{"content":{"parts":[{"text":"checking"},{"functionCall":{"name":"get_weather","args":{"city":"Paris"}},"thoughtSignature":"sig1"}]}}],"modelVersion":"gemini-x"}`,
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1}}`,
	})

	buf := acc.Result()
	if len(buf.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %d, want 1", len(buf.ToolCalls))
	}
	call := buf.ToolCalls[0]
	if call.Name != "get_weather" {
		t.Fatalf("call name = %q, want get_weather", call.Name)
	}
	if city, _ := call.Arguments["city"].(string); city != "Paris" {
		t.Fatalf("args = %v, want city=Paris", call.Arguments)
	}
	if call.ExtraContent != "sig1" {
		t.Fatalf("ExtraContent = %v, want sig1 (thoughtSignature parity)", call.ExtraContent)
	}
	if !buf.HasContent {
		t.Fatal("the text part must accumulate content")
	}
	if acc.Model != "gemini-x" {
		t.Fatalf("model = %q, want gemini-x", acc.Model)
	}
}

// TestSSEAccumulatorGeminiMultipleToolCalls: several functionCall parts in
// one chunk each become their own call.
func TestSSEAccumulatorGeminiMultipleToolCalls(t *testing.T) {
	acc := newSSEAccumulator(nil)
	acc.Format = "gemini"
	feedLines(acc, []string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":{}}},{"functionCall":{"name":"b","args":{"k":1}}}]}}]}`,
	})

	buf := acc.Result()
	if len(buf.ToolCalls) != 2 {
		t.Fatalf("toolCalls = %d, want 2", len(buf.ToolCalls))
	}
	if buf.ToolCalls[0].Name != "a" || buf.ToolCalls[1].Name != "b" {
		t.Fatalf("calls = %q,%q want a,b", buf.ToolCalls[0].Name, buf.ToolCalls[1].Name)
	}
}

// TestSSEAccumulatorOpenAIIgnoredForUnknownFormat pins backward compat: with
// no target format (the OpenAI default) the Anthropic lines must NOT produce
// tool calls — the OpenAI parse path is untouched.
func TestSSEAccumulatorOpenAIIgnoredForUnknownFormat(t *testing.T) {
	acc := newSSEAccumulator(nil)
	feedLines(acc, anthropicToolLines())

	buf := acc.Result()
	if len(buf.ToolCalls) != 0 {
		t.Fatalf("toolCalls = %d, want 0 for an unknown format", len(buf.ToolCalls))
	}
}

// TestSSEAccumulatorAnthropicHostileIndex: out-of-shape block indices are
// dropped, not trusted (the NENYA-48 class).
func TestSSEAccumulatorAnthropicHostileIndex(t *testing.T) {
	acc := newSSEAccumulator(nil)
	acc.Format = "anthropic"
	feedLines(acc, []string{
		`data: {"type":"content_block_start","index":-1,"content_block":{"type":"tool_use","id":"x","name":"evil","input":{}}}`,
		`data: {"type":"content_block_start","index":1.5,"content_block":{"type":"tool_use","id":"y","name":"evil2","input":{}}}`,
		`data: {"type":"content_block_start","index":100000000000,"content_block":{"type":"tool_use","id":"z","name":"evil3","input":{}}}`,
		`data: {"type":"content_block_start","index":"zero","content_block":{"type":"tool_use","id":"w","name":"evil4","input":{}}}`,
	})

	buf := acc.Result()
	if len(buf.ToolCalls) != 0 {
		t.Fatalf("hostile indices must be dropped, got %d calls", len(buf.ToolCalls))
	}
}

// TestBufferStreamResponseFormatWiring pins the end-to-end wiring: the
// accumulator's format comes from the serving target, so an anthropic-format
// UpstreamTarget yields executed tool calls from native SSE bytes.
func TestBufferStreamResponseFormatWiring(t *testing.T) {
	target := routing.UpstreamTarget{
		Provider: "p",
		Model:    "claude-x",
		Format:   "anthropic",
	}
	body := strings.Join(anthropicToolLines(), "\n") + "\n"
	buf, err := mcpproxy.BufferStreamResponse(t.Context(), strings.NewReader(body), nil, "claude-x", target)
	if err != nil {
		t.Fatalf("bufferStreamResponse: %v", err)
	}
	if len(buf.ToolCalls) != 1 || buf.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("buffered tool calls = %+v, want one get_weather", buf.ToolCalls)
	}
}

// TestSSEAccumulatorGeminiNoArgsCall: a parameterless functionCall carries
// empty-object arguments, never a nil accumulator.
func TestSSEAccumulatorGeminiNoArgsCall(t *testing.T) {
	acc := newSSEAccumulator(nil)
	acc.Format = "gemini"
	feedLines(acc, []string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping"}}]},"finishReason":"STOP"}]}`,
	})

	buf := acc.Result()
	if len(buf.ToolCalls) != 1 {
		t.Fatalf("toolCalls = %d, want 1", len(buf.ToolCalls))
	}
	if buf.ToolCalls[0].Arguments == nil {
		t.Fatal("a no-args call must carry an empty map, not nil")
	}
	if len(buf.ToolCalls[0].Arguments) != 0 {
		t.Fatalf("arguments = %v, want empty", buf.ToolCalls[0].Arguments)
	}
	if buf.FinishReason != "STOP" {
		t.Fatalf("finishReason = %q, want STOP", buf.FinishReason)
	}
}

// TestSSEAccumulatorExplicitOpenAIFormat: an explicit openai format routes
// through the OpenAI parse only.
func TestSSEAccumulatorExplicitOpenAIFormat(t *testing.T) {
	acc := newSSEAccumulator(nil)
	acc.Format = "openai"
	feedLines(acc, []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"f","arguments":"{}"}}]}}]}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"evil","input":{}}}`,
	})

	buf := acc.Result()
	if len(buf.ToolCalls) != 1 || buf.ToolCalls[0].Name != "f" {
		t.Fatalf("openai format must parse only OpenAI deltas, got %+v", buf.ToolCalls)
	}
}
