package stream

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// NENYA-178 — the effective-output classifier behind the time-to-first-token
// clock. The clock must stop at the first frame a client would render, in every
// wire format, and never on handshake/metadata/keepalive frames.

func TestHasEffectiveOutput(t *testing.T) {
	tests := []struct {
		name             string
		payload          string
		includeReasoning bool
		want             bool
	}{
		{name: "openai content delta", payload: `{"choices":[{"delta":{"content":"hi"}}]}`, want: true},
		{name: "openai tool call delta", payload: `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1"}]}}]}`, want: true},
		{name: "openai non-streaming message", payload: `{"choices":[{"message":{"content":"hi"}}]}`, want: true},
		{name: "openai role-only handshake", payload: `{"choices":[{"delta":{"role":"assistant"}}]}`, want: false},
		{name: "openai empty delta", payload: `{"choices":[{"delta":{}}]}`, want: false},
		{name: "openai empty content", payload: `{"choices":[{"delta":{"content":""}}]}`, want: false},
		{name: "openai finish-only", payload: `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, want: false},
		{name: "openai second choice only is not output (first choice decides)", payload: `{"choices":[{"delta":{}},{"delta":{"content":"hi"}}]}`, want: false},
		{name: "openai array content part", payload: `{"choices":[{"delta":{"content":[{"type":"text","text":"hi"}]}}]}`, want: true},
		{name: "openai legacy function_call", payload: `{"choices":[{"delta":{"function_call":{"name":"f"}}}]}`, want: true},
		{name: "openai reasoning field alias", payload: `{"choices":[{"delta":{"reasoning":"thinking"}}]}`, includeReasoning: true, want: true},
		{name: "openai refusal", payload: `{"choices":[{"delta":{"refusal":"I cannot help with that"}}]}`, want: true},
		{name: "openai empty refusal", payload: `{"choices":[{"delta":{"refusal":""}}]}`, want: false},
		{name: "openai audio delta", payload: `{"choices":[{"delta":{"audio":{"id":"a1","data":"x"}}}]}`, want: true},
		{name: "openai annotations", payload: `{"choices":[{"delta":{"annotations":[{"type":"url_citation"}]}}]}`, want: true},
		{name: "anthropic bare block start", payload: `{"type":"content_block_start","index":0}`, want: false},
		{name: "anthropic text block start", payload: `{"type":"content_block_start","content_block":{"type":"text","text":"hi"}}`, want: true},
		{name: "anthropic empty text block start", payload: `{"type":"content_block_start","content_block":{"type":"text"}}`, want: false},
		{name: "anthropic tool block start", payload: `{"type":"content_block_start","content_block":{"type":"tool_use","id":"t1"}}`, want: true},
		{name: "anthropic server tool block start", payload: `{"type":"content_block_start","content_block":{"type":"server_tool_use","id":"s1"}}`, want: true},
		{name: "anthropic mcp tool block start", payload: `{"type":"content_block_start","content_block":{"type":"mcp_tool_use","id":"m1"}}`, want: true},
		{name: "anthropic thinking block start, excluded", payload: `{"type":"content_block_start","content_block":{"type":"thinking"}}`, want: false},
		{name: "anthropic thinking block start, included", payload: `{"type":"content_block_start","content_block":{"type":"thinking"}}`, includeReasoning: true, want: true},
		{name: "anthropic content_block_delta", payload: `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`, want: true},
		{name: "anthropic empty text delta", payload: `{"type":"content_block_delta","delta":{"type":"text_delta","text":""}}`, want: false},
		{name: "anthropic thinking delta, excluded", payload: `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"t"}}`, want: false},
		{name: "anthropic thinking delta, included", payload: `{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"t"}}`, includeReasoning: true, want: true},
		{name: "anthropic signature delta", payload: `{"type":"content_block_delta","delta":{"type":"signature_delta","signature":"s"}}`, want: false},
		{name: "anthropic tool input delta", payload: `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{}"}}`, want: true},
		{name: "anthropic thinking delta spelled with text stays excluded", payload: `{"type":"content_block_delta","delta":{"type":"thinking_delta","text":"reasoning"}}`, want: false},
		{name: "anthropic content_block_stop", payload: `{"type":"content_block_stop","index":0}`, want: false},
		{name: "anthropic message_start", payload: `{"type":"message_start","message":{"role":"assistant"}}`, want: false},
		{name: "anthropic message_delta", payload: `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, want: false},
		{name: "anthropic ping", payload: `{"type":"ping"}`, want: false},
		{name: "gemini candidate with parts", payload: `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`, want: true},
		{name: "gemini candidate without parts", payload: `{"candidates":[{"content":{}}]}`, want: false},
		{name: "gemini empty text part", payload: `{"candidates":[{"content":{"parts":[{"text":""}]}}]}`, want: false},
		{name: "gemini thought-only part, excluded", payload: `{"candidates":[{"content":{"parts":[{"text":"t","thought":true}]}}]}`, want: false},
		{name: "gemini thought-only part, included", payload: `{"candidates":[{"content":{"parts":[{"text":"t","thought":true}]}}]}`, includeReasoning: true, want: true},
		{name: "gemini signature-only part", payload: `{"candidates":[{"content":{"parts":[{"thoughtSignature":"s"}]}}]}`, want: false},
		{name: "gemini text part carrying a thought signature", payload: `{"candidates":[{"content":{"parts":[{"text":"hi","thoughtSignature":"s"}]}}]}`, want: true},
		{name: "gemini signature-only part beside a text part", payload: `{"candidates":[{"content":{"parts":[{"thoughtSignature":"s"},{"text":"hi"}]}}]}`, want: true},
		{name: "gemini second candidate only is not output", payload: `{"candidates":[{"content":{}},{"content":{"parts":[{"text":"hi"}]}}]}`, want: false},
		{name: "gemini function call part", payload: `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"f"}}]}}]}`, want: true},
		{name: "gemini inline data part", payload: `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png"}}]}}]}`, want: true},
		{name: "gemini executable code part", payload: `{"candidates":[{"content":{"parts":[{"executableCode":{"language":"PYTHON","code":"1"}}]}}]}`, want: true},
		{name: "gemini code execution result part", payload: `{"candidates":[{"content":{"parts":[{"codeExecutionResult":{"outcome":"OK"}}]}}]}`, want: true},
		{name: "gemini file data part", payload: `{"candidates":[{"content":{"parts":[{"fileData":{"mimeType":"image/png"}}]}}]}`, want: true},
		{name: "gemini prompt feedback", payload: `{"promptFeedback":{"blockReason":"SAFETY"}}`, want: false},
		{name: "usage frame", payload: `{"usage":{"prompt_tokens":1,"completion_tokens":2}}`, want: false},
		{name: "reasoning only, excluded", payload: `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, want: false},
		{name: "reasoning only, included", payload: `{"choices":[{"delta":{"reasoning_content":"thinking"}}]}`, includeReasoning: true, want: true},
		{name: "reasoning alongside content", payload: `{"choices":[{"delta":{"reasoning_content":"t","content":"hi"}}]}`, want: true},
		{name: "junk types", payload: `{"choices":"nope","candidates":42,"type":7}`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed := ParseSSEChunk([]byte(tt.payload))
			if parsed == nil {
				t.Fatalf("payload did not parse: %s", tt.payload)
			}
			if got := HasEffectiveOutput(parsed, tt.includeReasoning); got != tt.want {
				t.Fatalf("HasEffectiveOutput(%s, %v) = %v, want %v", tt.payload, tt.includeReasoning, got, tt.want)
			}
		})
	}
	if HasEffectiveOutput(nil, true) {
		t.Error("a nil payload has no effective output")
	}
}

// TestSSETransformingReader_OnFirstEffectiveOutput pins the one-shot clock: the
// callback fires exactly once, at the first output-bearing frame, and never on a
// keepalive, an empty delta, a role-only handshake, a usage frame or (by
// default) a reasoning-only frame.
func TestSSETransformingReader_OnFirstEffectiveOutput(t *testing.T) {
	handshake := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":1}}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n"

	t.Run("fires once at the first content frame", func(t *testing.T) {
		input := handshake + "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"there\"}}]}\n\ndata: [DONE]\n\n"
		fired := 0
		r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		r.SetOnFirstEffectiveOutput(func() { fired++ }, false)
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if fired != 1 {
			t.Fatalf("callback fired %d times, want exactly 1", fired)
		}
	})

	t.Run("does not fire on a reasoning-only stream by default", func(t *testing.T) {
		fired := 0
		r := NewSSETransformingReader(strings.NewReader(handshake+"data: [DONE]\n\n"), nil, context.Background())
		r.SetOnFirstEffectiveOutput(func() { fired++ }, false)
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if fired != 0 {
			t.Fatalf("callback fired %d times, want 0 (no effective output)", fired)
		}
	})

	t.Run("fires on a reasoning frame when opted in", func(t *testing.T) {
		fired := 0
		r := NewSSETransformingReader(strings.NewReader(handshake+"data: [DONE]\n\n"), nil, context.Background())
		r.SetOnFirstEffectiveOutput(func() { fired++ }, true)
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if fired != 1 {
			t.Fatalf("callback fired %d times, want 1 (reasoning counts when opted in)", fired)
		}
	})

	t.Run("fires for an anthropic content block", func(t *testing.T) {
		input := "data: {\"type\":\"message_start\",\"message\":{\"role\":\"assistant\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"
		fired := 0
		r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		r.SetOnFirstEffectiveOutput(func() { fired++ }, false)
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if fired != 1 {
			t.Fatalf("callback fired %d times, want 1", fired)
		}
	})

	t.Run("fires for a gemini candidate", func(t *testing.T) {
		input := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n"
		fired := 0
		r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
		r.SetOnFirstEffectiveOutput(func() { fired++ }, false)
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatalf("copy: %v", err)
		}
		if fired != 1 {
			t.Fatalf("callback fired %d times, want 1", fired)
		}
	})

	t.Run("nil callback is safe", func(t *testing.T) {
		var out bytes.Buffer
		r := NewSSETransformingReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), nil, context.Background())
		r.SetOnFirstEffectiveOutput(nil, false)
		if _, err := io.Copy(&out, r); err != nil {
			t.Fatalf("copy: %v", err)
		}
		// The nil hook must not disturb the pass-through.
		if !strings.Contains(out.String(), `"content":"hi"`) {
			t.Fatalf("content was not forwarded with a nil hook: %q", out.String())
		}
	})
}

// TestSSETransformingReader_FirstOutputClearedByReset pins the one-shot contract
// across a counter reset (NENYA-178): ResetCounters clears the hook (the caller
// must re-register it to measure a further stream) while the reasoning-inclusion
// configuration survives the reset.
func TestSSETransformingReader_FirstOutputClearedByReset(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	fired := 0
	r := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	r.SetOnFirstEffectiveOutput(func() { fired++ }, true)
	if _, err := io.Copy(io.Discard, r); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if fired != 1 {
		t.Fatalf("fired %d times before reset, want 1", fired)
	}

	r.ResetCounters()
	if !r.ttftIncludeReasoning {
		t.Fatal("the reasoning-inclusion configuration must survive ResetCounters")
	}
	if r.onFirstOutput != nil {
		t.Fatal("ResetCounters must clear the one-shot hook")
	}
	// The hook is cleared by the reset: the next stream is not measured until
	// the caller re-registers it.
	r.SetOnFirstEffectiveOutput(func() { fired++ }, false)
	r.callUsageAndContentCallbacks(ParseSSEChunk([]byte(`{"choices":[{"delta":{"content":"again"}}]}`)))
	if fired != 2 {
		t.Fatalf("fired %d times after re-registration, want 2", fired)
	}
}
