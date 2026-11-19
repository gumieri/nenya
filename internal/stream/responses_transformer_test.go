package stream

// Pinning tests for the Responses-API SSE → OpenAI chat deltas transformer
// (NENYA-337, GitHub issue #154): when the gateway dispatches a chat client's
// request to an openai-responses target, the upstream Responses event stream
// must be converted back into the chat-completions chunk shape the client
// (and the reader's tool-call/usage machinery) understands.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// mustChunk asserts a chat chunk frame was emitted.
func mustChunk(t *testing.T, out []byte) map[string]interface{} {
	t.Helper()
	if len(out) == 0 {
		t.Fatal("transformer emitted nothing, want a chat chunk")
	}
	var chunk map[string]interface{}
	if err := json.Unmarshal(out, &chunk); err != nil {
		t.Fatalf("emitted frame is not JSON: %v (%s)", err, out)
	}
	return chunk
}

// consume runs one SSE line through the transformer: consumed state events
// (ErrEventConsumed, the state-capture contract) yield consumed=true;
// anything else must emit a frame.
func consume(t *testing.T, tr *ResponsesTransformer, ctx context.Context, line string) ([]byte, bool) {
	t.Helper()
	out, err := tr.TransformSSEChunk(ctx, []byte(line))
	if err == ErrEventConsumed {
		return nil, true
	}
	if err != nil {
		t.Fatalf("transform %s: unexpected error %v", line, err)
	}
	return out, false
}

func firstDelta(t *testing.T, chunk map[string]interface{}) map[string]interface{} {
	t.Helper()
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk has no choices: %v", chunk)
	}
	choice, _ := choices[0].(map[string]interface{})
	delta, _ := choice["delta"].(map[string]interface{})
	return delta
}

// TestResponsesTransformer_TextDeltas pins the text path: output_text deltas
// become chat content deltas; response.completed closes the stream with
// finish_reason stop and the usage remapped into the chat spelling (the
// reader's usage callback keys on exactly that spelling).
func TestResponsesTransformer_TextDeltas(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()

	// Response envelope + assistant message item: state-only, consumed.
	if _, consumed := consume(t, tr, ctx, `{"type":"response.created","response":{"id":"resp_1","model":"gpt-6-luna"}}`); !consumed {
		t.Fatal("response.created must be consumed (state capture)")
	}
	if _, consumed := consume(t, tr, ctx, `{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}`); !consumed {
		t.Fatal("message output_item.added must be consumed")
	}

	// Text deltas → content deltas.
	out, consumed := consume(t, tr, ctx, `{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hel"}`)
	if consumed {
		t.Fatal("output_text.delta must emit a chunk")
	}
	chunk := mustChunk(t, out)
	delta := firstDelta(t, chunk)
	if delta["content"] != "Hel" {
		t.Errorf("first content delta = %v, want Hel", delta["content"])
	}
	if delta["role"] != "assistant" {
		t.Errorf("first delta must carry delta.role: %v", delta)
	}

	out, _ = consume(t, tr, ctx, `{"type":"response.output_text.delta","item_id":"msg_1","delta":"lo"}`)
	if delta := firstDelta(t, mustChunk(t, out)); delta["content"] != "lo" {
		t.Errorf("second content delta = %v, want lo", delta["content"])
	}
	if delta := firstDelta(t, mustChunk(t, out)); delta["role"] != nil {
		t.Errorf("role must ride only the first delta, got delta %v", delta)
	}

	// Completion: finish_reason stop + usage in the chat spelling.
	completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":64},"output_tokens_details":{"reasoning_tokens":20}}}}`
	out, consumed = consume(t, tr, ctx, completed)
	if consumed {
		t.Fatal("response.completed must emit the finish chunk")
	}
	chunk = mustChunk(t, out)
	choices := chunk["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "stop" {
		t.Errorf("finish_reason = %v, want stop", choice["finish_reason"])
	}
	usage, ok := chunk["usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("completed chunk carries no usage: %v", chunk)
	}
	if usage["prompt_tokens"] != float64(100) || usage["completion_tokens"] != float64(50) {
		t.Errorf("usage = %v, want prompt 100 / completion 50", usage)
	}
	if details, _ := usage["prompt_tokens_details"].(map[string]interface{}); details == nil || details["cached_tokens"] != float64(64) {
		t.Errorf("prompt_tokens_details = %v, want cached_tokens 64", usage["prompt_tokens_details"])
	}
	if details, _ := usage["completion_tokens_details"].(map[string]interface{}); details == nil || details["reasoning_tokens"] != float64(20) {
		t.Errorf("completion_tokens_details = %v, want reasoning_tokens 20", usage["completion_tokens_details"])
	}
}

// TestResponsesTransformer_ToolCallStitching pins the tool-call path: a
// function_call output item streams its arguments via deltas; the first chat
// delta for the call carries id+name, subsequent deltas accumulate arguments,
// and completion's finish_reason is tool_calls.
func TestResponsesTransformer_ToolCallStitching(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()

	if _, consumed := consume(t, tr, ctx, `{"type":"response.created","response":{"id":"resp_2","model":"gpt-6-luna"}}`); !consumed {
		t.Fatal("response.created must be consumed")
	}
	added := `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_abc","name":"bash","arguments":""}}`
	out, _ := consume(t, tr, ctx, added)
	delta := firstDelta(t, mustChunk(t, out))
	tcs, ok := delta["tool_calls"].([]interface{})
	if !ok || len(tcs) != 1 {
		t.Fatalf("added-frame delta.tool_calls = %v, want one entry carrying id+name", delta["tool_calls"])
	}
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "call_abc" || tc["index"] != float64(0) {
		t.Errorf("added-frame tool_call = %v, want id call_abc index 0", tc)
	}

	out, _ = consume(t, tr, ctx, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"cm"}`)
	delta = firstDelta(t, mustChunk(t, out))
	tcs = delta["tool_calls"].([]interface{})
	tc = tcs[0].(map[string]interface{})
	fn := tc["function"].(map[string]interface{})
	if fn["arguments"] != `{"cm` {
		t.Errorf("first arguments delta = %v, want only the increment", fn["arguments"])
	}

	out, _ = consume(t, tr, ctx, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"d\":\"ls\"}"}`)
	delta = firstDelta(t, mustChunk(t, out))
	tc = delta["tool_calls"].([]interface{})[0].(map[string]interface{})
	fn = tc["function"].(map[string]interface{})
	if fn["arguments"] != `d":"ls"}` {
		t.Errorf("second arguments delta = %v, want only the increment", fn["arguments"])
	}

	completed := `{"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[{"type":"function_call","call_id":"call_abc","name":"bash","arguments":"{\"cmd\":\"ls\"}"}],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`
	out, _ = consume(t, tr, ctx, completed)
	chunk := mustChunk(t, out)
	choice := chunk["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", choice["finish_reason"])
	}
}

// TestResponsesTransformer_Failed pins the failure path: a response.failed
// event must surface the upstream error to the client, not terminate as a
// clean empty stream.
func TestResponsesTransformer_Failed(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()
	failed := `{"type":"response.failed","response":{"id":"resp_3","status":"failed","error":{"code":"server_error","message":"upstream exploded"}}}`
	out, _ := consume(t, tr, ctx, failed)
	if len(out) == 0 {
		t.Fatal("response.failed produced no client-facing frame")
	}
	if !strings.Contains(string(out), "upstream exploded") {
		t.Errorf("failed frame %s does not carry the upstream error message", out)
	}
}

// TestResponsesTransformer_Incomplete pins the incomplete path: a response
// that hit max_output_tokens maps finish_reason to length (never silently
// stop — the client must be able to distinguish truncation).
func TestResponsesTransformer_Incomplete(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()
	if _, consumed := consume(t, tr, ctx, `{"type":"response.created","response":{"id":"resp_4","model":"gpt-6-luna"}}`); !consumed {
		t.Fatal("response.created must be consumed")
	}
	incomplete := `{"type":"response.incomplete","response":{"id":"resp_4","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":9,"output_tokens":99,"total_tokens":108}}}`
	out, _ := consume(t, tr, ctx, incomplete)
	choice := mustChunk(t, out)["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length for max_output_tokens truncation", choice["finish_reason"])
	}
}

// TestResponsesStreamUsageReachesOnUsage pins the NENYA-337 usage pipeline
// end-to-end: the reader's usage callback must fire from the RAW surface of
// a Responses stream (the response.completed event's response.usage
// placement) with the cache decomposition from input_tokens_details —
// without it, the usage tracker, cost model, calibration loop, and
// cache-state tracker are blind on streaming responses targets.
func TestResponsesStreamUsageReachesOnUsage(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_tokens_details":{"cached_tokens":64},"output_tokens_details":{"reasoning_tokens":20}}}}`,
		``,
	}, "\n")

	reader := NewSSETransformingReader(strings.NewReader(wire), NewResponsesTransformer(), context.Background())
	var got UsageData
	fired := false
	reader.SetOnUsage(func(u UsageData) {
		fired = true
		got = u
	})
	out, err := io.Copy(&bytes.Buffer{}, reader)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if out == 0 {
		t.Fatal("no output")
	}
	if !fired {
		t.Fatal("usage callback never fired for a Responses stream — usage accounting is blind on this format")
	}
	if got.PromptTokens != 100 || got.CompletionTokens != 50 {
		t.Errorf("usage = prompt %d / completion %d, want 100/50", got.PromptTokens, got.CompletionTokens)
	}
	if got.CacheHitTokens != 64 {
		t.Errorf("cache hit = %d, want 64 (input_tokens_details.cached_tokens)", got.CacheHitTokens)
	}
}

// TestResponsesTransformer_UnknownResponseEventsConsumed pins the garbage
// guard: unknown response.* events (and the reasoning_summary_part family)
// must never forward raw to a chat client — they are noise in every
// downstream parser.
func TestResponsesTransformer_UnknownResponseEventsConsumed(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()
	for _, line := range []string{
		`{"type":"response.reasoning_summary_part.added","item_id":"rs_1","summary_index":0}`,
		`{"type":"response.output_text.annotation.added","annotation":{"type":"url_citation"}}`,
		`{"type":"response.web_search_call.searching","item_id":"ws_1"}`,
		`{"type":"response.future_unknown_event","payload":{}}`,
	} {
		out, consumed := consume(t, tr, ctx, line)
		if !consumed {
			t.Errorf("event %s must be consumed, got frame %s", line, out)
		}
	}
	// A non-response-prefixed unknown line keeps the passthrough contract.
	out, consumed := consume(t, tr, ctx, `{"type":"totally_foreign","x":1}`)
	if consumed {
		t.Fatal("non-response unknown events must pass through unchanged")
	}
	if !strings.Contains(string(out), "totally_foreign") {
		t.Errorf("passthrough lost the payload: %s", out)
	}
}

// TestResponsesErrorTerminalDrainsHeldFragment pins NENYA-205 on the error
// cell: a fragment the exfil guard withheld before the error frame must
// still reach the client at EOF (never withheld, whatever the ending).
func TestResponsesErrorTerminalDrainsHeldFragment(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`,
		``,
		// The trailing URL construct is withheld by the guard (it could
		// still grow into a longer URL) — NENYA-205's hold.
		`data: {"type":"response.output_text.delta","delta":"check https://evil.example.com/x?d="}`,
		``,
		`data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"x","message":"died"}}}`,
		``,
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), NewResponsesTransformer(), context.Background())
	reader.SetExfilGuard(NewExfilGuard(config.ExfilGuardConfig{Action: config.ExfilActionStrip}, nil))
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	// The withheld URL fragment must be flushed at EOF even though the
	// stream ended on the error cell (no injection happened there).
	if !strings.Contains(body, "https://evil.example.com/x?d=") {
		t.Fatalf("the withheld fragment must be drained at the error terminal (NENYA-205):\n%s", body)
	}
}

// TestResponsesErrorThenFinishKeepsCleanClose pins the third classification
// ordering: an error frame FOLLOWED by a finish_reason (a recovering
// aggregator that then completed) classifies as the finish cell — silent
// [DONE], never a gateway_error (the flags' clearing order decides; the
// finish arrived last, so the stream is a completion).
func TestResponsesErrorThenFinishKeepsCleanClose(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"error":{"message":"blip","type":"upstream_error"}}`,
		``,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`,
		``,
		// EOF without [DONE]: the finish cell composes it.
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	emptyFired := false
	reader.SetEmptyStreamCallback(func() { emptyFired = true })
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("a completion after a recovering error must close with the sentinel:\n%s", body)
	}
	if strings.Contains(body, "gateway_error") {
		t.Fatalf("the finish cell owns the ending — no injected error:\n%s", body)
	}
	if emptyFired {
		t.Fatal("content was streamed — not an empty stream")
	}
}

// TestResponsesReasoningDeltaAfterErrorClearsTerminal pins the flag's
// clearing contract on the reasoning branch: a reasoning delta arriving
// after an error frame means the error was not the last signal — the stream
// is recoverable/cuttable, never silently error-terminal. Passthrough
// surface (the ResponsesTransformer latches at its terminal event, so
// post-error frames cannot occur there by contract).
func TestResponsesReasoningDeltaAfterErrorClearsTerminal(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"started"}}]}`,
		``,
		`data: {"error":{"message":"blip","type":"upstream_error"}}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"reasoning_content":"thinking again"}}]}`,
		``,
		// cut: no [DONE], no finish_reason.
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	emptyFired := false
	reader.SetEmptyStreamCallback(func() { emptyFired = true })
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out.String(), "gateway_error") {
		t.Fatalf("a cut after post-error reasoning must inject the cut terminal (the error was not the last signal):\n%s", out.String())
	}
	if emptyFired {
		t.Fatal("content was streamed — not an empty stream")
	}
}

// TestResponsesTransformer_BareErrorSurfacesDiagnostic pins the bare
// `error` event shape: its TOP-LEVEL code/message/param are the real
// upstream diagnostic — the fabricated fallback must not mask them.
func TestResponsesTransformer_BareErrorSurfacesDiagnostic(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()
	out, _ := consume(t, tr, ctx, `{"type":"error","code":"rate_limit_exceeded","message":"too many requests","param":"tools"}`)
	frame := mustChunk(t, out)
	errObj, ok := frame["error"].(map[string]interface{})
	if !ok {
		t.Fatalf("frame carries no error object: %v", frame)
	}
	if errObj["message"] != "too many requests" || errObj["code"] != "rate_limit_exceeded" || errObj["param"] != "tools" {
		t.Errorf("error payload = %v, want the upstream's own diagnostics", errObj)
	}
}

// TestResponsesTransformer_FallbackIndexClosesToolCalls pins the unseen-item
// fallback: arguments deltas for an item whose added event was never seen
// allocate a fallback index AND mark the stream as tool-calling — the
// finish_reason must be tool_calls, never stop (stop would tell the client
// the tool call was complete prose).
func TestResponsesTransformer_FallbackIndexClosesToolCalls(t *testing.T) {
	tr := NewResponsesTransformer()
	ctx := context.Background()
	if _, consumed := consume(t, tr, ctx, `{"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`); !consumed {
		t.Fatal("response.created must be consumed")
	}
	// No output_item.added: the delta attaches mid-stream.
	out, _ := consume(t, tr, ctx, `{"type":"response.function_call_arguments.delta","item_id":"fc_late","delta":"{}"}`)
	delta := firstDelta(t, mustChunk(t, out))
	if delta["tool_calls"] == nil {
		t.Fatalf("fallback delta must emit tool_calls: %v", delta)
	}
	out, _ = consume(t, tr, ctx, `{"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	choice := mustChunk(t, out)["choices"].([]interface{})[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls (a tool call streamed)", choice["finish_reason"])
	}
}

// TestResponsesFailedStreamNoGatewayError pins the round-3 terminal-state
// fix: a stream that ends on the converted error frame must NOT get the EOF
// gateway_error injection (the error frame IS the client-facing terminal)
// and must NOT count as an empty-200 degradation — a failed response is not
// an empty one.
func TestResponsesFailedStreamNoGatewayError(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`,
		``,
		`data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"server_error","message":"exploded"}}}`,
		``,
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), NewResponsesTransformer(), context.Background())
	emptyFired := false
	reader.SetEmptyStreamCallback(func() { emptyFired = true })
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "exploded") {
		t.Fatalf("the error frame must reach the client, got:\n%s", body)
	}
	if strings.Contains(body, "gateway_error") || strings.Contains(body, "upstream stream ended") {
		t.Fatalf("a failed stream must not get an injected gateway_error:\n%s", body)
	}
	if emptyFired {
		t.Fatal("a failed stream must not count as an empty-200 degradation")
	}
}

// TestResponsesErrorFrameAfterContent pins the recovering-error semantics:
// content streamed before an error frame keeps sawContent set — the EOF
// path must not classify the stream as empty (the client already received
// the content plus the error frame).
func TestResponsesErrorFrameAfterContent(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"partial answer"}`,
		``,
		`data: {"type":"response.failed","response":{"id":"r1","status":"failed","error":{"code":"x","message":"died"}}}`,
		``,
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), NewResponsesTransformer(), context.Background())
	emptyFired := false
	reader.SetEmptyStreamCallback(func() { emptyFired = true })
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "partial answer") || !strings.Contains(body, "died") {
		t.Fatalf("content + error frame must both reach the client:\n%s", body)
	}
	if strings.Contains(body, "gateway_error") {
		t.Fatalf("the error frame terminates the stream; no second injected error:\n%s", body)
	}
	if emptyFired {
		t.Fatal("content was streamed — not an empty stream")
	}
}

// TestResponsesErrorThenContentCutFallsBackToCut pins the flag's
// "error was the LAST signal" semantics on the raw surface: an aggregator
// that emits an error frame and then keeps streaming content is a recovering
// stream — if it is then genuinely cut, the EOF matrix must classify the CUT
// (injected gateway_error), not the error cell (which would bless the
// truncation and let it into the response cache). The ResponsesTransformer
// itself latches at its terminal event (stateful-close contract), so this
// corner exercises the passthrough surface.
func TestResponsesErrorThenContentCutFallsBackToCut(t *testing.T) {
	wire := strings.Join([]string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"pre"}}]}`,
		``,
		`data: {"error":{"message":"blip","type":"upstream_error"}}`,
		``,
		`data: {"choices":[{"index":0,"delta":{"content":"recovered and streaming"}}]}`,
		``,
		// ... then the connection is cut: no [DONE], no finish_reason.
	}, "\n")
	reader := NewSSETransformingReader(strings.NewReader(wire), nil, context.Background())
	emptyFired := false
	reader.SetEmptyStreamCallback(func() { emptyFired = true })
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "recovered and streaming") {
		t.Fatalf("the post-error content must reach the client:\n%s", body)
	}
	if !strings.Contains(body, "gateway_error") {
		t.Fatalf("a cut after recovered content must inject the cut terminal (never cache a truncation):\n%s", body)
	}
	if emptyFired {
		t.Fatal("content was streamed — not an empty stream")
	}
}
