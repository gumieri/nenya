package ssefixtures

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAIToolCallStream renders a complete OpenAI chat-completions tool-call
// SSE stream: a role chunk, content deltas, 3 function calls with their
// arguments split across two deltas each (the shape the reader's tool-call
// joiner exists for), a finish_reason chunk, a trailing usage-only chunk,
// and [DONE]. Callers wanting a different call count can s/3/N/ — the
// hardcode keeps the fixture internally consistent (finish_reason matches
// the streamed calls).
func OpenAIToolCallStream() string {
	var b strings.Builder
	b.WriteString(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, `data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[{"index":0,"delta":{"content":"chunk %d of the assistant answer"},"finish_reason":null}]}`+"\n\n", i)
	}
	for c := 0; c < 3; c++ {
		name := fmt.Sprintf("bash_%d", c)
		callID := fmt.Sprintf("call_bench_%d", c)
		fmt.Fprintf(&b, `data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]},"finish_reason":null}]}`+"\n\n", c, callID, name)
		// Arguments split across two deltas (the reader joins them).
		fmt.Fprintf(&b, `data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":"{\"cmd\":\"ls -la ./pro"}}]},"finish_reason":null}]}`+"\n\n", c)
		fmt.Fprintf(&b, `data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"function":{"arguments":"ject\"}"}}]},"finish_reason":null}]}`+"\n\n", c)
	}
	b.WriteString(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString(`data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":0,"model":"gpt-6-luna","choices":[],"usage":{"prompt_tokens":812,"completion_tokens":194,"total_tokens":1006,"prompt_tokens_details":{"cached_tokens":256},"completion_tokens_details":{"reasoning_tokens":37}}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// ResponsesToolStream renders a complete Responses-API SSE stream (the
// NENYA-337 wire): created + in_progress, a message item with its content
// parts, text deltas with their done, a function_call item with arguments
// split across deltas, the terminal completed event carrying usage with the
// cached/reasoning decompositions.
func ResponsesToolStream() string {
	var b strings.Builder
	b.WriteString(`data: {"type":"response.created","response":{"id":"resp_bench","model":"gpt-6-luna"}}` + "\n\n")
	b.WriteString(`data: {"type":"response.in_progress","response":{"id":"resp_bench","model":"gpt-6-luna"}}` + "\n\n")
	b.WriteString(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant"}}` + "\n\n")
	b.WriteString(`data: {"type":"response.content_part.added","item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n")
	for i := 0; i < 3; i++ {
		fmt.Fprintf(&b, `data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"chunk %d of the assistant answer"}`+"\n\n", i)
	}
	const answer = "chunk 0 of the assistant answerchunk 1 of the assistant answerchunk 2 of the assistant answer"
	// Done events embed the accumulated answer text (production parity);
	// json.Marshal produces the JSON string literal (strconv.Quote would
	// emit Go escapes invalid in JSON for non-ASCII text).
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		panic(fmt.Sprintf("ssefixtures: answer literal: %v", err))
	}
	answerStr := string(answerJSON)
	b.WriteString(`data: {"type":"response.output_text.done","item_id":"msg_1","text":` + answerStr + `}` + "\n\n")
	b.WriteString(`data: {"type":"response.content_part.done","item_id":"msg_1","content_index":0,"part":{"type":"output_text","text":` + answerStr + `}}` + "\n\n")
	b.WriteString(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":` + answerStr + `}]}}` + "\n\n")
	b.WriteString(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_bench_0","name":"bash","arguments":""}}` + "\n\n")
	b.WriteString(`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"cmd\":\"ls -la ./pro"}` + "\n\n")
	b.WriteString(`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"ject\"}"}` + "\n\n")
	b.WriteString(`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_bench_0","name":"bash","arguments":"{\"cmd\":\"ls -la ./project\"}"}}` + "\n\n")
	b.WriteString(`data: {"type":"response.completed","response":{"id":"resp_bench","status":"completed","usage":{"input_tokens":812,"output_tokens":194,"total_tokens":1006,"input_tokens_details":{"cached_tokens":256},"output_tokens_details":{"reasoning_tokens":37}}}}` + "\n\n")
	return b.String()
}

// AnthropicThinkingToolStream renders a complete Anthropic Messages SSE
// stream: message_start, a thinking block, a text block, a tool_use block
// with input_json_delta fragments, message_delta (stop_reason tool_use +
// usage with the cache decompositions), message_stop.
func AnthropicThinkingToolStream() string {
	var b strings.Builder
	b.WriteString(`data: {"type":"message_start","message":{"id":"msg_bench","model":"claude-bench","usage":{"input_tokens":812,"cache_read_input_tokens":256,"cache_creation_input_tokens":41,"output_tokens":1}}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"the user wants a directory listing; bash is the right tool"}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_stop","index":0}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"running the listing now"}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_stop","index":1}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_bench_0","name":"bash","input":{}}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls -la ./pro"}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"ject\"}"}}` + "\n\n")
	b.WriteString(`data: {"type":"content_block_stop","index":2}` + "\n\n")
	b.WriteString(`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":194}}` + "\n\n")
	b.WriteString(`data: {"type":"message_stop"}` + "\n\n")
	return b.String()
}

// ChatPayloadJSON renders an OpenCode-shaped chat-completions request body:
// a system message, then nMessages-1 alternating user/assistant turns of
// roughly msgChars bytes each, with the last assistant turn carrying two
// tool calls and their paired tool results. Deterministic. nMessages must
// be ≥ 2 (system + final turns are always emitted) and msgChars ≥ 0.
func ChatPayloadJSON(nMessages, msgChars int) []byte {
	if nMessages < 2 {
		panic(fmt.Sprintf("ChatPayloadJSON: nMessages=%d, want >= 2 (system + final turns always emitted)", nMessages))
	}
	if msgChars < 0 {
		panic(fmt.Sprintf("ChatPayloadJSON: msgChars=%d, want >= 0", msgChars))
	}
	turn := strings.Repeat("x", msgChars)
	head := turn
	if len(head) > 32 {
		head = head[:32]
	}
	var msgs []string
	msgs = append(msgs, fmt.Sprintf(`{"role":"system","content":"You are a coding agent working in %s."}`, head))
	for i := 1; i < nMessages-1; i++ {
		if i%2 == 1 {
			msgs = append(msgs, fmt.Sprintf(`{"role":"user","content":"turn %d: %s"}`, i, turn))
		} else {
			msgs = append(msgs, fmt.Sprintf(`{"role":"assistant","content":"turn %d reply: %s"}`, i, turn))
		}
	}
	msgs = append(msgs, `{"role":"assistant","content":null,"tool_calls":[{"id":"call_payload_0","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}},{"id":"call_payload_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"main.go\"}"}}]}`)
	// The tool results follow the calls (paired, the shape production
	// dispatches): without them the anthropic-format transform strips the
	// orphaned tool_use blocks — benchmarking a warn path, not the transform.
	msgs = append(msgs,
		`{"role":"tool","tool_call_id":"call_payload_0","content":"total 42"}`,
		`{"role":"tool","tool_call_id":"call_payload_1","content":"package main"}`)
	return []byte(`{"model":"gpt-6-luna","stream":true,"reasoning_effort":"medium","max_completion_tokens":16384,"temperature":0.7,` +
		`"tools":[{"type":"function","function":{"name":"bash","description":"Run a shell command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}},` +
		`{"type":"function","function":{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],` +
		`"tool_choice":"auto","messages":[` + strings.Join(msgs, ",") + `]}`)
}
