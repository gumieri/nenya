package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
)

func (r *SSETransformingReader) handleScannerDone() {
	// NENYA-344: complete every open tool-call assembly at stream end so
	// observers receive the held remainders and completion signals even
	// when the wire never sent an explicit per-call completion.
	if r.toolArgs != nil {
		// Capture the cap telemetry BEFORE Flush: the flush resets both
		// counters, so reading them afterwards would never fire for a
		// saturation-only breach.
		sat, over := r.toolArgs.Saturated(), r.toolArgs.OverCapCalls()
		r.toolArgs.Flush()
		if over > 0 || sat {
			slog.Warn("tool-call argument assembly hit accumulation caps",
				"overCapCalls", over,
				"saturated", sat)
		}
	}

	// NENYA-337: drain the stateful transformer's held tail BEFORE the
	// hygiene warn below and before classifying the ending — see
	// drainTransformerTailBeforeClassification. The drain updates the
	// terminal-state flags from the RAW held bytes (no observer notify —
	// framing/notify happens once in the composition paths), so the logged
	// snapshot reflects the held close.
	r.drainTransformerTailBeforeClassification()

	// Terminal-state matrix hygiene (NENYA-46): argument chunks buffered
	// waiting for a name that never arrived are silently dropped by
	// design (they were never forwarded). Surface the loss — a provider
	// emitting args-before-name then terminating has lost those tool
	// calls for the client, and the log is the only trace.
	if n := r.pendingToolCallCount(); n > 0 {
		slog.Warn("stream ended with unflushed tool_call argument buffers (name chunk never arrived)",
			"pending", n,
			"sawDone", r.sawDone,
			"sawFinishReason", r.sawFinishReason)
	}

	switch r.scanner.Err() {
	case nil:
		r.classifyCleanEOF()
		r.err = io.EOF
	case bufio.ErrTooLong:
		slog.Warn("SSE scanner error: line too long, injecting gateway_error")
		r.injectErrorBuffer("upstream SSE line exceeded maximum scanner buffer")
		r.err = r.scanner.Err()
	default:
		slog.Warn("SSE scanner error", "err", r.scanner.Err())
		r.err = r.scanner.Err()
	}
	// NENYA-345: a tool-args guard verdict — the guard's withheld tail is
	// EOF-evaluated during the flush above — outranks the ending
	// classification: the client gets the policy block frame instead of a
	// clean end. drainAfterScanDone routes it through the same blocked
	// handling as any text-path block.
	if r.toolArgs != nil && r.toolArgs.Blocked() {
		r.err = r.toolArgs.BlockedError()
	}
	// NENYA-169: terminate the final event. A spec-compliant SSE client
	// discards a pending event at EOF unless a blank line closed it, so an
	// upstream that omitted the trailing blank line — or a stream that ended on
	// a forwarded frame — would silently lose its last event, including the
	// [DONE] marker. Only when no injected terminal already closed the stream:
	// injectDoneOnly and injectErrorBuffer emit their own terminator (preceded
	// by eventSeparator when the previous line was unterminated).
	r.closePendingEvent()
}

// drainTransformerTailBeforeClassification drains the stateful transformer's
// held tail BEFORE classifying the ending (NENYA-337). A converter that holds
// its close (the OpenAI→Anthropic stage holds message_delta/message_stop for
// a trailing usage chunk that a Responses terminal never sends) must have
// those frames counted as the stream's terminal state — without the drain a
// clean close classifies as a mid-generation cut (spurious gateway_error, no
// stop_reason). The pulled frame is memoized: the composition paths below
// emit exactly these bytes once.
func (r *SSETransformingReader) drainTransformerTailBeforeClassification() {
	r.memoizeTransformerTail()
	if len(r.drainedTail) == 0 {
		return
	}
	// The raw tail is concatenated JSON objects (the converter appends
	// marshaled frames) — split it the way frameTransformedEvents does so
	// EVERY terminal frame updates the matrix, then feed each object to the
	// progress tracker (raw bytes, no observer notify; framing is
	// transformerTailFrame's job). The splitter's remainder is deliberately
	// dropped here: the tail is gateway-built (never carries trailing
	// garbage), while frameTransformedEvents must still forward a remainder
	// to the client for upstream-shaped tails.
	objs, _, ok := splitConcatenatedJSON(r.drainedTail)
	if !ok {
		r.trackStreamProgress(r.drainedTail)
		return
	}
	for _, obj := range objs {
		r.trackProgressLine(obj)
	}
}

// classifyCleanEOF routes a scanner-nil (clean EOF) ending through the
// terminal-state matrix: normal [DONE] end, finish-reason close (with the
// anthropic-finish suppression and the NENYA-337 drained-tail composition),
// the NENYA-337 error-terminal cell (no injection, no empty count), the
// NENYA-160 empty cell, and the mid-generation cut.
func (r *SSETransformingReader) classifyCleanEOF() {
	switch {
	case r.sawDone:
		// Normal end.
	case r.sawFinishReason:
		if r.sawAnthropicFinish || r.sawConvertedAnthropicFinish {
			// Native-Anthropic passthrough: the stream completed through
			// its own terminal (message_delta stop_reason / message_stop)
			// and the client already received the upstream's message_stop.
			// Appending OpenAI framing (a bare data: [DONE]) on top of the
			// client's wire shape would corrupt the passthrough — end
			// exactly where the upstream ended (AUD-01). A completion
			// recognized from an OpenAI finish_reason line — even on a
			// stream that also carried Anthropic deltas — keeps the
			// injected [DONE]. Interaction note: the sawContent &&
			// sawFinishReason early return at the top of
			// trackStreamProgress seals the matrix, so on a pathological
			// mixed wire an OpenAI finish recognized BEFORE the Anthropic
			// terminal wins the injection — whoever completes the stream
			// first in arrival order decides.
			// The exfil guard's held tail (NENYA-205) must still reach
			// the client at the clean end. The normal drain happens at
			// the raw message_stop (transformSSEData's pre-terminal
			// hook); this EOF-side drain is the safety net for streams
			// that completed via message_delta alone, shaped as a
			// content_block_delta via lastSurface — mirroring
			// injectDoneOnly minus the [DONE]. NENYA-337: the drained
			// transformer tail (a held message_delta/message_stop close
			// the client has NOT received yet — the pre-drain
			// classification is what surfaced it here) rides the same
			// composition.
			var buf []byte
			if held, heldOK := r.synthesizeHeldFlush(); heldOK {
				buf = r.appendSSEEvent(buf, held)
			}
			if tailFrame := r.transformerTailFrame(); tailFrame != nil {
				buf = r.appendSSEEvent(buf, tailFrame)
			}
			if len(buf) > 0 {
				r.buffer = buf
				r.pos = 0
				r.lastLineBlank = true
			}
			return
		}
		slog.Debug("SSE scanner ended without [DONE] after finish_reason, injecting silent [DONE]",
			"has_transformer", r.transformer != nil)
		r.injectDoneOnly()
	case r.sawResponsesTerminal:
		// NENYA-346: the stream ended through its own Responses terminal
		// (completed/failed/incomplete) — a clean end, never a
		// mid-generation cut: no gateway_error injection, and no empty-200
		// count (the upstream answered; emptiness is its answer, the same
		// ruling as the error-terminal cell). A failed terminal lands
		// here too (the error event went out live; the held tail follows
		// it — NENYA-205's rule: the tail reaches the client whatever the
		// ending). The raw-terminal hook skips the nil-transformer
		// passthrough (line-split scanning composes mid-frame), so this is
		// where the passthrough's tail drains — as a typed event frame
		// after the complete terminal.
		r.drainHeldAtResponsesEOF()
	case r.sawErrorTerminal && !r.sawFinishReason:
		// NENYA-337: the stream ended on an upstream error frame (the
		// converted {"error":{...}} or the raw failed event already
		// forwarded to the client). Neither a finish_reason nor content
		// followed: inject nothing and count nothing — a FAILED stream
		// is not an empty-200 degradation signal.
		slog.Debug("SSE scanner ended on an upstream error frame; no injection",
			"has_transformer", r.transformer != nil)
		// NENYA-205: the exfil guard's withheld last fragment must reach
		// the client even on this ending — the error frame went out live,
		// but a fragment withheld before it is still outstanding (the normal
		// drain hooks fire at the raw terminal and at the finish cells,
		// neither of which an error-terminal stream reached).
		if held, heldOK := r.synthesizeHeldFlush(); heldOK {
			r.buffer = r.appendSSEEvent(nil, held)
			r.pos = 0
			r.lastLineBlank = true
		}
	case !r.sawContent:
		// NENYA-160: the always-on empty-200 degradation signal — this
		// path is flag-independent (it IS the empty-stream handler).
		if r.onEmptyStream != nil {
			r.onEmptyStream()
		}
		slog.Warn("SSE scanner ended without [DONE] marker and no content, injecting gateway_error",
			"has_transformer", r.transformer != nil)
		r.injectErrorBuffer("upstream stream ended without [DONE]")
	default:
		if r.suppressCutError {
			slog.Debug("SSE scanner ended without [DONE] mid-generation, cut error suppressed for continuation",
				"has_transformer", r.transformer != nil,
				"saw_content", r.sawContent)
			return
		}
		slog.Warn("SSE scanner ended without [DONE] marker mid-generation, injecting gateway_error",
			"has_transformer", r.transformer != nil,
			"saw_content", r.sawContent)
		r.injectErrorBuffer("upstream stream ended without [DONE]")
	}
}

// drainHeldAtResponsesEOF drains the exfil guard's held tail at a clean
// Responses-terminal end (NENYA-346), buffered as a typed event frame the
// next Read delivers. See classifyCleanEOF's sawResponsesTerminal cell.
func (r *SSETransformingReader) drainHeldAtResponsesEOF() {
	held, heldOK := r.synthesizeHeldFlush()
	if !heldOK {
		return
	}
	r.buffer = r.appendSSEEvent(nil, held)
	r.pos = 0
	r.lastLineBlank = true
}

// injectDoneOnly buffers a bare [DONE] event without a gateway_error. Used for
// benign stream endings where the model signaled completion but the upstream
// omitted the [DONE] marker.
func (r *SSETransformingReader) injectDoneOnly() {
	held, heldOK := r.synthesizeHeldFlush()
	tailFrame := r.transformerTailFrame()
	var buf []byte
	if heldOK {
		buf = r.appendSSEEvent(buf, held)
	}
	if tailFrame != nil {
		buf = r.appendSSEEvent(buf, tailFrame)
	}
	buf = r.appendSSEEvent(buf, []byte("data: [DONE]"))
	r.buffer = buf
	r.pos = 0
	r.lastLineBlank = true
}

// injectErrorBuffer creates a gateway_error SSE event + [DONE] and places it
// in r.buffer so the client receives the error before EOF. A nil-transformer
// passthrough that carried Responses-format frames instead gets a
// Responses-native error event (injectResponsesErrorBuffer) — a chat-shaped
// payload plus a stray [DONE] would land on a stream the client parses by
// event name (NENYA-346). The gate is ANY Responses frame (not just text
// deltas): a stream cut before its first text delta — or one that only
// streamed argument deltas — is still Responses-format to its client.
func (r *SSETransformingReader) injectErrorBuffer(message string) {
	if r.transformer == nil && r.sawResponsesFrame {
		r.injectResponsesErrorBuffer(message)
		return
	}
	r.injectedError = true
	slog.Warn("injecting gateway_error into stream", "message", message, "sawDone", r.sawDone)
	errPayload, err := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "gateway_error",
		},
	})
	if err != nil {
		slog.Error("failed to marshal error payload", "error", err, "message", message)
		errPayload = []byte(`{"error":{"message":"internal error","type":"gateway_error"}}`)
	}
	held, heldOK := r.synthesizeHeldFlush()
	var buf []byte
	if heldOK {
		buf = r.appendSSEEvent(buf, held)
	}
	buf = r.appendSSEEvent(buf, append([]byte("data: "), errPayload...))
	// The [DONE] terminator glues on directly: the error event above already
	// closed with a blank line, so a separator here would add a stray one.
	buf = append(buf, "data: [DONE]\n\n"...)
	r.buffer = buf
	r.pos = 0
	r.lastLineBlank = true
	if r.observer != nil {
		var errMap map[string]any
		if err := json.Unmarshal(errPayload, &errMap); err != nil {
			slog.Error("failed to unmarshal error payload for observer", "error", err)
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: map[string]any{"message": message, "type": "gateway_error"}, GatewayInjected: true})
		} else {
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: errMap, GatewayInjected: true})
		}
	}
}

// injectResponsesErrorBuffer buffers a Responses-native error event for a
// nil-transformer passthrough that carried Responses-format frames
// (NENYA-346): `event: error` + a typed data payload, no [DONE] — a
// chat-shaped gateway_error payload plus a stray [DONE] data line would
// corrupt a stream the client parses by event name. The held exfil tail
// composes ahead of the frame (NENYA-205: the tail reaches the client
// whatever the ending).
func (r *SSETransformingReader) injectResponsesErrorBuffer(message string) {
	r.injectedError = true
	slog.Warn("injecting gateway_error into responses stream", "message", message)
	errPayload, err := json.Marshal(map[string]any{
		"type":    "error",
		"code":    "gateway_error",
		"message": message,
	})
	if err != nil {
		errPayload = []byte(`{"type":"error","code":"gateway_error","message":"internal error"}`)
	}
	held, heldOK := r.synthesizeHeldFlush()
	var buf []byte
	if heldOK {
		buf = r.appendSSEEvent(buf, held)
	}
	// One frame, event line and data line together: composing them through
	// appendSSEEvent separately would strand the event line (a blank line
	// between it and its data makes the event data-less, and strict
	// Responses SDKs dispatch on event names).
	frame := append([]byte("event: error\ndata: "), errPayload...)
	buf = append(buf, frame...)
	buf = append(buf, "\n\n"...)
	r.buffer = buf
	r.pos = 0
	r.lastLineBlank = true
	// Observer parity with the chat injector: the gateway-injected error is
	// classified for any observer (breaker semantics, MCP accumulator) the
	// same way regardless of the wire shape it was rendered in.
	if r.observer != nil {
		var errMap map[string]any
		if err := json.Unmarshal(errPayload, &errMap); err != nil {
			slog.Error("failed to unmarshal error payload for observer", "error", err)
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: map[string]any{"message": message, "type": "gateway_error"}, GatewayInjected: true})
		} else {
			r.observer.OnSSEEvent(SSEEvent{Type: "error", Data: errMap, GatewayInjected: true})
		}
	}
}

// drainAfterScanDone returns any injected error buffer data before signaling
// the terminal error. This ensures error+[DONE] events reach the client
// when the upstream stream ends without [DONE].
func (r *SSETransformingReader) drainAfterScanDone(p []byte) (int, error) {
	if r.pos < len(r.buffer) {
		n := copy(p, r.buffer[r.pos:])
		r.pos += n
		if r.pos >= len(r.buffer) {
			r.buffer = nil
			r.pos = 0
		}
		return n, nil
	}
	if r.observer != nil && !r.closed {
		r.closed = true
		r.observer.OnStreamClose(r.err)
	}
	return r.finishWithError(r.err)
}

func (r *SSETransformingReader) transformSSEData(line []byte) []byte {
	origData, _ := SSEDataField(line)

	if len(origData) == 0 {
		r.notifySSEObserver(line, nil, "keepalive")
		return line
	}
	if IsDonePayload(origData) {
		r.sawDone = true
		// upstreamLine keeps the SSEObserver contract (Raw is the pre-filter
		// upstream bytes) while the emitted frame is canonicalized.
		upstreamLine := line
		if string(line) != canonicalDoneFrame {
			// Forward the canonical spelling: a client with a strict parser
			// must see the sentinel (the tolerance premise is that strict
			// parsers exist in the wild).
			line = []byte(canonicalDoneFrame)
		}
		return r.finishStreamFrames(upstreamLine, line)
	}

	data := origData
	parsed := r.tryParseJSON(data)

	// Resumable terminals (NENYA-174) are remembered on the RAW surface:
	// format transformers erase the reason before the capture tee sees it.
	if !r.sawResumableStop && isResumableStopPayload(parsed) {
		r.sawResumableStop = true
	}
	r.noteRawAnthropicDelta(parsed)
	r.noteRawResponsesState(parsed)

	// Provider-private/telemetry frames (NENYA-174) are suppressed from
	// every downstream surface when the filter is enabled; the stream
	// itself continues.
	if r.dropPrivateEvents && isPrivateSSEType(parsed) {
		return nil
	}

	// Upstream error payloads routed through a healthy 200 stream (e.g. an
	// aggregator whose provider leg failed after the stream opened) are tagged
	// with "error" so observers can record circuit-breaker failures. The error
	// line itself is still forwarded to the client unchanged below.
	// NENYA-337: the Responses failed event is the same class (the bootstrap
	// probe and the MCP accumulator already recognize it).
	if IsStreamErrorPayload(parsed) || IsResponseFailedEvent(parsed) {
		r.notifySSEObserver(line, parsed, "error")
	}

	parsed, filtersMutated := r.applyStreamFilters(parsed)
	if r.anyFilterBlocked() {
		// A filter blocked this exact chunk: suppress the violating
		// line; the stream terminates with the filter-specific error
		// next Read. Nil parsed from malformed JSON still falls through
		// to passthrough below.
		return nil
	}
	if filtersMutated {
		// Filter rewrites live in the parsed map: re-encode so the wire
		// output (transformer input and passthrough) carries them.
		if reencoded, err := json.Marshal(parsed); err == nil {
			data = reencoded
		}
	}

	r.callUsageAndContentCallbacks(parsed)

	// NENYA-205/AUD-02: a stateful transformer closes at the raw terminal
	// (the AnthropicTransformer sets streamDone at message_stop and drops
	// every later chunk), so the exfil guard's held tail must drain BEFORE
	// the terminal reaches it. The drained frame re-enters the full pipeline
	// here — while the transformer is still live — and composes ahead of the
	// terminal line, so the tail precedes the client's terminal event. The
	// EOF-side drains (handleScannerDone's Anthropic clean end,
	// finishStreamFrames at [DONE]) remain as safety nets for streams that
	// never delivered the raw terminal; a drained guard's Flush is a no-op.
	preTerminal := r.drainHeldAtRawTerminal(parsed)

	var result []byte
	if r.transformer == nil {
		result = r.handleNoTransformer(parsed, data, origData, line)
	} else {
		result = r.handleWithTransformer(parsed, data, origData, line)
	}
	if result == nil {
		// A consumed/suppressed terminal must not discard an already
		// drained held tail (AUD-02): the guard re-armed on composition, so
		// dropping the frame here would lose the tail for good.
		return r.prependHeldFlush(preTerminal, nil)
	}
	return r.prependHeldFlush(preTerminal, result)
}

// drainHeldAtRawTerminal drains the exfil guard's held tail when the RAW
// chunk is the Anthropic message_stop terminal (NENYA-205/AUD-02). Returns
// the transformed flush frame, or nil when there is nothing to drain. Gated
// on real terminal-sequence evidence (a raw message_delta preceded the stop):
// a spoofed bare message_stop must not trigger an early drain — and even if
// one does, synthesizeHeldFlush re-arms the guard so later chunks stay
// filtered. The hook assumes a terminal-closing transformer (the
// AnthropicTransformer is the one that synthesizes [DONE] at message_stop);
// a transformer that stays live past message_stop would simply receive the
// tail slightly earlier than [DONE]/EOF — benign.
func (r *SSETransformingReader) drainHeldAtRawTerminal(parsed map[string]interface{}) []byte {
	if r.exfilGuard == nil || r.exfilGuard.IsBlocked() {
		return nil
	}
	// The Responses terminals close the ResponsesTransformer to later
	// chunks the same way message_stop closes the AnthropicTransformer
	// (NENYA-337 taxonomy), so a held tail must compose before the
	// terminal reaches it — the transformer's output is one frame, so
	// the composition order is preserved. The NIL-transformer passthrough
	// is excluded: line-split scanning already emitted the terminal's
	// event: line, and composing between it and the data line would
	// splice the tail into the terminal's frame — there the scanner-done
	// cell (classifyCleanEOF's sawResponsesTerminal) drains after the
	// complete frame instead. Evidence gate mirrors the Anthropic cell: a
	// raw Responses TEXT delta must have preceded (a spoofed bare
	// terminal must not trigger an early drain — and even if one does,
	// synthesizeHeldFlush re-arms the guard).
	if r.sawRawAnthropicDelta && isRawAnthropicStop(parsed) {
		held, heldOK := r.synthesizeHeldFlush()
		if heldOK {
			return held
		}
		return nil
	}
	if r.transformer != nil && r.sawRawResponsesDelta && isRawResponsesTerminal(parsed) {
		held, heldOK := r.synthesizeHeldFlush()
		if heldOK {
			return held
		}
	}
	return nil
}

// isRawAnthropicStop reports whether the parsed RAW chunk is the Anthropic
// message_stop terminal (the event whose transformer-side handling closes the
// AnthropicTransformer to later chunks).
func isRawAnthropicStop(parsed map[string]interface{}) bool {
	if parsed == nil {
		return false
	}
	typ, _ := parsed["type"].(string)
	return typ == "message_stop"
}

// isRawResponsesTerminal reports whether the parsed RAW chunk is one of the
// Responses-format terminal events (the NENYA-337 taxonomy: completed,
// failed, incomplete) whose transformer-side handling closes the
// ResponsesTransformer to later chunks.
func isRawResponsesTerminal(parsed map[string]interface{}) bool {
	if parsed == nil {
		return false
	}
	switch typ, _ := parsed["type"].(string); typ {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	default:
		return false
	}
}

// noteRawAnthropicDelta records raw terminal-sequence evidence (AUD-02): the
// pre-terminal exfil drain requires a raw message_delta before honoring a
// message_stop, so a spoofed bare terminal cannot trigger an early drain.
func (r *SSETransformingReader) noteRawAnthropicDelta(parsed map[string]interface{}) {
	if r.sawRawAnthropicDelta {
		return
	}
	if typ, _ := parsed["type"].(string); typ == "message_delta" {
		r.sawRawAnthropicDelta = true
	}
}

// noteRawResponsesState records the Responses-format evidence flags
// (NENYA-346): that a raw TEXT delta was seen (the spoofing-guard twin of
// the Anthropic message_delta evidence — the pre-terminal drain on a
// Responses terminal requires a text delta before it), that a raw terminal
// event was seen (the clean-end classification for a passthrough stream
// that ends through its own terminal instead of [DONE]), and that ANY
// Responses frame was seen (the injection routing gate — a chat-shaped
// injected terminal must not land on a stream the client parses by event
// name, whatever the frame mix).
func (r *SSETransformingReader) noteRawResponsesState(parsed map[string]interface{}) {
	if typ, _ := parsed["type"].(string); strings.HasPrefix(typ, "response.") {
		r.sawResponsesFrame = true
	}
	r.noteRawResponsesDelta(parsed)
	if isRawResponsesTerminal(parsed) {
		r.sawResponsesTerminal = true
	}
}

// noteRawResponsesDelta records the Responses-format evidence twin
// (NENYA-346): the pre-terminal exfil drain on a Responses terminal requires
// a raw Responses TEXT delta before it — the same spoofing guard as the
// Anthropic cell.
func (r *SSETransformingReader) noteRawResponsesDelta(parsed map[string]interface{}) {
	if r.sawRawResponsesDelta {
		return
	}
	if text, ok := parsed["delta"].(string); ok && text != "" && !responsesArgsDelta(parsed) {
		r.sawRawResponsesDelta = true
	}
}

// prependHeldFlush composes a pre-terminal held-flush frame ahead of the
// terminal line (NENYA-205/AUD-02): the tail is delivered before the client's
// terminal event instead of landing after it, and before the transformer's
// streamDone gate would drop a post-terminal synthetic chunk. A nil terminal
// line (consumed/suppressed terminal) still delivers the drained tail.
func (r *SSETransformingReader) prependHeldFlush(preTerminal, terminalLine []byte) []byte {
	if preTerminal == nil {
		return terminalLine
	}
	// The drained tail becomes a terminated event; the terminal line stays
	// raw — it is the pipeline's own output and is terminated downstream.
	out := r.appendSSEEvent(nil, preTerminal)
	return append(out, terminalLine...)
}

// notifySSEObserver forwards a parsed SSE event to the registered observer.
// Events already classified and notified as upstream errors (eventType "error")
// are suppressed here so observers count each error exactly once.
func (r *SSETransformingReader) notifySSEObserver(line []byte, parsed map[string]interface{}, eventType string) {
	if r.observer == nil {
		return
	}
	// Upstream error payloads are already notified with Type "error" when
	// parsed; suppress the duplicate downstream notify so observers count each
	// error event exactly once. NENYA-337: the Responses failed event is the
	// same class.
	if eventType == "" && (IsStreamErrorPayload(parsed) || IsResponseFailedEvent(parsed)) {
		return
	}
	r.observer.OnSSEEvent(SSEEvent{Type: eventType, Raw: line, Data: parsed})
}

// transformerTailFrame returns the memoized transformer tail, framed for the
// wire (TailFinisher, AUD-29 / NENYA-241). Nil when the transformer holds
// nothing or does not implement TailFinisher. Memoized (NENYA-337):
// handleScannerDone drains the raw tail before classifying (feeding
// trackStreamProgress directly — no observer notify), and the composition
// paths frame + notify the same bytes exactly once.
func (r *SSETransformingReader) transformerTailFrame() []byte {
	r.memoizeTransformerTail()
	if len(r.drainedTail) == 0 {
		return nil
	}
	return r.frameTransformedEvents(r.drainedTail, nil)
}

// memoizeTransformerTail pulls the transformer's FinishTail once and stores
// the raw bytes (framing — and its observer notify — happens only in
// transformerTailFrame, so the classification-time drain never notifies).
func (r *SSETransformingReader) memoizeTransformerTail() {
	if r.tailDrained {
		return
	}
	r.tailDrained = true
	if tailer, ok := r.transformer.(TailFinisher); ok {
		r.drainedTail = tailer.FinishTail()
	}
}

// finishStreamFrames composes the clean-terminal frame sequence at
// [DONE]: the exfil guard's held tail first (NENYA-205 — the response's
// last fragment must not be withheld), then the synthetic finish_reason
// chunk when one is due, then the transformer's held closing events
// (TailFinisher), then the canonical [DONE]. Each synthesized
// frame is a complete blank-line-separated event (NENYA-169 convention).
// With nothing synthesized the upstream line passes through unchanged.
func (r *SSETransformingReader) finishStreamFrames(upstreamLine, line []byte) []byte {
	heldLine, heldOK := r.synthesizeHeldFlush()
	synthLine, synthOK := r.synthesizeTerminalFinish()
	tailFrame := r.transformerTailFrame()
	r.notifySSEObserver(upstreamLine, nil, "done")
	if !heldOK && !synthOK && tailFrame == nil {
		return line
	}
	out := []byte{}
	for _, part := range [][]byte{heldLine, synthLine, tailFrame, line} {
		if len(part) == 0 {
			continue
		}
		out = r.appendSSEEvent(out, part)
	}
	return out
}

// synthesizeHeldFlush drains the exfil guard's held tail (NENYA-205)
// into a synthetic content chunk routed through the full pipeline: the
// format transformer converts it exactly like the upstream chunks that
// preceded it, and the content callbacks and observer see it. The guard
// is drained first, so the re-entrant pass cannot re-hold its own
// output. Returns the fully transformed wire frame (no trailing event
// separator — finishStreamFrames supplies it).
func (r *SSETransformingReader) synthesizeHeldFlush() ([]byte, bool) {
	if r.discarding || r.exfilGuard == nil || r.exfilGuard.IsBlocked() {
		return nil, false
	}
	held, action := r.exfilGuard.Flush()
	if held == "" || action == ActionBlock {
		// A no-op Flush still disarms the guard (drained=true is set before
		// the empty check): re-arm so a pre-terminal drain on an empty hold
		// cannot open a pass-through window for later chunks (AUD-02).
		r.exfilGuard.Rearm()
		return nil, false
	}
	encoded := buildExfilFlushChunk(r.lastSurface, held)
	if encoded == nil {
		// Same disarm-window class as below: Flush already set drained, so
		// re-arm even when the payload cannot be built (AUD-02).
		r.exfilGuard.Rearm()
		return nil, false
	}
	out := r.transformSSEData(append([]byte("data: "), encoded...))
	// The composition is complete: re-enable filtering so a drain triggered
	// before the real end of stream (pre-terminal hook) cannot leave the
	// guard permanently pass-through (AUD-02).
	r.exfilGuard.Rearm()
	if len(out) == 0 {
		return nil, false
	}
	// NENYA-346: a passthrough (nil transformer) synthetic Responses event
	// must carry its event: line — strict Responses SDKs dispatch on event
	// names and skip data-only frames. The transformer path emits data
	// lines; the caller's framing turns the composed bytes into a complete
	// frame either way.
	if r.transformer == nil && r.lastSurface.responses {
		out = append([]byte("event: response.output_text.delta\n"), out...)
	}
	return out, true
}

// synthesizeTerminalFinish implements the [DONE]-without-finish_reason cell
// of the terminal-state matrix (NENYA-46): an OpenAI passthrough stream that
// streamed content but never signaled completion leaves the client message
// open when [DONE] arrives — clients keying on finish_reason (tool-call
// reconciliation, turn finalization) hang or reconcile incorrectly. A
// synthetic finish_reason:"stop" chunk is emitted ahead of [DONE] so the
// message closes protocol-correctly.
//
// Deliberately narrow: only pure passthrough (no format transformer — the
// client format would be unknown), only when content was actually streamed,
// never when a finish_reason was seen, and never in discard mode (the early
// [DONE] passthrough branch bypasses this path entirely). The synthesized
// frame is OpenAI-shaped, so an Anthropic-shaped passthrough stream never
// receives one (AUD-01): its own message_stop protocol closes the message.
func (r *SSETransformingReader) synthesizeTerminalFinish() ([]byte, bool) {
	if r.transformer != nil || !r.sawContent || r.sawFinishReason || r.sawAnthropicContent {
		return nil, false
	}
	r.sawFinishReason = true
	synth := []byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
	if r.logger != nil {
		r.logger.Debug("synthesized finish_reason before [DONE] (stream ended with open message)")
	}
	return synth, true
}

func (r *SSETransformingReader) tryParseJSON(data []byte) map[string]interface{} {
	return r.tryParseJSONLabeled(data, "malformed JSON in SSE data line")
}

// tryParseJSONQuiet parses a bare (non-`data:`) JSON line, returning nil on
// malformed input without warning: non-data lines carry arbitrary noise and
// truncated writes, and a parse failure there is not a protocol violation.
func (r *SSETransformingReader) tryParseJSONQuiet(data []byte) map[string]interface{} {
	return r.tryParseJSONLabeled(data, "")
}

func (r *SSETransformingReader) tryParseJSONLabeled(data []byte, warn string) map[string]interface{} {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		if warn != "" && r.logger != nil {
			r.logger.Warn(warn,
				"err", err,
				"data_len", len(data))
		}
		return nil
	}
	return parsed
}
