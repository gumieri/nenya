package stream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"strconv"

	"github.com/nenya/internal/util"
)

func (r *SSETransformingReader) applyStreamFilters(parsed map[string]interface{}) (map[string]interface{}, bool) {
	if parsed == nil {
		return parsed, false
	}
	mutated := false

	if r.streamFilter != nil && !r.streamFilter.IsBlocked() {
		var action FilterAction
		parsed, action = applyStreamFilter(parsed, r.streamFilter)
		if r.streamFilter.IsBlocked() {
			return nil, false
		}
		mutated = mutated || action == ActionRedact
	}
	if r.exfilGuard != nil && !r.exfilGuard.IsBlocked() {
		out, changed, blocked := r.applyExfilStage(parsed)
		if blocked {
			return nil, false
		}
		parsed = out
		mutated = mutated || changed
	}

	if r.canaryWatcher != nil && !r.canaryWatcher.IsBlocked() {
		parsed = applyCanaryWatcher(parsed, r.canaryWatcher)
		if r.canaryWatcher.IsBlocked() {
			return nil, false
		}
	}

	if r.streamEntropyFilter != nil {
		var changed bool
		parsed, changed = applyEntropyFilter(parsed, r.streamEntropyFilter)
		mutated = mutated || changed
	}

	return parsed, mutated
}

// applyStreamFilter runs the secret/policy filter over one SSE chunk,
// reporting the resulting action so callers can detect rewrites. Every
// model-text surface is scanned (AUD-04): the OpenAI/Anthropic delta fields
// and each Gemini-native text part — raw upstream chunks are scanned before
// the provider transformer converts them.
func applyStreamFilter(parsed map[string]interface{}, filter *StreamFilter) (map[string]interface{}, FilterAction) {
	action := ActionPass
	blocked, _ := forEachFilterableTextPart(parsed, func(content string) (string, FilterAction) {
		redacted, a, _ := filter.FilterContent(content)
		if a == ActionBlock {
			return content, ActionBlock
		}
		if a == ActionRedact {
			action = ActionRedact
		}
		return redacted, a
	})
	if blocked {
		return nil, ActionBlock
	}
	return parsed, action
}

// anyFilterBlocked reports whether a stream-side filter has terminated
// the stream.
func (r *SSETransformingReader) anyFilterBlocked() bool {
	return (r.streamFilter != nil && r.streamFilter.IsBlocked()) ||
		(r.exfilGuard != nil && r.exfilGuard.IsBlocked()) ||
		(r.canaryWatcher != nil && r.canaryWatcher.IsBlocked())
}

// blockedStreamError returns the specific error for whichever filter
// terminated the stream (canary tripwire takes precedence, then the
// exfil guard; all wrap as stream-block errors for generic handling).
func (r *SSETransformingReader) blockedStreamError() error {
	if r.canaryWatcher != nil && r.canaryWatcher.IsBlocked() {
		return ErrCanaryDetected
	}
	if r.exfilGuard != nil && r.exfilGuard.IsBlocked() {
		return ErrExfilBlocked
	}
	return ErrStreamBlocked
}

// applyCanaryWatcher scans every model-text surface of one SSE chunk for
// the request canary (AUD-04: including Gemini-native parts). A block
// verdict nils the chunk so the reader raises ErrCanaryDetected.
func applyCanaryWatcher(parsed map[string]interface{}, watcher *CanaryWatcher) map[string]interface{} {
	blocked, _ := forEachFilterableTextPart(parsed, func(content string) (string, FilterAction) {
		if _, action, _ := watcher.FilterContent(content); action == ActionBlock {
			return content, ActionBlock
		}
		return content, ActionPass
	})
	if blocked {
		return nil
	}
	return parsed
}

// applyExfilStage runs the URL egress policy over one parsed chunk: it
// records the wire shape being evaluated (the terminal flush mimics it,
// NENYA-205), applies the guard's client-visible return, and reports
// whether the guard blocked this chunk.
func (r *SSETransformingReader) applyExfilStage(parsed map[string]interface{}) (out map[string]interface{}, mutated, blocked bool) {
	if hasFilterableText(parsed) {
		r.lastSurface = exfilSurfaceOf(parsed)
	}
	out, _, mutated = applyExfilGuard(parsed, r.exfilGuard)
	blocked = r.exfilGuard.IsBlocked()
	return out, mutated, blocked
}

// applyExfilGuard runs the URL egress policy over every model-text surface
// of one SSE chunk (AUD-04). The guard's return is the client-visible
// content for each surface and is applied on EVERY action: stripped spans
// replace the text field, and a shortened emit (the guard withheld a
// whitespace-free tail, NENYA-205) must not stream the original — the
// withheld remainder is delivered at the terminal flush. A block verdict
// nils the chunk so the reader raises ErrExfilBlocked (the guard itself
// flips to blocked). The bool reports whether the chunk was rewritten
// (callers re-encode). The middle FilterAction is degenerate by
// construction (Pass/Redact only — a block comes back as a nil chunk with
// the guard flipped, observable via IsBlocked); the only caller ignores it.
func applyExfilGuard(parsed map[string]interface{}, guard *ExfilGuard) (map[string]interface{}, FilterAction, bool) {
	rewrote := false
	blocked, _ := forEachFilterableTextPart(parsed, func(content string) (string, FilterAction) {
		rewritten, action, _ := guard.FilterContent(content)
		if rewritten != content {
			rewrote = true
		}
		return rewritten, action
	})
	if blocked {
		return nil, ActionBlock, true
	}
	if rewrote {
		return parsed, ActionRedact, true
	}
	return parsed, ActionPass, false
}

// applyEntropyFilter runs the entropy redactor over one SSE chunk,
// reporting whether it rewrote anything. Every model-text surface is
// visited (AUD-04).
func applyEntropyFilter(parsed map[string]interface{}, filter *StreamEntropyFilter) (map[string]interface{}, bool) {
	rewrote := false
	forEachFilterableTextPart(parsed, func(content string) (string, FilterAction) {
		redacted, a := filter.FilterContent(content)
		if a == ActionRedact && redacted != content {
			rewrote = true
			return redacted, a
		}
		return content, a
	})
	return parsed, rewrote
}

func (r *SSETransformingReader) callUsageAndContentCallbacks(parsed map[string]interface{}) {
	// Seed the response-model memory on EVERY parsed chunk (NENYA-173):
	// aggregators often report the model on content chunks only, so
	// reading it at the usage-emit point alone would miss the
	// substitution.
	_ = r.responseModel(parsed)
	if parsed == nil {
		return
	}
	if r.onUsage != nil {
		r.extractUsageFromMap(parsed)
	}
	if r.onContent != nil {
		if content := ExtractDeltaContentFromMap(parsed); content != "" {
			r.onContent(content)
		}
	}
	if r.onThinking != nil {
		if active, hasSignal := ExtractThinkingSignal(parsed); hasSignal {
			r.onThinking(active)
		}
	}
	// The TTFT clock stops at the first effective output frame, whatever the
	// wire format (NENYA-178); reasoning-only frames count only when the
	// operator opted in.
	if r.onFirstOutput != nil && HasEffectiveOutput(parsed, r.ttftIncludeReasoning) {
		r.fireFirstOutput()
	}
}

// fireFirstOutput fires the one-shot first-output callback (NENYA-178).
// Precondition: a hook is registered (the caller checks, so the per-frame cost
// of the classifier is paid only while a clock is running).
func (r *SSETransformingReader) fireFirstOutput() {
	cb := r.onFirstOutput
	r.onFirstOutput = nil
	if cb == nil {
		return
	}
	cb()
}

func (r *SSETransformingReader) handleNoTransformer(parsed map[string]interface{}, data []byte, origData []byte, line []byte) []byte {
	if parsed != nil && normalizeToolCalls(parsed, &r.tcState) {
		if out, err := json.Marshal(parsed); err == nil {
			data = out
		}
	}

	if bytes.Equal(data, origData) {
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	finalLine := append([]byte("data: "), data...)
	r.notifySSEObserver(finalLine, parsed, "")
	return finalLine
}

func (r *SSETransformingReader) handleWithTransformer(parsed map[string]interface{}, data []byte, origData []byte, line []byte) []byte {
	transformed, err := r.transformer.TransformSSEChunk(r.ctx, data)
	if err != nil {
		if errors.Is(err, ErrEventConsumed) {
			r.notifySSEObserver(line, parsed, "consumed")
			return nil
		}
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	if len(transformed) == 0 {
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	transformed = r.applyToolCallNormalization(transformed, &r.tcState)

	if IsDonePayload(transformed) {
		r.sawDone = true
		// Route through finishStreamFrames exactly like the raw-[DONE] path,
		// so a transformer-synthesized terminal composes the same frames
		// (held tail, done notification) instead of returning bare — the
		// direct return silently dropped the withheld tail at stream end
		// (AUD-02). On the Anthropic path the pre-terminal drain has usually
		// emptied the hold already; this branch is the backstop for
		// transformers that synthesize [DONE] without a raw terminal line.
		return r.finishStreamFrames([]byte(canonicalDoneFrame), []byte(canonicalDoneFrame))
	}

	if bytes.Equal(transformed, origData) && bytes.Equal(data, origData) {
		if parsed != nil {
			marshaled, err := json.Marshal(parsed)
			if err == nil && !bytes.Equal(marshaled, origData) {
				finalLine := append([]byte("data: "), marshaled...)
				r.notifySSEObserver(finalLine, parsed, "")
				return finalLine
			}
		}
		r.notifySSEObserver(line, parsed, "")
		return line
	}

	// AUD-12 (NENYA-220): a transformer may synthesize MULTIPLE events for
	// one upstream chunk (the OpenAI→Anthropic converter emits message_start
	// + content_block_* + message_delta/message_stop for a finish chunk).
	// Pre-fix every event was concatenated onto ONE `data:` line — an
	// invalid JSON payload strict clients reject. Frame each object as its
	// own event, with an `event:` line for the Anthropic type field.
	return r.frameTransformedEvents(transformed, parsed)
}

// frameTransformedEvents frames a transformer emission. Every JSON object
// with a type field becomes `event: <type>\ndata: <obj>\n\n` (strict
// Anthropic SDKs dispatch on event: lines and skip data-only frames — the
// single-object path must carry them too, AUD-12 review); typeless objects
// (OpenAI/Gemini re-marshals) keep the historical data-only shape so
// passthrough identity is preserved. A malformed multi-object payload is
// logged and framed from the objects decoded so far plus the raw remainder —
// never re-glued onto one invalid data line.
func (r *SSETransformingReader) frameTransformedEvents(transformed []byte, parsed map[string]interface{}) []byte {
	if len(transformed) == 0 || transformed[0] != '{' {
		finalLine := append([]byte("data: "), transformed...)
		r.notifySSEObserver(finalLine, parsed, "")
		return finalLine
	}
	// Hot path (NENYA-177 allocation discipline): a single valid JSON object
	// needs no decoder machinery — json.Valid rejects multi-object blobs in
	// one allocation-free pass. A TYPED object still gets its event: line
	// (strict Anthropic SDKs skip data-only frames), so a converted stream's
	// message_start and single-delta frames are dispatched correctly.
	if json.Valid(transformed) {
		return r.frameOneEvent(transformed, parsed)
	}

	objs, remainder, ok := splitConcatenatedJSON(transformed)
	if !ok {
		// Not decodable as concatenated JSON: the historical whole-blob
		// shape is the only safe framing.
		finalLine := append([]byte("data: "), transformed...)
		r.notifySSEObserver(finalLine, parsed, "")
		return finalLine
	}
	if remainder != nil {
		if r.logger != nil {
			r.logger.Warn("transformer emission had a trailing undecodable segment; framing degraded",
				"decoded", len(objs), "remainder_bytes", len(remainder))
		}
	}
	var b bytes.Buffer
	for i, obj := range objs {
		if t := sseEventTypeOf(obj); t != "" {
			b.WriteString("event: ")
			b.WriteString(t)
			b.WriteByte('\n')
		}
		b.WriteString("data: ")
		b.Write(obj)
		if i == len(objs)-1 && remainder == nil {
			// The LAST frame ends with a single newline: the upstream blank
			// separator line that follows this emission completes the event.
			// Emitting "\n\n" here plus that separator produced a spurious
			// empty SSE event after every converted chunk (AUD-12 review).
			b.WriteByte('\n')
			continue
		}
		b.WriteString("\n\n")
	}
	if remainder != nil {
		b.WriteString("data: ")
		b.Write(remainder)
		b.WriteString("\n\n")
	}
	out := b.Bytes()
	r.notifySSEObserver(out, parsed, "")
	return out
}

// frameOneEvent frames a single transformed object: an event: line when the
// object carries a well-formed type, the historical data-only line otherwise
// (OpenAI/Gemini re-marshals have no top-level type, so passthrough identity
// is preserved).
func (r *SSETransformingReader) frameOneEvent(obj []byte, parsed map[string]interface{}) []byte {
	if t := sseEventTypeOf(obj); t != "" {
		// Overflow-safe capacity (CWE-190, §7): the size hint is a hint
		// only, but unsized arithmetic on len()s is the flagged class —
		// AddCap clamps instead of wrapping to a negative make() size.
		out := make([]byte, 0, util.AddCap(util.AddCap(len(t), len(obj)), len("event: \ndata: \n\n")))
		out = append(out, "event: "...)
		out = append(out, t...)
		out = append(out, "\ndata: "...)
		out = append(out, obj...)
		out = append(out, "\n\n"...)
		r.notifySSEObserver(out, parsed, "")
		return out
	}
	finalLine := append([]byte("data: "), obj...)
	r.notifySSEObserver(finalLine, parsed, "")
	return finalLine
}

// splitConcatenatedJSON enumerates the top-level JSON values of a payload
// the transformer built by concatenating complete objects (raw bytes back to
// back). Returns the raw byte slice of each value; ok is false when the
// payload does not decode as concatenated JSON at all. A non-nil remainder
// is a trailing segment after the last decodable value (truncated write,
// stray noise) that could not be decoded.
func splitConcatenatedJSON(payload []byte) (objs [][]byte, remainder []byte, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if len(objs) == 0 {
				return nil, nil, false
			}
			return objs, bytes.TrimSpace(payload[dec.InputOffset():]), true
		}
		objs = append(objs, raw)
	}
	// dec.More() is false at valid input end AND at a premature `}`/`]` that
	// closes an outer level: any non-whitespace bytes left here are a trailing
	// segment (truncated write, stray bracket, logging noise) — surfaced as
	// remainder so the caller frames it raw instead of silently dropping it.
	if rest := bytes.TrimSpace(payload[dec.InputOffset():]); len(rest) > 0 && len(objs) > 0 {
		return objs, rest, true
	}
	if len(objs) == 0 {
		return nil, nil, false
	}
	return objs, nil, true
}

// sseEventTypeOf reads the Anthropic-style event type field of one framed
// object. Only a strict token charset is accepted: the value is written
// verbatim into an `event:` line, and a JSON-decoded newline in a
// provider-supplied type would forge extra SSE fields on the client's stream
// (defense in depth — no built-in transformer feeds upstream type values
// here today). "" means "no event: line".
func sseEventTypeOf(obj []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(obj, &probe); err != nil || probe.Type == "" {
		return ""
	}
	for _, c := range probe.Type {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-', c == '.':
		default:
			return ""
		}
	}
	return probe.Type
}

func (r *SSETransformingReader) applyToolCallNormalization(transformed []byte, state *toolCallState) []byte {
	if len(transformed) == 0 || transformed[0] != '{' {
		return transformed
	}
	var transformedParsed map[string]interface{}
	if json.Unmarshal(transformed, &transformedParsed) != nil {
		return transformed
	}
	if normalizeToolCalls(transformedParsed, state) {
		if out, err := json.Marshal(transformedParsed); err == nil {
			return out
		}
	}
	return transformed
}

// transformNonSSELine handles bare-JSON lines (the sseDataLine convention:
// some upstreams emit raw JSON bodies under text/event-stream). The
// private-event filter applies here too (NENYA-174).
func (r *SSETransformingReader) transformNonSSELine(line []byte) []byte {
	if r.dropPrivateEvents {
		if parsed := r.tryParseJSON(line); isPrivateSSEType(parsed) {
			return nil
		}
	}
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return line
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return line
	}
	if r.transformer == nil {
		return line
	}
	transformed, err := r.transformer.TransformSSEChunk(r.ctx, trimmed)
	if err != nil {
		if errors.Is(err, ErrEventConsumed) {
			r.notifySSEObserver(line, nil, "consumed")
			return nil
		}
		return line
	}
	if len(transformed) == 0 || bytes.Equal(transformed, trimmed) {
		return line
	}
	if IsDonePayload(transformed) {
		// AUD-02: the bare-JSON variant of the synthesized terminal gets the
		// same composition as the data-line variant — canonical [DONE] frame,
		// held-tail flush, done notification. Forwarding the bare value raw
		// would emit an unprefixed frame strict clients ignore and drop the
		// withheld tail.
		r.sawDone = true
		return r.finishStreamFrames([]byte(canonicalDoneFrame), []byte(canonicalDoneFrame))
	}
	// AUD-12: a bare-JSON upstream through the same converter produces the
	// same multi-object emissions — frame them identically.
	return r.frameTransformedEvents(transformed, nil)
}

// ReasoningTokenExtractors lists the known provider spellings for the
// reasoning/thinking token counter inside a usage object (NENYA-33).
// They are alternative spellings of one counter, so the first non-zero
// value wins; zero-token placeholders never register.
var reasoningTokenExtractors = []func(usage map[string]interface{}) int{
	func(u map[string]interface{}) int { return ToInt(u["reasoning_tokens"]) },
	func(u map[string]interface{}) int {
		if d, ok := u["output_tokens_details"].(map[string]interface{}); ok {
			return ToInt(d["reasoning_tokens"]) // OpenAI current shape
		}
		return 0
	},
	func(u map[string]interface{}) int {
		if d, ok := u["completion_tokens_details"].(map[string]interface{}); ok {
			return ToInt(d["reasoning_tokens"]) // legacy OpenAI shape
		}
		return 0
	},
	func(u map[string]interface{}) int { return ToInt(u["completion_reasoning_tokens"]) }, // Anthropic raw
	func(u map[string]interface{}) int { return ToInt(u["thoughtsTokenCount"]) },          // Gemini native
}

// ExtractReasoningTokens pulls the reasoning/thinking token count from a
// usage object across every known provider spelling. Returns 0 when no
// spelling yields a value.
func ExtractReasoningTokens(usage map[string]interface{}) int {
	for _, extract := range reasoningTokenExtractors {
		if n := extract(usage); n > 0 {
			return n
		}
	}
	return 0
}

// ResponseModelOf returns the model an upstream response reports, across
// wire shapes (NENYA-173): OpenAI/Gemini put it at the top level, an
// Anthropic message_start nests it under "message" (and reports
// "modelVersion" on some native shapes). Empty when the chunk carries
// none. Exported so the buffered replay path reads the same surface.
func ResponseModelOf(chunk map[string]interface{}) string {
	if m, ok := chunk["model"].(string); ok && m != "" {
		return m
	}
	if m, ok := chunk["modelVersion"].(string); ok && m != "" {
		return m
	}
	if msg, ok := chunk["message"].(map[string]interface{}); ok {
		if m, ok := msg["model"].(string); ok && m != "" {
			return m
		}
	}
	return ""
}

// responseModel returns the model for this chunk: the model it reports
// when present, else the last model seen on the stream (NENYA-173).
func (r *SSETransformingReader) responseModel(chunk map[string]interface{}) string {
	if m := ResponseModelOf(chunk); m != "" {
		r.lastResponseModel = m
	}
	return r.lastResponseModel
}

// extractUsageFromMap reads the usage object from an upstream chunk (in any of
// the placements UsageMapFromChunk accepts) and emits the per-chunk deltas of
// every dimension. The object is normalized by NormalizeUsageTotals first, so
// the callback sees one canonical shape: Prompt is the total input including
// the cache dimensions and Completion is the total output including reasoning
// (NENYA-167).
func (r *SSETransformingReader) extractUsageFromMap(chunk map[string]interface{}) {
	usage, ok := UsageMapFromChunk(chunk)
	if !ok {
		return
	}
	totals := NormalizeUsageTotals(usage)
	completion, prompt, total := totals.Completion, totals.Prompt, totals.Total
	cacheHit, cacheMiss, cacheCreation := totals.CacheHit, totals.CacheMiss, totals.CacheCreation
	reasoning := totals.Reasoning
	if allUsageFieldsZero(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning) {
		return
	}
	dCompletion, lastCompletion := advanceHighWater(completion, r.lastCompletionTokens)
	dPrompt, lastPrompt := advanceHighWater(prompt, r.lastPromptTokens)
	dCacheHit, lastCacheHit := advanceHighWater(cacheHit, r.lastCacheHitTokens)
	dCacheMiss, lastCacheMiss := advanceHighWater(cacheMiss, r.lastCacheMissTokens)
	dCacheCreation, lastCacheCreation := advanceHighWater(cacheCreation, r.lastCacheCreationTokens)
	dReasoning, lastReasoning := advanceHighWater(reasoning, r.lastReasoningTokens)
	// A provider-reported total is used as-is; a shape whose total Nenya
	// derives (Anthropic/Gemini native) reports its output on a later chunk, so
	// the total delta must follow the parts rather than the derived per-chunk
	// sum, which would under-report the request.
	dTotal, lastTotal := advanceHighWater(total, r.lastTotalTokens)
	if !totals.TotalReported {
		dTotal = dPrompt + dCompletion
		lastTotal = r.lastTotalTokens
	}
	if allDeltasZero(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss, dCacheCreation, dReasoning) {
		return
	}
	r.lastCompletionTokens = lastCompletion
	r.lastPromptTokens = lastPrompt
	r.lastTotalTokens = lastTotal
	r.lastCacheHitTokens = lastCacheHit
	r.lastCacheMissTokens = lastCacheMiss
	r.lastCacheCreationTokens = lastCacheCreation
	r.lastReasoningTokens = lastReasoning
	r.onUsage(UsageData{
		CompletionTokens:    dCompletion,
		PromptTokens:        dPrompt,
		TotalTokens:         dTotal,
		CacheHitTokens:      dCacheHit,
		CacheMissTokens:     dCacheMiss,
		CacheCreationTokens: dCacheCreation,
		ReasoningTokens:     dReasoning,
		// AUD-27 (NENYA-242): the flag was computed by the normalizer but
		// never copied into the streaming callback's UsageData, so the
		// streaming consumer's ambiguous-semantics handling (fold-note) was
		// dead code — only the buffered path ever surfaced it.
		AmbiguousCacheSemantics: totals.AmbiguousCacheSemantics,
		Model:                   r.responseModel(chunk),
	})
}

func allUsageFieldsZero(completion, prompt, total, cacheHit, cacheMiss, cacheCreation, reasoning int) bool {
	return completion == 0 && prompt == 0 && total == 0 && cacheHit == 0 && cacheMiss == 0 && cacheCreation == 0 && reasoning == 0
}

func allDeltasZero(dCompletion, dPrompt, dTotal, dCacheHit, dCacheMiss, dCacheCreation, dReasoning int) bool {
	return dCompletion == 0 && dPrompt == 0 && dTotal == 0 && dCacheHit == 0 && dCacheMiss == 0 && dCacheCreation == 0 && dReasoning == 0
}

// advanceHighWater returns the per-chunk delta of a cumulative usage counter
// and the new high-water mark. A value that does not exceed the mark yields no
// delta and keeps the mark, so a provider that omits a field in some chunks
// (Anthropic's message_delta carries only the output side) or reports a
// smaller value after a partial one can never have the same tokens counted
// twice (NENYA-167).
func advanceHighWater(current, last int) (delta, mark int) {
	if current <= last {
		return 0, last
	}
	return current - last, current
}

// ToInt converts an interface{} value to int, handling float64, string, and int types.
// Returns 0 for unsupported types or on parse error.
func ToInt(v interface{}) int {
	switch n := v.(type) {
	case float64:
		if n > math.MaxInt || n < math.MinInt {
			return 0
		}
		return int(n)
	case int:
		return n
	case string:
		i, err := strconv.Atoi(n)
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

// normalizeToolCalls processes tool_calls deltas in a streaming response chunk.
// It normalizes missing tool_call IDs, buffers args chunks that arrive before names,
// and merges pending data when names arrive. Returns true if the chunk was mutated.
func normalizeToolCalls(chunk map[string]interface{}, state *toolCallState) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	choices, ok := chunk["choices"].([]interface{})
	if !ok {
		return false
	}
	mutated := false
	for _, c := range choices {
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

		keep, mutatedInDelta := processToolCallDelta(tcs, state)
		if mutatedInDelta {
			mutated = true
		}
		if len(keep) != len(tcs) {
			mutated = true
			if len(keep) == 0 {
				delete(delta, "tool_calls")
			} else {
				delta["tool_calls"] = keep
			}
		}
	}
	return mutated
}

// processToolCallDelta normalizes a single tool_calls delta array. It buffers
// argument chunks that arrive before the tool name and merges pending data
// when names arrive. Returns the filtered slice and whether it was mutated.
//
// Identity rules (NENYA-48): the call index falls back to the ARRAY POSITION
// when the upstream omits it (never 0-for-all, which collides across calls
// in one delta); invalid, empty, or oversized IDs are replaced with the
// deterministic synthetic ID for that index; and an ID already claimed by a
// different index in the same stream is deterministically deconflicted the
// same way — same input, same output, across requests.
func processToolCallDelta(tcs []interface{}, state *toolCallState) ([]interface{}, bool) {
	keep := make([]interface{}, 0, len(tcs))
	mutated := false

	for pos, tcRaw := range tcs {
		tc, ok := tcRaw.(map[string]interface{})
		if !ok {
			continue
		}
		rawIdx, hasIdx := tc["index"]
		var idx int
		if hasIdx {
			idx = util.ParseToolCallIndex(rawIdx, pos)
		} else {
			idx = pos
		}

		if state.seenIndices[idx] {
			keep = append(keep, tc)
			continue
		}

		mutated = normalizeToolCallID(tc, idx, state) || mutated

		fn, hasFn := tc["function"]
		tcID, _ := tc["id"].(string)
		fnNameStr := extractToolCallName(fn, hasFn, &mutated, tcID)
		fnArgsStr := extractToolCallArgs(fn, hasFn)

		if fnNameStr != "" {
			handleToolCallWithName(tc, idx, tcID, fnArgsStr, state, &mutated)
			state.seenIndices[idx] = true
			keep = append(keep, tc)
			continue
		}

		if fnArgsStr != "" && fnArgsStr != "{}" {
			bufferPendingToolCall(idx, tcID, fnArgsStr, state, &mutated)
		}
	}
	return keep, mutated
}

// normalizeToolCallID enforces deterministic tool-call identity on a new
// (not-yet-seen) call index: invalid/empty/oversized upstream IDs and IDs
// already claimed by another index are replaced with the synthetic ID for
// the call index. Returns whether the chunk was mutated.
func normalizeToolCallID(tc map[string]interface{}, idx int, state *toolCallState) bool {
	if !util.ValidToolCallID(tc["id"]) {
		tc["id"] = util.SyntheticToolCallID(idx)
		return true
	}
	id, _ := tc["id"].(string)
	if state.seenIDs[id] {
		// Duplicate upstream ID on a different index: keep the first
		// claimant, deterministically re-key the later one. Synthesizing
		// from the index keeps the mapping stable across requests.
		tc["id"] = util.SyntheticToolCallID(idx)
		return true
	}
	state.seenIDs[id] = true
	return false
}

func handleToolCallWithName(tc map[string]interface{}, idx int, tcID string, fnArgsStr string, state *toolCallState, mutated *bool) {
	if p, ok := state.pending[idx]; ok {
		mergePendingToolCall(tc, idx, tcID, fnArgsStr, p, mutated)
		delete(state.pending, idx)
		slog.Debug("merged pending tool_call data on name arrival",
			"index", idx,
			"pending_args_len", len(p.args),
			"tool_call_id", tcID,
		)
	}
}

func mergePendingToolCall(tc map[string]interface{}, idx int, tcID string, fnArgsStr string, pending *pendingToolCall, mutated *bool) {
	if (tcID == "" || util.IsSyntheticToolCallID(tcID)) && pending.id != "" {
		tc["id"] = pending.id
	}
	fn, ok := tc["function"].(map[string]interface{})
	if !ok {
		return
	}
	// "{}" is a JSON placeholder with no real data; use pending args as-is.
	if fnArgsStr == "" || fnArgsStr == "{}" {
		fn["arguments"] = pending.args
	} else {
		fn["arguments"] = pending.args + fnArgsStr
	}
	*mutated = true
}

func bufferPendingToolCall(idx int, tcID string, fnArgsStr string, state *toolCallState, mutated *bool) {
	state.pending[idx] = &pendingToolCall{
		id:   tcID,
		args: fnArgsStr,
	}
	*mutated = true
	slog.Debug("buffered tool_call entry missing name, waiting for name chunk",
		"index", idx,
		"buffered_args_len", len(fnArgsStr),
		"tool_call_id", tcID,
	)
}

func extractToolCallName(fn interface{}, hasFn bool, mutated *bool, tcID string) string {
	if !hasFn || fn == nil {
		return ""
	}
	fnMap, ok := fn.(map[string]interface{})
	if !ok {
		return ""
	}
	fnNameRaw := fnMap["name"]
	switch fnName := fnNameRaw.(type) {
	case string:
		return fnName
	case nil:
		return ""
	default:
		coerced := fmt.Sprintf("%v", fnNameRaw)
		fnMap["name"] = coerced
		*mutated = true
		slog.Debug("coerced non-string function.name to string",
			"coerced_value", coerced,
			"original_type", reflect.TypeOf(fnNameRaw).String(),
			"tool_call_id", tcID,
		)
		return coerced
	}
}

func extractToolCallArgs(fn interface{}, hasFn bool) string {
	if !hasFn || fn == nil {
		return ""
	}
	fnMap, ok := fn.(map[string]interface{})
	if !ok {
		return ""
	}
	args, _ := fnMap["arguments"].(string)
	return args
}
