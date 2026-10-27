package stream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
)

func (r *SSETransformingReader) handleScannerDone() {
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
				// injectDoneOnly minus the [DONE].
				if held, heldOK := r.synthesizeHeldFlush(); heldOK {
					r.buffer = r.appendSSEEvent(nil, held)
					r.pos = 0
					r.lastLineBlank = true
				}
				break
			}
			slog.Debug("SSE scanner ended without [DONE] after finish_reason, injecting silent [DONE]",
				"has_transformer", r.transformer != nil)
			r.injectDoneOnly()
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
				break
			}
			slog.Warn("SSE scanner ended without [DONE] marker mid-generation, injecting gateway_error",
				"has_transformer", r.transformer != nil,
				"saw_content", r.sawContent)
			r.injectErrorBuffer("upstream stream ended without [DONE]")
		}
		r.err = io.EOF
	case bufio.ErrTooLong:
		slog.Warn("SSE scanner error: line too long, injecting gateway_error")
		r.injectErrorBuffer("upstream SSE line exceeded maximum scanner buffer")
		r.err = r.scanner.Err()
	default:
		slog.Warn("SSE scanner error", "err", r.scanner.Err())
		r.err = r.scanner.Err()
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
// in r.buffer so the client receives the error before EOF.
func (r *SSETransformingReader) injectErrorBuffer(message string) {
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
	if IsStreamErrorPayload(parsed) {
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
	if r.exfilGuard == nil || r.exfilGuard.IsBlocked() || !r.sawRawAnthropicDelta || !isRawAnthropicStop(parsed) {
		return nil
	}
	held, heldOK := r.synthesizeHeldFlush()
	if !heldOK {
		return nil
	}
	return held
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
	// error event exactly once.
	if eventType == "" && IsStreamErrorPayload(parsed) {
		return
	}
	r.observer.OnSSEEvent(SSEEvent{Type: eventType, Raw: line, Data: parsed})
}

// transformerTailFrame pulls a stateful transformer's held closing events
// (TailFinisher, AUD-29 / NENYA-241) and frames them for the wire. Nil when
// the transformer holds nothing or does not implement TailFinisher.
func (r *SSETransformingReader) transformerTailFrame() []byte {
	tailer, ok := r.transformer.(TailFinisher)
	if !ok {
		return nil
	}
	tail := tailer.FinishTail()
	if len(tail) == 0 {
		return nil
	}
	return r.frameTransformedEvents(tail, nil)
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
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(data, &parsed); err != nil {
		if r.logger != nil {
			r.logger.Warn("malformed JSON in SSE data line",
				"err", err,
				"data_len", len(data))
		}
		return nil
	}
	return parsed
}

// applyStreamFilters runs the stream-side content filters over one
// parsed chunk, reporting whether any filter rewrote it (callers must
// re-encode; mutations live only in the map). A nil map means a filter
// blocked the chunk.
