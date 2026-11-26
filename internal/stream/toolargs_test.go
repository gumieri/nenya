package stream

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// captureObserver records the assembler's deliveries in order so tests can
// assert fragment ordering, hold-back behavior, and completion signals.
type captureObserver struct {
	frags     []captureFrag
	completed []captureDone
}

type captureFrag struct {
	key      string
	name     string
	fragment string
}

type captureDone struct {
	key  string
	name string
}

func (c *captureObserver) ToolArgsFragment(key, name, fragment string) {
	c.frags = append(c.frags, captureFrag{key, name, fragment})
}

func (c *captureObserver) ToolArgsComplete(key, name string) {
	c.completed = append(c.completed, captureDone{key, name})
}

// concatFor joins one key's fragments in delivery order.
func (c *captureObserver) concatFor(key string) string {
	var b strings.Builder
	for _, f := range c.frags {
		if f.key == key {
			b.WriteString(f.fragment)
		}
	}
	return b.String()
}

// completedKeys reports the completion order.
func (c *captureObserver) completedKeys() []string {
	keys := make([]string, 0, len(c.completed))
	for _, d := range c.completed {
		keys = append(keys, d.key)
	}
	return keys
}

// feedChunk parses one SSE data line's JSON payload and feeds it. It fails
// the test on malformed JSON so a test bug cannot masquerade as assembler
// behavior.
func feedChunk(t testing.TB, a *ToolCallAssembler, line string) {
	t.Helper()
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("test chunk is not valid JSON: %v", err)
	}
	a.FeedParsed(parsed)
}

// jsonQuote marshals one string as a JSON string literal.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// openAIToolCall is one entry of a delta.tool_calls array.
type openAIToolCall struct {
	index int
	name  string
	args  string
}

// openAILine builds an OpenAI chat chunk (choice index 0) carrying the given
// tool-call entries. Continuation entries carry an empty name.
func openAILine(calls ...openAIToolCall) string {
	return openAIChoiceLine(0, calls...)
}

// openAIChoiceLine builds an OpenAI chat chunk for one choice.
func openAIChoiceLine(choiceIdx int, calls ...openAIToolCall) string {
	var b strings.Builder
	b.WriteString(`{"choices":[{"index":` + strconv.Itoa(choiceIdx) + `,"delta":{"tool_calls":[`)
	for i, tc := range calls {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"index":` + strconv.Itoa(tc.index) +
			`,"type":"function","function":{"name":` + jsonQuote(tc.name) +
			`,"arguments":` + jsonQuote(tc.args) + `}}`)
	}
	b.WriteString(`]}}]}`)
	return b.String()
}

// TestToolArgsAssemblerOpenAI pins the pin-first issue (NENYA-344): a marker
// delivered only inside streamed tool-call argument deltas — split across two
// chunks and held by the hold-back — is reassembled whole for the observer.
// Pre-fix, no output-side surface saw delta.tool_calls arguments at all.
func TestToolArgsAssemblerOpenAI(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, openAILine(openAIToolCall{0, "get_ticket", ""}))
	feedChunk(t, a, openAILine(openAIToolCall{0, "", `{"ticket_id": `}))
	feedChunk(t, a, openAILine(openAIToolCall{0, "", `"NENYA-CANARY-abc123"}`}))
	key := "openai/0/0"

	if got := obs.concatFor(key); got != "" {
		t.Fatalf("hold-back must withhold the live tail until completion, got %q", got)
	}
	feedChunk(t, a, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	if got := obs.concatFor(key); got != `{"ticket_id": "NENYA-CANARY-abc123"}` {
		t.Fatalf("reassembled args = %q", got)
	}
	if keys := obs.completedKeys(); len(keys) != 1 || keys[0] != key {
		t.Fatalf("completions = %v, want [%s]", keys, key)
	}
	if obs.completed[0].name != "get_ticket" {
		t.Fatalf("completed name = %q, want get_ticket", obs.completed[0].name)
	}
	if a.OverCapCalls() != 0 || a.Saturated() {
		t.Fatalf("assembler not clean after flush: overCap=%d saturated=%v", a.OverCapCalls(), a.Saturated())
	}
}

// TestToolArgsAssemblerParallelCalls covers two interleaved OpenAI calls
// keyed by (choice, call) index, with a marker split across deltas of the
// second call: fragments must stay per-call and both must complete.
func TestToolArgsAssemblerParallelCalls(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, openAILine(
		openAIToolCall{0, "first", `{"a":"NENYA-CANARY-first-aaaa"}`},
		openAIToolCall{1, "second", `{"b":"NENYA-CANARY-secon`},
	))
	feedChunk(t, a, openAILine(openAIToolCall{1, "", `d-aaaa"}`}))
	feedChunk(t, a, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)

	if got := obs.concatFor("openai/0/0"); got != `{"a":"NENYA-CANARY-first-aaaa"}` {
		t.Fatalf("call 0 args = %q", got)
	}
	if got := obs.concatFor("openai/0/1"); got != `{"b":"NENYA-CANARY-second-aaaa"}` {
		t.Fatalf("call 1 args = %q", got)
	}
	if len(obs.completed) != 2 {
		t.Fatalf("completions = %d, want 2", len(obs.completed))
	}
}

// TestToolArgsAssemblerChoiceIsolation pins the n>1 behavior: calls of
// different choices never merge, and one choice's finish_reason completes
// only its own calls — the other choice stays open until stream end.
func TestToolArgsAssemblerChoiceIsolation(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, `{"choices":[`+
		`{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"a","arguments":"{\"x\":\"NENYA-CANARY-cho\"}"}}]}},`+
		`{"index":1,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"b","arguments":"{\"y\":1}"}}]}}]}`)
	feedChunk(t, a, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)

	if keys := obs.completedKeys(); len(keys) != 1 || keys[0] != "openai/0/0" {
		t.Fatalf("choice-0 finish must complete only openai/0/0, got %v", keys)
	}
	a.Flush()
	if keys := obs.completedKeys(); len(keys) != 2 || keys[1] != "openai/1/0" {
		t.Fatalf("choice-1 call must complete at flush, got %v", keys)
	}
	if got := obs.concatFor("openai/1/0"); got != `{"y":1}` {
		t.Fatalf("choice-1 args = %q", got)
	}
}

// TestToolArgsAssemblerAnthropic pins the Anthropic shape: the name arrives on
// content_block_start.tool_use, arguments accumulate from input_json_delta
// partial_json fragments keyed by block index, and content_block_stop
// completes the call. Text blocks must produce nothing.
func TestToolArgsAssemblerAnthropic(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, `{"type":"message_start","message":{"id":"m1"}}`)
	feedChunk(t, a, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	feedChunk(t, a, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"NENYA-CANARY-in-text"}}`)
	feedChunk(t, a, `{"type":"content_block_stop","index":0}`)
	feedChunk(t, a, `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file"}}`)
	feedChunk(t, a, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/etc/"}}`)
	feedChunk(t, a, `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"hosts\"}"}}`)
	feedChunk(t, a, `{"type":"content_block_stop","index":1}`)

	if got := obs.concatFor("anthropic/1"); got != `{"path":"/etc/hosts"}` {
		t.Fatalf("anthropic args = %q", got)
	}
	if got := obs.concatFor("anthropic/0"); got != "" {
		t.Fatalf("text block leaked into the assembler: %q", got)
	}
	if len(obs.completed) != 1 || obs.completed[0].key != "anthropic/1" {
		t.Fatalf("completions = %v, want [anthropic/1]", obs.completedKeys())
	}
	if obs.completed[0].name != "read_file" {
		t.Fatalf("completed name = %q, want read_file", obs.completed[0].name)
	}
}

// TestToolArgsAssemblerAnthropicMessageStopFlush covers the unterminated
// block: a provider that streams input_json_delta and closes the message
// without content_block_stop still delivers the held tail and completion.
func TestToolArgsAssemblerAnthropicMessageStopFlush(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"run"}}`)
	feedChunk(t, a, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`)
	if len(obs.completed) != 0 {
		t.Fatalf("completion before any terminal frame")
	}
	feedChunk(t, a, `{"type":"message_stop"}`)
	if got := obs.concatFor("anthropic/0"); got != `{"cmd":"ls"}` {
		t.Fatalf("flushed args = %q", got)
	}
	if len(obs.completed) != 1 {
		t.Fatalf("completions = %d, want 1", len(obs.completed))
	}
}

// TestToolArgsAssemblerGemini pins the Gemini shape: candidates[].content
// .parts[].functionCall arrives whole, so the marshaled args object is
// emitted and completed in one step. A zero-arg functionCall observes as an
// empty object, never a JSON null.
func TestToolArgsAssemblerGemini(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, `{"candidates":[{"content":{"parts":[`+
		`{"text":"thinking out loud"},`+
		`{"functionCall":{"name":"search","args":{"query":"NENYA-CANARY-gem"}}}]}}]}`)

	key := "gemini/0/1"
	if got := obs.concatFor(key); got != `{"query":"NENYA-CANARY-gem"}` {
		t.Fatalf("gemini args = %q", got)
	}
	if got := obs.concatFor("gemini/0/0"); got != "" {
		t.Fatalf("text part leaked into the assembler: %q", got)
	}
	if len(obs.completed) != 1 || obs.completed[0].name != "search" {
		t.Fatalf("completions = %+v, want one search completion", obs.completed)
	}

	feedChunk(t, a, `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"ping"}}]}}]}`)
	if got := obs.concatFor("gemini/0/0"); got != "{}" {
		t.Fatalf("zero-arg functionCall observed %q, want {}", got)
	}
	if len(obs.completed) != 2 {
		t.Fatalf("completions = %d, want 2", len(obs.completed))
	}
}

// TestToolArgsAssemblerResponses pins the Responses shape: function_call items
// keyed by item_id, argument fragments on response.function_call_arguments
// .delta, completion on output_item.done — plus the output_index fallback for
// shapes that omit ids, and fail-closed dropping of anonymous deltas that
// name no call.
func TestToolArgsAssemblerResponses(t *testing.T) {
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"get_ticket"}}`)
	feedChunk(t, a, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"ticket\":"}`)
	feedChunk(t, a, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"NENYA-CANARY-resp\"}"}`)
	if got := obs.concatFor("responses/fc_1"); got != "" {
		t.Fatalf("hold-back delivered early: %q", got)
	}
	feedChunk(t, a, `{"type":"response.output_item.done","item_id":"fc_1"}`)
	if got := obs.concatFor("responses/fc_1"); got != `{"ticket":"NENYA-CANARY-resp"}` {
		t.Fatalf("responses args = %q", got)
	}

	// An anonymous delta (neither item_id nor output_index) must be
	// dropped, not merged into a shared accumulator.
	feedChunk(t, a, `{"type":"response.function_call_arguments.delta","delta":"GHOST"}`)

	// Index fallback: an added event with no ids, deltas keyed by
	// output_index only, completed via response.completed.
	feedChunk(t, a, `{"type":"response.output_item.added","output_index":3,"item":{"type":"function_call","name":"by_index"}}`)
	feedChunk(t, a, `{"type":"response.function_call_arguments.delta","output_index":3,"delta":"{}"}`)

	// A done event carrying the full item object completes via the item id.
	feedChunk(t, a, `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_2","name":"with_item"}}`)
	feedChunk(t, a, `{"type":"response.function_call_arguments.delta","item_id":"fc_2","delta":"[1]"}`)
	feedChunk(t, a, `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_2","name":"with_item","arguments":"[1]"}}`)

	feedChunk(t, a, `{"type":"response.completed","response":{"id":"r1"}}`)

	if got := obs.concatFor("responses/idx/3"); got != "{}" {
		t.Fatalf("fallback-keyed args = %q", got)
	}
	if got := obs.concatFor("responses/fc_2"); got != "[1]" {
		t.Fatalf("item-object-done args = %q", got)
	}
	if got := obs.concatFor(""); got != "" {
		t.Fatalf("anonymous delta was not dropped: %q", got)
	}
	if keys := obs.completedKeys(); len(keys) != 3 || keys[0] != "responses/fc_1" || keys[1] != "responses/fc_2" || keys[2] != "responses/idx/3" {
		t.Fatalf("completions = %v", keys)
	}
}

// TestToolArgsAssemblerResponsesFailedTerminal pins the failed/incomplete
// terminal events (the NENYA-337 taxonomy) flushing open calls instead of
// delaying them to EOF.
func TestToolArgsAssemblerResponsesFailedTerminal(t *testing.T) {
	for _, typ := range []string{"response.failed", "response.incomplete"} {
		t.Run(typ, func(t *testing.T) {
			a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
			obs := &captureObserver{}
			a.SetObserver(obs)
			feedChunk(t, a, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_9","name":"n"}}`)
			feedChunk(t, a, `{"type":"response.function_call_arguments.delta","item_id":"fc_9","delta":"{\"x\":1}"}`)
			frame := `{"type":"` + typ + `","response":{"id":"r1"}}`
			feedChunk(t, a, frame)
			if len(obs.completed) != 1 || obs.completed[0].key != "responses/fc_9" {
				t.Fatalf("%s did not flush: %v", typ, obs.completedKeys())
			}
			if got := obs.concatFor("responses/fc_9"); got != `{"x":1}` {
				t.Fatalf("flushed args = %q", got)
			}
		})
	}
}

// TestToolArgsAssemblerHoldBackAndRuneBoundary pins the hold semantics: bytes
// within hold of the live tail are never emitted until completion, the
// boundary backs up to a rune start so no fragment splits a multi-byte rune,
// and fragments never overlap.
func TestToolArgsAssemblerHoldBackAndRuneBoundary(t *testing.T) {
	a := NewToolCallAssembler(2, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	// 4 bytes, hold 2: safe boundary at 2.
	feedChunk(t, a, openAILine(openAIToolCall{0, "n", "abcd"}))
	if got := obs.concatFor("openai/0/0"); got != "ab" {
		t.Fatalf("hold=2 over 4 bytes: emitted %q, want \"ab\"", got)
	}
	// 10 bytes: safe boundary at 8.
	feedChunk(t, a, openAILine(openAIToolCall{0, "", "efghij"}))
	if got := obs.concatFor("openai/0/0"); got != "abcdefgh" {
		t.Fatalf("after second chunk: emitted %q, want \"abcdefgh\"", got)
	}
	// "é" is 2 bytes at offsets 10-11; the safe boundary lands mid-rune
	// (11) and must back up to the rune start (10).
	feedChunk(t, a, openAILine(openAIToolCall{0, "", "é!"}))
	if got := obs.concatFor("openai/0/0"); got != "abcdefghij" {
		t.Fatalf("rune boundary: emitted %q, want \"abcdefghij\"", got)
	}
	for _, f := range obs.frags {
		if !utf8.ValidString(f.fragment) {
			t.Fatalf("fragment split a rune: %q", f.fragment)
		}
	}
	a.Flush()
	if got := obs.concatFor("openai/0/0"); got != "abcdefghijé!" {
		t.Fatalf("after flush: %q, want full args", got)
	}
}

// TestToolArgsAssemblerOverCapEviction pins the per-call cap with hold
// carry-over: on breach the prefix up to the cap is delivered, the final
// hold bytes are withheld and seeded into the successor accumulator (the
// observed fragment stream stays contiguous across the boundary), the key's
// completion fires only when the successor ends, and the eviction is
// counted.
func TestToolArgsAssemblerOverCapEviction(t *testing.T) {
	a := NewToolCallAssembler(4, 8, 1024, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, openAILine(openAIToolCall{0, "big", "12345678"}))
	feedChunk(t, a, openAILine(openAIToolCall{0, "", "9012"}))
	if got := obs.concatFor("openai/0/0"); got != "1234" {
		t.Fatalf("breached call must deliver only the prefix up to the rune-aligned boundary: %q", got)
	}
	if a.OverCapCalls() != 1 {
		t.Fatalf("OverCapCalls = %d, want 1", a.OverCapCalls())
	}
	if len(obs.completed) != 0 {
		t.Fatalf("completion must wait for the successor, got %d", len(obs.completed))
	}
	// The successor carries the withheld hold bytes; its completion
	// delivers them plus everything after the breach.
	feedChunk(t, a, openAILine(openAIToolCall{0, "", "{}"}))
	feedChunk(t, a, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if got := obs.concatFor("openai/0/0"); got != "12345678{}" {
		t.Fatalf("carry-over lost bytes: %q", got)
	}
	if len(obs.completed) != 1 {
		t.Fatalf("completions = %d, want 1", len(obs.completed))
	}
}

// TestToolArgsAssemblerTotalBreachEvicts pins the stream-total bound being
// enforced on EVERY append, not only on new calls: a call whose growth would
// breach the bound is evicted exactly like an over-cap call.
func TestToolArgsAssemblerTotalBreachEvicts(t *testing.T) {
	a := NewToolCallAssembler(1, 100, 10, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, openAILine(openAIToolCall{0, "grower", "abcdef"}))
	feedChunk(t, a, openAILine(openAIToolCall{0, "", "ghijkl"}))
	if got := obs.concatFor("openai/0/0"); got != "abcde" {
		t.Fatalf("breached call must deliver only its rune-aligned prefix: %q", got)
	}
	if a.OverCapCalls() != 1 || a.Saturated() {
		t.Fatalf("overCap=%d saturated=%v, want eviction without saturation", a.OverCapCalls(), a.Saturated())
	}
	a.Flush()
	if got := obs.concatFor("openai/0/0"); got != "abcdef" {
		t.Fatalf("carried tail lost: %q", got)
	}
	if len(obs.completed) != 1 {
		t.Fatalf("completions = %d, want 1", len(obs.completed))
	}
}

// TestToolArgsAssemblerTotalSaturation pins the total bound on NEW calls:
// once hit, new calls are dropped (nothing observable, no completion) until
// Flush resets the budget.
func TestToolArgsAssemblerTotalSaturation(t *testing.T) {
	a := NewToolCallAssembler(1, 100, 10, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, openAILine(openAIToolCall{0, "a", "0123456789"}))
	feedChunk(t, a, openAILine(openAIToolCall{1, "b", "x"}))
	if !a.Saturated() {
		t.Fatal("Saturated = false after the total bound was hit")
	}
	if got := obs.concatFor("openai/0/1"); got != "" {
		t.Fatalf("saturated call delivered fragments: %q", got)
	}
	feedChunk(t, a, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if got := obs.concatFor("openai/0/0"); got != "0123456789" {
		t.Fatalf("saturation must not affect the open call: %q", got)
	}
	if len(obs.completed) != 1 || obs.completed[0].name != "a" {
		t.Fatalf("completions = %+v, want one for call a", obs.completed)
	}

	// Flush resets the budget: a new call is observed again.
	feedChunk(t, a, openAILine(openAIToolCall{2, "c", "y"}))
	if a.Saturated() {
		t.Fatal("saturation survived Flush")
	}
	a.Flush()
	if got := obs.concatFor("openai/0/2"); got != "y" {
		t.Fatalf("post-reset call lost: %q", got)
	}
	if len(obs.completed) != 2 {
		t.Fatalf("completions = %d, want 2", len(obs.completed))
	}
}

// TestToolArgsAssemblerMaxCallsBound pins the open-call bound: name-only
// fragments pin no bytes, so the map size needs its own bound; new calls
// beyond it are dropped until Flush.
func TestToolArgsAssemblerMaxCallsBound(t *testing.T) {
	a := NewToolCallAssembler(1, 100, 100, 2)
	obs := &captureObserver{}
	a.SetObserver(obs)

	feedChunk(t, a, openAILine(openAIToolCall{0, "a", "{\"k\":1}"}))
	feedChunk(t, a, openAILine(openAIToolCall{1, "b", "{\"k\":2}"}))
	feedChunk(t, a, openAILine(openAIToolCall{2, "c", "{\"k\":3}"}))
	if !a.Saturated() {
		t.Fatal("Saturated = false after the open-call bound was hit")
	}
	if got := obs.concatFor("openai/0/2"); got != "" {
		t.Fatalf("dropped call delivered fragments: %q", got)
	}
	a.Flush()
	if len(obs.completed) != 2 {
		t.Fatalf("completions = %d, want 2 (dropped call never completes)", len(obs.completed))
	}
	if a.Saturated() {
		t.Fatal("saturation survived Flush")
	}
}

// TestToolArgsAssemblerTerminalFlushes covers the terminal frames of each
// wire shape flushing every open call.
func TestToolArgsAssemblerTerminalFlushes(t *testing.T) {
	t.Run("openai finish_reason", func(t *testing.T) {
		a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
		obs := &captureObserver{}
		a.SetObserver(obs)
		feedChunk(t, a, openAILine(openAIToolCall{0, "n", "{}"}))
		feedChunk(t, a, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		if len(obs.completed) != 1 {
			t.Fatalf("finish_reason did not flush: %d completions", len(obs.completed))
		}
	})
	t.Run("responses completed", func(t *testing.T) {
		a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
		obs := &captureObserver{}
		a.SetObserver(obs)
		feedChunk(t, a, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","name":"n"}}`)
		feedChunk(t, a, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{}"}`)
		feedChunk(t, a, `{"type":"response.completed","response":{"id":"r1"}}`)
		if len(obs.completed) != 1 {
			t.Fatalf("response.completed did not flush: %d completions", len(obs.completed))
		}
	})
	t.Run("flush is idempotent", func(t *testing.T) {
		a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
		obs := &captureObserver{}
		a.SetObserver(obs)
		feedChunk(t, a, openAILine(openAIToolCall{0, "n", "{}"}))
		a.Flush()
		a.Flush()
		if len(obs.completed) != 1 {
			t.Fatalf("double flush completed twice: %d", len(obs.completed))
		}
	})
}

// TestToolArgsAssemblerObservesWithoutMutation pins the observation-only
// contract: a chunk fed to the assembler is byte-identical afterwards, and an
// assembler with no observer never panics.
func TestToolArgsAssemblerObservesWithoutMutation(t *testing.T) {
	lines := []string{
		openAILine(openAIToolCall{0, "n", `{"k":"NENYA-CANARY-m"}`}),
		`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"g","args":{"q":1}}}]}}]}`,
	}
	for _, line := range lines {
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("test chunk invalid: %v", err)
		}
		before, err := json.Marshal(parsed)
		if err != nil {
			t.Fatalf("marshal before: %v", err)
		}
		a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
		a.FeedParsed(parsed) // no observer set
		after, err := json.Marshal(parsed)
		if err != nil {
			t.Fatalf("marshal after: %v", err)
		}
		if string(before) != string(after) {
			t.Fatalf("FeedParsed mutated the chunk:\nbefore %s\nafter  %s", before, after)
		}
	}
}

// TestReaderToolArgsAssemblerWire is the recorded pin-first test (NENYA-344,
// Module A Phase 010): a marker delivered ONLY inside streamed tool-call
// argument deltas — split across two SSE events — is reassembled for the
// observer and delivered whole at completion, while the client-visible wire
// bytes stay byte-identical (observation only). Pre-fix, the output-side
// guard surfaces saw no tool-call argument bytes at all: the observation
// surface did not exist.
func TestReaderToolArgsAssemblerWire(t *testing.T) {
	marker := "NENYA-CANARY-wire123"
	lines := []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_ticket","arguments":""}}]}}]}`,
		`data: ` + openAILine(openAIToolCall{0, "", `{"ticket_id": `}),
		`data: ` + openAILine(openAIToolCall{0, "", jsonQuote(marker) + `}`}),
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)
	reader.SetToolArgsAssembler(a)

	out := drainReader(t, reader)
	for _, line := range lines {
		if !strings.Contains(out, line) {
			t.Fatalf("observation-only contract broken: output lost the wire bytes %q", line)
		}
	}
	if got := obs.concatFor("openai/0/0"); !strings.Contains(got, marker) {
		t.Fatalf("marker split across deltas not reassembled: %q", got)
	}
	if len(obs.completed) != 1 || obs.completed[0].name != "get_ticket" {
		t.Fatalf("completions = %+v, want one get_ticket", obs.completed)
	}
}

// TestReaderToolArgsAssemblerEOFBackstop covers the reader-level backstop: a
// stream that ends without any terminal frame still delivers the held tails
// and completions at scanner-done.
func TestReaderToolArgsAssemblerEOFBackstop(t *testing.T) {
	lines := []string{
		`data: ` + openAILine(openAIToolCall{0, "truncated", `{"cmd":"ls`}),
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)
	reader.SetToolArgsAssembler(a)

	drainReader(t, reader)
	if got := obs.concatFor("openai/0/0"); got != `{"cmd":"ls` {
		t.Fatalf("EOF backstop lost the held tail: %q", got)
	}
	if len(obs.completed) != 1 {
		t.Fatalf("EOF backstop delivered %d completions, want 1", len(obs.completed))
	}
}

// TestReaderToolArgsAssemblerBlockedFlush covers the early-termination
// funnel: a guard-blocked stream never runs the scanner-done path, but the
// assembler must still deliver held tails and completions through the
// finishWithError funnel.
func TestReaderToolArgsAssemblerBlockedFlush(t *testing.T) {
	lines := []string{
		`data: ` + openAILine(openAIToolCall{0, "blocked_later", `{"x":"`}),
		`data: {"choices":[{"index":0,"delta":{"content":"BLOCKME"}}]}`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)
	reader.SetToolArgsAssembler(a)
	reader.SetStreamFilter(NewStreamFilter(nil, []*regexp.Regexp{regexp.MustCompile("BLOCKME")}, "", 4096))

	var out strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		out.Write(buf[:n])
		if err != nil {
			break // the blocked stream ends with its policy error, not EOF
		}
	}
	if got := obs.concatFor("openai/0/0"); got != `{"x":"` {
		t.Fatalf("blocked-stream funnel lost the held tail: %q", got)
	}
	if len(obs.completed) != 1 {
		t.Fatalf("blocked-stream funnel delivered %d completions, want 1", len(obs.completed))
	}
}

// TestReaderBareJSONMalformedQuiet pins the bare-JSON observation surface
// and its quiet parse: tool-call arguments on non-`data:` lines are
// reassembled, a malformed non-data line emits NO data-line warning (it is
// noise, not a protocol violation), and the line itself still passes
// through.
func TestReaderBareJSONMalformedQuiet(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	lines := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"n","arguments":"{\"a\":1}"}}]}}]}`,
		`{"broken`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	src := strings.NewReader(strings.Join(lines, "\n\n") + "\n\n")
	reader := NewSSETransformingReader(src, nil, context.Background())
	reader.SetLogger(logger)
	a := NewToolCallAssembler(DefaultToolArgsHoldBytes, DefaultToolArgsMaxBytes, DefaultToolArgsTotalBytes, 0)
	obs := &captureObserver{}
	a.SetObserver(obs)
	reader.SetToolArgsAssembler(a)

	out := drainReader(t, reader)
	if strings.Contains(logBuf.String(), "malformed JSON in SSE data line") {
		t.Fatalf("bare-JSON parse failure must not warn as a data-line violation: %s", logBuf.String())
	}
	if got := obs.concatFor("openai/0/0"); got != `{"a":1}` {
		t.Fatalf("bare-JSON tool args not reassembled: %q", got)
	}
	if !strings.Contains(out, `{"broken`) {
		t.Fatalf("bare-JSON line passthrough broken")
	}
}

// nameForPos gives the first chunk a name so the completion carries one.
func nameForPos(pos int) string {
	if pos == 0 {
		return "f"
	}
	return ""
}

// FuzzToolArgsAssembly splits arbitrary text into deterministic chunks —
// adjusted so no chunk splits a multi-byte rune, since the JSON round-trip
// would otherwise mask the original bytes — feeds them through the OpenAI
// shape, and asserts the assembler's core invariant: fragments never overlap,
// follow in order, and their concatenation equals the full arguments string
// once the call completes.
func FuzzToolArgsAssembly(f *testing.F) {
	for _, seed := range []string{
		`{"cmd":"ls -la"}`,
		"NENYA-CANARY-0123456789abcdef",
		"héllo wörld — emoji 🎉 tail",
		"a",
		strings.Repeat("chunky ", 40),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 1<<16 || !utf8.ValidString(text) {
			t.Skip()
		}
		a := NewToolCallAssembler(9, 1<<20, 8<<20, 0)
		obs := &captureObserver{}
		a.SetObserver(obs)
		pos := 0
		for pos < len(text) {
			end := pos + int(text[pos])%7 + 1
			if end > len(text) {
				end = len(text)
			}
			for end < len(text) && !utf8.RuneStart(text[end]) {
				end++
			}
			feedChunk(t, a, openAILine(openAIToolCall{0, nameForPos(pos), text[pos:end]}))
			pos = end
		}
		a.Flush()
		if got := obs.concatFor("openai/0/0"); got != text {
			t.Fatalf("reassembled %q, want %q", got, text)
		}
		for _, frag := range obs.frags {
			if frag.fragment == "" {
				t.Fatalf("empty fragment delivered for key %s", frag.key)
			}
			if !utf8.ValidString(frag.fragment) {
				t.Fatalf("fragment split a rune: %q", frag.fragment)
			}
		}
		wantCompletions := 1
		if len(text) == 0 {
			wantCompletions = 0
		}
		if len(obs.completed) != wantCompletions {
			t.Fatalf("completions = %d, want %d", len(obs.completed), wantCompletions)
		}
	})
}
