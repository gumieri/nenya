package stream

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nenya/internal/util"
)

// DefaultToolArgsHoldBytes is the assembler's per-call hold-back: the tail of
// the accumulated arguments that is withheld from observers until the call
// completes. Its floor is sized to the longest exact-substring token the
// stream guards match today (the canary marker is ~45 bytes). The hold does
// NOT make a stateless per-fragment matcher sound mid-stream — fragments are
// contiguous and non-overlapping, and the live tail is withheld until the
// call completes, which is enough only for match-at-completion observers or
// window-retaining observers (see the ToolArgsObserver contract). Across a
// cap-eviction boundary the same contiguity is preserved by carrying the
// withheld tail into the successor accumulator.
const DefaultToolArgsHoldBytes = 64

// DefaultToolArgsMaxBytes is the per-call accumulation cap, aligned with
// mcp.DefaultMaxArgBytes (the tool-argument guard's default): a tool call
// whose arguments exceed it is no longer fully observable and is evicted.
// When a tail remains the observed prefix was delivered and the final hold
// carried into a successor accumulator that completes the key; a call
// evicted with an empty accumulator delivers nothing and never completes.
const DefaultToolArgsMaxBytes = 1 << 20

// DefaultToolArgsTotalBytes bounds the assembler's total accumulated bytes
// across all open calls of one stream, so a hostile upstream cannot pin
// unbounded memory. The bound is enforced on EVERY append (a call whose
// growth would breach it is evicted like an over-cap call), not only on new
// calls, and the constructor clamps the per-call cap to it — an operator-set
// max_arg_bytes above the stream bound can never punch through.
const DefaultToolArgsTotalBytes = 8 << 20

// DefaultToolArgsMaxCalls bounds the number of concurrently open calls. A
// name-only fragment opens a call that pins no argument bytes, so the byte
// bounds alone cannot stop map growth; new calls beyond the bound are
// dropped (saturated) until every open call completes (or Flush resets the
// bounds).
const DefaultToolArgsMaxCalls = 256

// ToolArgsObserver consumes reassembled tool-call argument content. Fragments
// for one call are delivered in order and never overlap, and their
// concatenation equals the full arguments string except that the one fragment
// which breached a per-call or stream cap is never delivered (evictions are
// visible only via OverCapCalls — offsets after an eviction stay contiguous
// in delivery order but are shifted relative to the wire string);
// ToolArgsComplete fires when the key ends and every held byte has been
// delivered by the preceding fragment calls.
//
// Contract notes for guard phases (A020+):
//
//   - Fragments are plain string COPIES delivered in order, without explicit
//     offsets. An observer that needs absolute offsets accumulates fragment
//     lengths itself; offsets continue across a cap-eviction boundary (the
//     successor accumulator carries the withheld tail) until ToolArgsComplete.
//   - Matching contract: fragments for a key are contiguous and
//     non-overlapping, and the live tail is withheld until the key
//     completes — so an observer matches a token either at completion
//     (every byte of the arguments has then been delivered across its
//     fragments) or by retaining a window of up to the assembler's hold
//     bytes across fragment calls, exactly the sliding-window mechanism the
//     existing stream guards use. A stateless per-fragment matcher misses a
//     token that straddles two fragment boundaries mid-stream.
//   - ToolArgsComplete does not fire for keys that ended before anything
//     was observed of them: a call dropped while saturated, or evicted with
//     no delivered prefix and no carried tail, never completes. Observers
//     holding per-key state must tolerate never-completed keys (bounded by
//     the distinct keys actually observed).
//   - Phase 010 is OBSERVATION-ONLY: the wire has already carried every byte
//     by the time an observer sees it, and a call's held tail surfaces only
//     at its completion. This surface supports detection, metrics and
//     tripwires — not retroactive stripping. An enforcing guard registers in
//     Phase 020 with chunk-mutation or mid-stream termination semantics.
type ToolArgsObserver interface {
	// ToolArgsFragment receives the next safe-to-emit slice of one tool
	// call's accumulated arguments. key identifies the call across the
	// stream's wire shape; name is the call's function name when known.
	ToolArgsFragment(key, name, fragment string)
	// ToolArgsComplete fires when the key ends — explicit wire completion,
	// terminal frame, or stream end (the EOF backstop and the reader's
	// early-termination funnels included). Every held byte has been
	// delivered by the preceding fragment calls for that key.
	ToolArgsComplete(key, name string)
}

// toolArgsCall is one tool call's accumulated argument stream.
type toolArgsCall struct {
	name    string
	args    []byte
	emitted int
}

// ToolCallAssembler reassembles streamed tool-call names and argument
// fragments across SSE events (NENYA-344, Module A Phase 010). The
// output-side guards visit model text only — `exfilContentKeys` and
// `forEachFilterableTextPart` never see `delta.tool_calls[].function.arguments`
// fragments, `input_json_delta.partial_json`, or Responses argument deltas —
// so a marker or URL split across (or delivered entirely inside) argument
// deltas bypasses every guard today. The assembler presents that surface:
// it accumulates each call's arguments across the raw provider-native chunk
// shapes and drives an observer with hold-bounded, non-overlapping slices
// plus a completion signal.
//
// Wire shapes fed (the raw chunk, before any provider transformer):
//
//   - OpenAI chat: choices[].delta.tool_calls[].function.{name,arguments}
//     keyed by (choice index, call index) — call index with the array
//     position fallback, as normalizeToolCalls.
//   - Anthropic: content_block_start.tool_use (name), input_json_delta
//     partial_json fragments and content_block_stop, keyed by block index.
//   - Responses: response.output_item.added function_call items
//     (name), response.function_call_arguments.delta fragments and
//     output_item.done completions, keyed by item_id (output_index fallback).
//   - Gemini: whole-part candidates[].content.parts[].functionCall — the
//     args object arrives complete and is emitted and completed at once.
//     Bare-JSON (non-`data:`) lines carry the same shapes and are fed too.
//
// The assembler observes only: it never mutates the parsed chunk, and the
// client-visible wire bytes are untouched. It is used from the single
// goroutine calling Read(); no mutex is needed. One assembler per reader:
// failover builds a fresh reader, so no rearm path exists.
type ToolCallAssembler struct {
	hold        int
	maxArgBytes int
	maxTotal    int
	maxCalls    int
	total       int
	observer    ToolArgsObserver
	calls       map[string]*toolArgsCall
	overCap     int
	saturated   bool

	// blockedErr, set by the Phase 020 enforcing guard observer, carries
	// the first guard verdict that must terminate the stream (canary or
	// egress policy); the reader checks Blocked() after every feed and
	// reports BlockedError() through the existing blocked machinery.
	blockedErr error
	// flushHook, when set, runs at the end of every Flush — the guard
	// observer drains its guards' held tails there so a stream-end tail
	// scan can still raise a verdict.
	flushHook func()
}

// Blocked reports whether an enforcing guard observer has raised a
// stream-terminating verdict on the reassembled arguments.
func (a *ToolCallAssembler) Blocked() bool {
	return a != nil && a.blockedErr != nil
}

// BlockedError returns the verdict raised by the enforcing guard observer
// (nil when not blocked).
func (a *ToolCallAssembler) BlockedError() error {
	if a == nil {
		return nil
	}
	return a.blockedErr
}

// setBlocked records the first guard verdict; later verdicts cannot
// displace it.
func (a *ToolCallAssembler) setBlocked(err error) {
	if a == nil || a.blockedErr != nil || err == nil {
		return
	}
	a.blockedErr = err
}

// SetFlushHook registers a callback that runs at the end of every Flush —
// the enforcing guard observer drains its guards' held tails there so a
// stream-end tail scan can still raise a verdict (visible via Blocked()
// immediately after the flush).
func (a *ToolCallAssembler) SetFlushHook(fn func()) {
	if a == nil {
		return
	}
	a.flushHook = fn
}

// NewToolCallAssembler creates an assembler. holdBytes is a security floor,
// not a tuning knob: a non-positive value coerces to the package default and
// hold>=1 is mandatory through this constructor — the hold is what withholds
// the live tail until completion (see DefaultToolArgsHoldBytes for the
// matching contract it does and does not provide). maxArgBytes, maxTotalBytes
// and maxCalls fall back to the package defaults when non-positive, and
// maxArgBytes is clamped to maxTotalBytes so no single call can exceed the
// stream bound.
func NewToolCallAssembler(holdBytes, maxArgBytes, maxTotalBytes, maxCalls int) *ToolCallAssembler {
	if holdBytes <= 0 {
		holdBytes = DefaultToolArgsHoldBytes
	}
	if maxTotalBytes <= 0 {
		maxTotalBytes = DefaultToolArgsTotalBytes
	}
	if maxArgBytes <= 0 || maxArgBytes > maxTotalBytes {
		maxArgBytes = DefaultToolArgsMaxBytes
		if maxArgBytes > maxTotalBytes {
			maxArgBytes = maxTotalBytes
		}
	}
	if maxCalls <= 0 {
		maxCalls = DefaultToolArgsMaxCalls
	}
	return &ToolCallAssembler{
		hold:        holdBytes,
		maxArgBytes: maxArgBytes,
		maxTotal:    maxTotalBytes,
		maxCalls:    maxCalls,
		calls:       make(map[string]*toolArgsCall),
	}
}

// SetObserver registers the consumer of reassembled fragments. Called from
// the reader setup, before the stream is read.
func (a *ToolCallAssembler) SetObserver(obs ToolArgsObserver) {
	a.observer = obs
}

// OverCapCalls reports how many calls exceeded the per-call or stream-total
// cap and were evicted. When a tail remains, the observed prefix was
// delivered and the final hold bytes were carried into the successor
// accumulator; a call evicted with an empty accumulator delivered nothing
// and never completes (see the ToolArgsObserver contract).
func (a *ToolCallAssembler) OverCapCalls() int {
	if a == nil {
		return 0
	}
	return a.overCap
}

// Saturated reports whether a bound was hit such that further calls cannot
// be observed — the total byte bound or the open-call bound dropped a NEW
// call. Fragments for calls opened after that point are dropped until every
// open call completes (or Flush resets the bounds).
func (a *ToolCallAssembler) Saturated() bool {
	if a == nil {
		return false
	}
	return a.saturated
}

// FeedParsed extracts tool-call identity and argument fragments from one
// RAW provider-native chunk. Observation only: the chunk is never mutated.
// Terminal frames of the wire shapes complete their open calls.
func (a *ToolCallAssembler) FeedParsed(parsed map[string]interface{}) {
	if a == nil || parsed == nil {
		return
	}
	a.feedOpenAI(parsed)
	a.feedAnthropic(parsed)
	a.feedResponses(parsed)
	a.feedGemini(parsed)
	a.flushIfTerminal(parsed)
}

// Flush completes every open call, delivering held remainders and completion
// signals in deterministic key order. Called at the stream's terminal (and
// in the reader's early-termination funnels) as the backstop; a flush with
// no open calls is a no-op and is idempotent.
func (a *ToolCallAssembler) Flush() {
	if a == nil {
		return
	}
	if len(a.calls) > 0 {
		keys := make([]string, 0, len(a.calls))
		for k := range a.calls {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			a.complete(k)
		}
		a.total = 0
		a.saturated = false
	}
	// The hook runs on EVERY flush — including no-ops: a cleanly completed
	// call empties the map before the enforcing guard's withheld tail is
	// drained, and skipping the hook there would leave that tail
	// un-EOF-evaluated at stream end. Guard drains are idempotent.
	if a.flushHook != nil {
		a.flushHook()
	}
}

func (a *ToolCallAssembler) open(key, name string) *toolArgsCall {
	if c, ok := a.calls[key]; ok {
		if name != "" {
			c.name = name
		}
		return c
	}
	if len(a.calls) >= a.maxCalls {
		// Open-call bound: a name-only fragment pins no argument bytes,
		// so the byte bounds cannot stop map growth. Drop the new call
		// (it was never observable: no fragments, no completion) until
		// the next completion (or Flush) frees a slot.
		a.saturated = true
		return nil
	}
	if a.total >= a.maxTotal {
		// Total bound hit: a new call cannot be observed without breaking
		// the bound. Drop its fragments (completion signals for it are
		// never emitted either — it was never observable) until the next
		// completion (or Flush) resets the budget.
		a.saturated = true
		return nil
	}
	c := &toolArgsCall{name: name}
	a.calls[key] = c
	return c
}

func (a *ToolCallAssembler) append(key, name, frag string) {
	if frag == "" {
		if name != "" {
			a.open(key, name)
		}
		return
	}
	c := a.open(key, name)
	if c == nil {
		return
	}
	if len(c.args)+len(frag) > a.maxArgBytes || a.total+len(frag) > a.maxTotal {
		a.evictWithCarryOver(key, c)
		return
	}
	c.args = append(c.args, frag...)
	a.total += len(frag)
	a.emitSafe(key, c)
}

// evictWithCarryOver handles a per-call or stream-total cap breach: the rest
// of this call is no longer fully observable. Withhold the final hold bytes
// and seed the successor accumulator with them, keeping the observed
// fragment stream for the key contiguous across the boundary (the same
// sliding-window obligation an observer already carries mid-stream); the
// key's completion now fires when the successor ends (or at Flush).
// Evicting releases the prefix's budget — a giant argument must not pin it
// for the stream's lifetime.
func (a *ToolCallAssembler) evictWithCarryOver(key string, c *toolArgsCall) {
	keep := a.hold
	if keep > len(c.args) {
		keep = len(c.args)
	}
	boundary := len(c.args) - keep
	for boundary > c.emitted && !utf8.RuneStart(c.args[boundary]) {
		boundary--
	}
	if boundary < c.emitted {
		boundary = c.emitted
	}
	a.emitUpTo(key, c, boundary)
	tail := string(c.args[boundary:])
	name := c.name
	a.total -= len(c.args)
	a.overCap++
	delete(a.calls, key)
	if tail == "" {
		return
	}
	seed := a.open(key, name)
	if seed == nil {
		// Unreachable while the invariants hold — the slot was just
		// freed (the evicted call was deleted before open) and a
		// non-empty tail implies total < maxTotal — kept as
		// defense-in-depth so a future accounting change fails closed.
		return
	}
	seed.args = append(seed.args, tail...)
	a.total += len(tail)
}

// emitSafe delivers the accumulated bytes that are more than hold bytes away
// from the live tail, backing the boundary up to a rune start so no fragment
// splits a multi-byte rune.
func (a *ToolCallAssembler) emitSafe(key string, c *toolArgsCall) {
	safe := len(c.args) - a.hold
	if safe <= c.emitted {
		return
	}
	for safe > c.emitted && !utf8.RuneStart(c.args[safe]) {
		safe--
	}
	if safe <= c.emitted {
		return
	}
	a.emitUpTo(key, c, safe)
}

// emitUpTo delivers accumulated bytes below the boundary (the caller keeps
// the boundary rune-aligned) and records the delivery position. A nil
// observer makes this pure bookkeeping.
func (a *ToolCallAssembler) emitUpTo(key string, c *toolArgsCall, boundary int) {
	if a.observer != nil && c.emitted < boundary {
		a.observer.ToolArgsFragment(key, c.name, string(c.args[c.emitted:boundary]))
	}
	c.emitted = boundary
}

// emitAll delivers every held byte (completion path).
func (a *ToolCallAssembler) emitAll(key string, c *toolArgsCall) {
	a.emitUpTo(key, c, len(c.args))
}

func (a *ToolCallAssembler) complete(key string) {
	c, ok := a.calls[key]
	if !ok {
		return
	}
	a.emitAll(key, c)
	if a.observer != nil {
		a.observer.ToolArgsComplete(key, c.name)
	}
	a.total -= len(c.args)
	delete(a.calls, key)
	if len(a.calls) == 0 {
		// With no open calls left, no drop can still be in effect: the
		// bounds re-evaluate on every new open, and a stale saturation
		// flag would only misreport telemetry.
		a.saturated = false
		// All observed calls have ended — the arguments are final, so the
		// enforcing guard's withheld tail can be EOF-evaluated now (the
		// hook is the Phase 020 guard drain; idempotent). Draining only at
		// map-empty keeps the tail pending while other calls still stream,
		// so a still-growing construct is not falsely EOF-terminated early.
		if a.flushHook != nil {
			a.flushHook()
		}
	}
}

// completeChoice completes every open call of one OpenAI choice: its
// finish_reason ends that choice's message only, so an n>1 stream keeps the
// other choices accumulating.
func (a *ToolCallAssembler) completeChoice(prefix string) {
	keys := make([]string, 0, 2)
	for k := range a.calls {
		if strings.HasPrefix(k, prefix+"/") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		a.complete(k)
	}
}

// flushIfTerminal completes open calls on the wire shapes' terminal frames:
// an OpenAI finish_reason ends its own choice's message, the Anthropic
// message_stop and the Responses terminal events (completed/failed/
// incomplete, the NENYA-337 taxonomy) end the whole response. The EOF
// backstop lives in the reader's scanner-done and error funnels.
func (a *ToolCallAssembler) flushIfTerminal(parsed map[string]interface{}) {
	if typ, _ := parsed["type"].(string); typ == "message_stop" ||
		typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete" {
		a.Flush()
		return
	}
	choices, ok := parsed["choices"].([]interface{})
	if !ok {
		return
	}
	for pos, c := range choices {
		choice, _ := c.(map[string]interface{})
		if choice == nil {
			continue
		}
		if fr, has := choice["finish_reason"]; has && fr != nil {
			idx := pos
			if raw, has := choice["index"]; has {
				idx = ToInt(raw)
			}
			a.completeChoice("openai/" + strconv.Itoa(idx))
		}
	}
}

func (a *ToolCallAssembler) feedOpenAI(parsed map[string]interface{}) {
	choices, ok := parsed["choices"].([]interface{})
	if !ok {
		return
	}
	for pos, c := range choices {
		choice, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		delta, ok := choice["delta"].(map[string]interface{})
		if !ok {
			continue
		}
		tcs, ok := delta["tool_calls"].([]interface{})
		if !ok {
			continue
		}
		choiceIdx := pos
		if raw, has := choice["index"]; has {
			choiceIdx = ToInt(raw)
		}
		for tcPos, tcRaw := range tcs {
			tc, ok := tcRaw.(map[string]interface{})
			if !ok {
				continue
			}
			idx := tcPos
			if rawIdx, has := tc["index"]; has {
				idx = util.ParseToolCallIndex(rawIdx, tcPos)
			}
			fn, _ := tc["function"].(map[string]interface{})
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			a.append("openai/"+strconv.Itoa(choiceIdx)+"/"+strconv.Itoa(idx), name, args)
		}
	}
}

func (a *ToolCallAssembler) feedAnthropic(parsed map[string]interface{}) {
	typ, _ := parsed["type"].(string)
	switch typ {
	case "content_block_start":
		cb, _ := parsed["content_block"].(map[string]interface{})
		if t, _ := cb["type"].(string); t == "tool_use" {
			name, _ := cb["name"].(string)
			a.open(anthropicKey(parsed), name)
		}
	case "content_block_delta":
		delta, _ := parsed["delta"].(map[string]interface{})
		if d, _ := delta["type"].(string); d == "input_json_delta" {
			pj, _ := delta["partial_json"].(string)
			a.append(anthropicKey(parsed), "", pj)
		}
	case "content_block_stop":
		a.complete(anthropicKey(parsed))
	}
}

// anthropicKey keys a block by its top-level stream index.
func anthropicKey(parsed map[string]interface{}) string {
	return "anthropic/" + strconv.Itoa(ToInt(parsed["index"]))
}

func (a *ToolCallAssembler) feedResponses(parsed map[string]interface{}) {
	typ, _ := parsed["type"].(string)
	switch typ {
	case "response.output_item.added":
		item, _ := parsed["item"].(map[string]interface{})
		if t, _ := item["type"].(string); t == "function_call" {
			name, _ := item["name"].(string)
			a.open(responsesKey(parsed, item), name)
		}
	case "response.function_call_arguments.delta":
		// Deltas with neither item_id nor output_index have no call to
		// attach to; dropping them (like the done path) fails closed
		// instead of merging fragments of distinct calls into one
		// anonymous accumulator.
		if key := responsesKey(parsed, nil); key != "" {
			delta, _ := parsed["delta"].(string)
			a.append(key, "", delta)
		}
	case "response.output_item.done":
		// The done event may carry the full item object or only item_id /
		// output_index; both must complete the call (a done for a
		// never-opened key is a harmless no-op).
		item, _ := parsed["item"].(map[string]interface{})
		if key := responsesKey(parsed, item); key != "" {
			a.complete(key)
		}
	}
}

// responsesKey keys a Responses call by its item id, falling back to the
// output_index both the added and delta events carry.
func responsesKey(parsed map[string]interface{}, item map[string]interface{}) string {
	if id, _ := parsed["item_id"].(string); id != "" {
		return "responses/" + id
	}
	if item != nil {
		if id, _ := item["id"].(string); id != "" {
			return "responses/" + id
		}
	}
	if raw, has := parsed["output_index"]; has {
		return "responses/idx/" + strconv.Itoa(ToInt(raw))
	}
	return ""
}

func (a *ToolCallAssembler) feedGemini(parsed map[string]interface{}) {
	candidates, ok := parsed["candidates"].([]interface{})
	if !ok {
		return
	}
	for ci, c := range candidates {
		cand, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		content, ok := cand["content"].(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]interface{})
		if !ok {
			continue
		}
		for pi, pr := range parts {
			part, ok := pr.(map[string]interface{})
			if !ok {
				continue
			}
			fc, ok := part["functionCall"].(map[string]interface{})
			if !ok {
				continue
			}
			// The Gemini functionCall part arrives whole (recorded
			// fixtures): marshal the args object, emit, and complete —
			// nothing more arrives for this call. A zero-arg functionCall
			// (no args field) observes as an empty object, not a JSON
			// null.
			name, _ := fc["name"].(string)
			key := "gemini/" + strconv.Itoa(ci) + "/" + strconv.Itoa(pi)
			args := fc["args"]
			if args == nil {
				args = map[string]interface{}{}
			}
			argsJSON, err := json.Marshal(args)
			if err != nil {
				argsJSON = []byte("{}")
			}
			a.append(key, name, string(argsJSON))
			a.complete(key)
		}
	}
}
