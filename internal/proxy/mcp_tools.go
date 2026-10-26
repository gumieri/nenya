package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/util"
)

const (
	mcpExecTimeout = 30 * time.Second
)

// bufferedSSE holds a buffered streaming response from the upstream LLM.
// It contains raw SSE bytes, extracted tool calls, the assistant message,
// and metadata about the response.
type bufferedSSE struct {
	rawBytes         []byte
	toolCalls        []mcpToolCall
	assistantMessage map[string]any
	finishReason     string
	hasContent       bool
	model            string
	// providerName is the target that produced the buffer (NENYA-155): the
	// replay write's client-stall telemetry is labeled with it.
	providerName string
	// exfilBlocked marks a buffer truncated by the egress guard: callers
	// must not replay it and should emit the structured block payload.
	exfilBlocked bool
	// canaryBlocked marks a buffer truncated by the canary tripwire
	// (routes to error_kind=exfil_detected instead of exfil_blocked).
	canaryBlocked bool
	// streamError marks a buffer carrying an in-stream error frame (NENYA-153):
	// the client is replayed the error, so the attempt must not be charged as a
	// breaker success.
	streamError bool
	// emptyResponse marks a buffer whose HTTP-200 carried NO SSE data frames
	// at all (empty body, keepalives only, or non-SSE junk): a failed upstream
	// attempt like any other — counted in the NENYA-160 degradation signal,
	// charged as a breaker failure, and failed over (AUD-16 / NENYA-258).
	emptyResponse    bool
	reasoningContent string
	// calibrationModel is the serving target's model (set at construction
	// by handleBufferedStream): the exact key the dispatch estimate for
	// this buffer was recorded under (NENYA-135).
	calibrationModel string
	// servingTarget is the target this buffer was dispatched to (NENYA-193):
	// cost and spend are attributed to its provider/account, which the model
	// string alone cannot identify when a chain lists one model under several
	// providers.
	servingTarget routing.UpstreamTarget
	// usageScan caches the one-time parse of rawBytes: every data chunk
	// carrying a usage object, in order. The shared scan behind
	// usageChunk/usageTotals so multi-MB buffers are unmarshaled once
	// (NENYA-135, AUD-82 / NENYA-287). Contract: the usage accessors must
	// not be called before the egress/canary guards finish mutating
	// rawBytes; rawBytes is treated as immutable once parsed (current
	// ordering — guards inside forwardBuffered, both keeping usage
	// frames — satisfies this).
	usageScan       []map[string]interface{}
	usageScanParsed bool
	// usageMerged caches the per-dimension high-water merge of every usage
	// object in the buffer (NENYA-167).
	usageMerged     stream.UsageTotals
	usageMergedSeen bool
	// usageClaimed records that this buffer's usage was claimed by
	// recordMCPIterationUsage (one claim per dispatch); the MCP terminal paths
	// check it and are therefore idempotent no-ops for a claimed buffer
	// (NENYA-193).
	usageClaimed bool
}

// mcpToolCall represents a tool call extracted from an upstream LLM response.
type mcpToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	// ExtraContent carries the Gemini thought signature (extra_content) so
	// synthetic assistant tool_calls keep the model's signatures inline
	// (NENYA-51). Nil for providers that do not emit signatures.
	ExtraContent any
}

// usageChunk returns the final SSE data line carrying a usage object over
// the shared single scan (nil = no usage present).
func (b *bufferedSSE) usageChunk() map[string]interface{} {
	chunks := b.usageChunks()
	if len(chunks) == 0 {
		return nil
	}
	return chunks[len(chunks)-1]
}

// usageChunks parses rawBytes ONCE (cached) and returns every data chunk
// carrying a usage object in order — the shared scan behind usageChunk and
// usageTotals, which previously each walked and unmarshaled the same lines
// independently (AUD-82 / NENYA-287).
func (b *bufferedSSE) usageChunks() []map[string]interface{} {
	if b.usageScanParsed {
		return b.usageScan
	}
	b.usageScanParsed = true
	for _, line := range strings.Split(string(b.rawBytes), "\n") {
		if value, isData := stream.SSEDataField([]byte(line)); isData {
			line = string(value)
		}
		line = strings.TrimSpace(line)
		if line == "" || line == stream.DonePayload || !strings.Contains(line, "usage") {
			// Prefilter: content deltas are the bulk of a large buffer; only
			// lines mentioning usage in any spelling (usage, usageMetadata) are
			// worth unmarshaling.
			continue
		}
		var chunk map[string]interface{}
		if err := json.Unmarshal([]byte(line), &chunk); err != nil {
			continue
		}
		// Any accepted placement counts: the OpenAI top-level "usage", the
		// Anthropic "message.usage" and Gemini's "usageMetadata" (NENYA-167).
		if _, hasUsage := stream.UsageMapFromChunk(chunk); hasUsage {
			b.usageScan = append(b.usageScan, chunk)
		}
	}
	return b.usageScan
}

// replayBufferedResponse writes a buffered SSE response to the client.
// This is used during MCP tool call loops to replay the assistant message
// and tool calls before sending tool results.
// convertBufferedForReplay re-renders the buffer's SSE frames into the
// client's wire format when the serving target's format differs from it
// (issue 723f3db1): the chat paths translate every response target→client,
// but the MCP replay wrote the upstream's NATIVE frames verbatim — an OpenAI
// client of an Anthropic-format chain received message_start/
// content_block_delta frames it cannot parse, and an Anthropic client of an
// OpenAI chain received chunks with no message_stop. The same resolver the
// chat streaming path uses picks the transformer; frames are fed in order
// (the transformers are stateful across a stream) and the held tail is
// pulled at the end (stream.TailFinisher, AUD-29). A matching format (or no
// resolver verdict) replays verbatim; a transformer error also falls back to
// the verbatim buffer — replaying what the upstream said — rather than
// dropping the answer.
func (p *Proxy) convertBufferedForReplay(gw *gateway.NenyaGateway, buf *bufferedSSE, sourceFormat string, apiKey *config.ApiKey) []byte {
	if buf == nil || len(buf.rawBytes) == 0 {
		return buf.rawBytes
	}
	target := buf.servingTarget
	if target.Format == "" || target.Format == sourceFormat {
		return buf.rawBytes
	}
	tr := p.resolveTransformer(gw, target, sourceFormat, scopedThoughtSigCache(gw, apiKey))
	if tr == nil {
		return buf.rawBytes
	}
	converted, ok := p.convertReplayFrames(tr, buf.rawBytes)
	if !ok {
		return buf.rawBytes
	}
	return converted
}

// convertReplayFrames drives a stateful transformer over the buffer's SSE
// data frames in order and frames the converted events for the client. A
// transformer error aborts with ok=false (the caller falls back to the
// verbatim buffer).
func (p *Proxy) convertReplayFrames(tr stream.ResponseTransformer, raw []byte) ([]byte, bool) {
	var out []byte
	for _, line := range strings.Split(string(raw), "\n") {
		value, isData := stream.SSEDataField([]byte(line))
		if !isData {
			continue
		}
		payload := strings.TrimSpace(string(value))
		if payload == "" {
			continue
		}
		converted, ok := p.convertReplayFrame(tr, payload)
		if !ok {
			return nil, false
		}
		if converted == stream.DonePayload {
			out = append(out, "data: [DONE]\n\n"...)
			continue
		}
		out = appendSSEEvents(out, []byte(converted))
	}
	out = appendConvertedTail(tr, out)
	return out, true
}

// convertReplayFrame transforms one data payload. The "[DONE]" sentinel and
// a terminator the transformer renders itself (the forward transformer emits
// it on message_stop) both come back as the sentinel string.
func (p *Proxy) convertReplayFrame(tr stream.ResponseTransformer, payload string) (string, bool) {
	// A "[DONE]" payload is fed through like any frame: the transformer
	// decides — a held tail is flushed in its place (the converted Anthropic
	// stream terminates at message_stop), otherwise the sentinel passes
	// through for OpenAI clients.
	converted, err := tr.TransformSSEChunk(context.Background(), []byte(payload))
	if err != nil && !errors.Is(err, stream.ErrEventConsumed) {
		return "", false
	}
	return string(converted), true
}

// appendSSEEvents frames each JSON event of a transformer's (possibly
// multi-event) output as its own "data: " frame.
func appendSSEEvents(out, events []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(events))
	for dec.More() {
		var evt map[string]interface{}
		if err := dec.Decode(&evt); err != nil {
			return out
		}
		reencoded, err := json.Marshal(evt)
		if err != nil {
			return out
		}
		out = append(out, "data: "...)
		out = append(out, reencoded...)
		out = append(out, '\n', '\n')
	}
	return out
}

// appendConvertedTail pulls the transformer's held tail (stream.TailFinisher,
// AUD-29) and appends its events. A transformer without a tail is unchanged.
func appendConvertedTail(tr stream.ResponseTransformer, out []byte) []byte {
	finisher, ok := tr.(stream.TailFinisher)
	if !ok {
		return out
	}
	tail := finisher.FinishTail()
	if len(tail) == 0 {
		return out
	}
	dec := json.NewDecoder(bytes.NewReader(tail))
	for dec.More() {
		var evt map[string]interface{}
		if err := dec.Decode(&evt); err != nil {
			return append(out, tail...)
		}
		reencoded, err := json.Marshal(evt)
		if err != nil {
			return append(out, tail...)
		}
		out = append(out, "data: "...)
		out = append(out, reencoded...)
		out = append(out, '\n', '\n')
	}
	return out
}

func replayBufferedResponse(w http.ResponseWriter, buf *bufferedSSE, gw *gateway.NenyaGateway, providerName string, logger *slog.Logger) {
	if len(buf.rawBytes) == 0 {
		logger.Warn("MCP loop: empty buffered response, sending error")
		writeSSEError(gw, w, http.StatusInternalServerError, "MCP loop: empty response from upstream")
		return
	}

	logger.Info("replaying buffered SSE response",
		"has_content", buf.hasContent,
		"finish_reason", buf.finishReason,
		"raw_bytes_len", len(buf.rawBytes))

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	if fw, ok := newImmediateFlushWriter(w, resolveStreamStallTimeout(gw)); ok {
		replayViaFlushWriter(gw, fw, buf, providerName)
	} else {
		if _, err := w.Write(buf.rawBytes); err != nil {
			return
		}
	}
}

// replayViaFlushWriter writes one replay chunk through the flushing writer and
// classifies a failure: a per-write deadline timeout is client_stalled
// (NENYA-155) — telemetry only, never a provider fault.
func replayViaFlushWriter(gw *gateway.NenyaGateway, fw *immediateFlushWriter, buf *bufferedSSE, providerName string) {
	if _, err := fw.Write(buf.rawBytes); err == nil {
		return
	} else if isWriteTimeout(err) {
		if gw != nil {
			gw.Metrics.RecordClientStalled("", providerName)
		}
	}
}

// writeSSEError sends an SSE error event to the client with the given status code and message.
// The error is formatted as an OpenAI-compatible error object and followed by [DONE].
func writeSSEError(gw *gateway.NenyaGateway, w http.ResponseWriter, statusCode int, message string) {
	// NENYA-155: re-arm the client-stall deadline — a terminal written long
	// after the last streamed chunk must not be silently dropped by the stale
	// deadline, and must stay bounded against a stuck peer.
	armClientWriteDeadline(w, resolveStreamStallTimeout(gw))
	writeGatewayStreamError(w, statusCode, ErrorTypeGateway, message)
}

// sseAccumulator accumulates SSE stream state during buffered reading,
// decomposing the high-cyclomatic-complexity bufferStreamResponse into
// single-responsibility methods.
type sseAccumulator struct {
	raw          strings.Builder
	finishReason string
	hasContent   bool
	content      strings.Builder
	reasoning    strings.Builder
	model        string
	logger       *slog.Logger

	tcArgsAccum map[int]*strings.Builder
	tcNameAccum map[int]string
	tcIDAccum   map[int]string
	// tcExtraAccum carries Gemini thought signatures (extra_content) seen in
	// tool_call deltas so synthetic assistant messages keep them inline
	// (NENYA-51); without this the MCP loop's own history lost the model's
	// signatures and the sanitizer stripped the just-completed tool pairs.
	tcExtraAccum map[int]any
	totalLines   int
	dataLines    int
	// streamError marks an in-stream error frame (NENYA-153).
	streamError bool
	// format is the serving target's wire format (UpstreamTarget.Format).
	// Tool-call extraction is format-aware (AUD-10 / NENYA-218): the OpenAI
	// delta parse always runs, and the Anthropic content_block / Gemini
	// functionCall parsers feed the SAME index-keyed accumulators when the
	// target's format says so — pre-fix, a non-OpenAI-format target's tool
	// calls were invisible to the loop and replayed unexecuted.
	format string
	// geminiToolSeq assigns synthetic indices to Gemini's complete
	// functionCall parts (they arrive whole — no deltas to join).
	geminiToolSeq int
}

func newSSEAccumulator(logger *slog.Logger) *sseAccumulator {
	return &sseAccumulator{
		tcArgsAccum:  make(map[int]*strings.Builder),
		tcNameAccum:  make(map[int]string),
		tcIDAccum:    make(map[int]string),
		tcExtraAccum: make(map[int]any),
		logger:       logger,
	}
}

// processLine handles a single scanner line. Returns true when [DONE] is seen.
func (acc *sseAccumulator) processLine(line string) bool {
	if line == "" {
		return false
	}
	acc.totalLines++

	value, isData := stream.SSEDataField([]byte(line))
	if !isData {
		return false
	}
	acc.dataLines++

	// Tolerant field recognition (NENYA-197): a provider emitting
	// "data:{json}" must not be read as an empty response.
	data := strings.TrimSpace(string(value))
	if data == "" {
		return false
	}

	if data == stream.DonePayload {
		acc.raw.WriteString("data: [DONE]\n\n")
		return true
	}

	acc.processChunk(data)
	switch acc.format {
	case "anthropic":
		acc.processAnthropicData(data)
	case "gemini":
		acc.processGeminiData(data)
	}
	acc.raw.WriteString("data: ")
	acc.raw.WriteString(data)
	acc.raw.WriteString("\n\n")
	return false
}

// processChunk unmarshals a JSON data chunk and extracts model, delta, and
// finish_reason.
func (acc *sseAccumulator) processChunk(data string) {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}

	// NENYA-153: a 200 stream can carry an error frame (a capacity rejection
	// smuggled after the handshake). Flag it here, where the map is already
	// parsed, so the breaker outcome can charge a failure instead of a success.
	// The predicate matches the bootstrap head probe's rejection set
	// (classifyBootstrapLine): an error payload, the OpenAI Responses
	// "response.failed" event, or (NENYA-168) a capacity/overload rejection that
	// is not shaped as an error envelope — with the NENYA-191 exemption: a
	// policy-worded rejection is the model declining the content, not a
	// provider fault, so it is not charged here either.
	policy := stream.IsPolicyRefusalPayload(chunk)
	if !policy && (stream.IsStreamErrorPayload(chunk) || stream.IsResponseFailedEvent(chunk) || stream.IsCapacityRejection(chunk)) {
		acc.streamError = true
	}

	if m := stream.ResponseModelOf(chunk); m != "" {
		acc.model = m
	}

	if choice := extractFirstChoice(chunk); choice != nil {
		if d, ok := choice["delta"].(map[string]any); ok {
			acc.processDelta(d)
		}
		if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
			acc.finishReason = fr
		}
	}

	if acc.finishReason == "" {
		if fr, ok := chunk["finish_reason"].(string); ok && fr != "" {
			acc.finishReason = fr
		}
	}
}

// extractFirstChoice extracts the first choices[0] element from a chunk.
func extractFirstChoice(chunk map[string]any) map[string]any {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return nil
	}
	return choice
}

// processDelta extracts content, reasoning_content, and tool_calls from a
// delta map.
func (acc *sseAccumulator) processDelta(delta map[string]any) {
	if content, ok := delta["content"]; ok && content != nil {
		if s, ok := content.(string); ok && s != "" {
			acc.hasContent = true
			acc.content.WriteString(s)
		}
	}

	if rc, ok := delta["reasoning_content"]; ok && rc != nil {
		if s, ok := rc.(string); ok && s != "" {
			acc.reasoning.WriteString(s)
		}
	}

	if tcSlice, ok := delta["tool_calls"].([]any); ok {
		for _, tcAny := range tcSlice {
			tc, ok := tcAny.(map[string]any)
			if !ok {
				continue
			}
			acc.processToolCallChunk(tc)
		}
	}
}

// processToolCallChunk extracts index, id, name, and arguments from a single
// tool_call delta chunk, accumulating arguments across chunks by index.
func (acc *sseAccumulator) processToolCallChunk(tc map[string]any) {
	// An absent index defaults to 0 (sloppy single-tool emitters); a present
	// but out-of-shape index is dropped, not trusted (NENYA-48 class).
	idx := 0
	if idxVal := tc["index"]; idxVal != nil {
		var ok bool
		if idx, ok = boundedIndex(idxVal); !ok {
			return
		}
	}

	if id, ok := tc["id"].(string); ok && id != "" {
		acc.tcIDAccum[idx] = id
	}

	if extra, ok := tc["extra_content"]; ok {
		acc.tcExtraAccum[idx] = extra
	}

	fn, ok := tc["function"].(map[string]any)
	if !ok {
		return
	}

	if name, ok := fn["name"].(string); ok && name != "" {
		acc.tcNameAccum[idx] = name
	}

	if argsAny, ok := fn["arguments"]; ok {
		argsStr, ok := argsAny.(string)
		if !ok || argsStr == "" {
			return
		}
		if acc.tcArgsAccum[idx] == nil {
			acc.tcArgsAccum[idx] = &strings.Builder{}
		}
		acc.tcArgsAccum[idx].WriteString(argsStr)
	}
}

// processAnthropicData extracts tool calls, text, reasoning, the model name
// and the stop reason from an Anthropic-format SSE data line (AUD-10 /
// NENYA-218). The data line carries its own "type" — the event: line is not
// needed. Everything feeds the same index-keyed accumulators the OpenAI
// parser uses, so buildToolCalls is format-agnostic downstream.
func (acc *sseAccumulator) processAnthropicData(data string) {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}

	msgType, _ := chunk["type"].(string)
	switch msgType {
	case "message_start":
		acc.anthropicMessageStart(chunk)
	case "content_block_start":
		acc.anthropicBlockStart(chunk)
	case "content_block_delta":
		acc.anthropicBlockDelta(chunk)
	case "message_delta":
		acc.anthropicMessageDelta(chunk)
	}
}

// anthropicMessageStart records the serving model's name.
func (acc *sseAccumulator) anthropicMessageStart(chunk map[string]any) {
	msg, ok := chunk["message"].(map[string]any)
	if !ok {
		return
	}
	if model, ok := msg["model"].(string); ok && model != "" {
		acc.model = model
	}
}

// anthropicMessageDelta records the stop reason (e.g. tool_use).
func (acc *sseAccumulator) anthropicMessageDelta(chunk map[string]any) {
	delta, ok := chunk["delta"].(map[string]any)
	if !ok {
		return
	}
	if sr, ok := delta["stop_reason"].(string); ok && sr != "" {
		acc.finishReason = sr
	}
}

// anthropicBlockStart registers a tool_use block (id + name) under its index.
func (acc *sseAccumulator) anthropicBlockStart(chunk map[string]any) {
	idx, ok := boundedIndex(chunk["index"])
	if !ok {
		return
	}
	cb, ok := chunk["content_block"].(map[string]any)
	if !ok {
		return
	}
	if t, _ := cb["type"].(string); t != "tool_use" {
		return
	}
	if id, ok := cb["id"].(string); ok && id != "" {
		acc.tcIDAccum[idx] = id
	}
	if name, ok := cb["name"].(string); ok && name != "" {
		acc.tcNameAccum[idx] = name
	}
	// Anthropic sends the parsed input at start when it is empty and streams
	// the rest via input_json_delta — seed the accumulator so a complete
	// start-frame input survives even with no delta following.
	if input, ok := cb["input"].(map[string]any); ok && len(input) > 0 {
		if encoded, err := json.Marshal(input); err == nil {
			acc.tcArgsAccum[idx] = &strings.Builder{}
			acc.tcArgsAccum[idx].WriteString(string(encoded))
		}
	}
}

// anthropicBlockDelta appends input_json_delta fragments (keyed by block
// index) and text/thinking deltas to the content accumulators.
func (acc *sseAccumulator) anthropicBlockDelta(chunk map[string]any) {
	idx, ok := boundedIndex(chunk["index"])
	if !ok {
		return
	}
	delta, ok := chunk["delta"].(map[string]any)
	if !ok {
		return
	}
	switch t, _ := delta["type"].(string); t {
	case "input_json_delta":
		if frag, ok := delta["partial_json"].(string); ok && frag != "" {
			if acc.tcArgsAccum[idx] == nil {
				acc.tcArgsAccum[idx] = &strings.Builder{}
			}
			acc.tcArgsAccum[idx].WriteString(frag)
		}
	case "text_delta":
		if s, ok := delta["text"].(string); ok && s != "" {
			acc.hasContent = true
			acc.content.WriteString(s)
		}
	case "thinking_delta":
		if s, ok := delta["thinking"].(string); ok && s != "" {
			acc.reasoning.WriteString(s)
		}
	}
}

// processGeminiData extracts tool calls, text, the model version and the
// finish reason from a Gemini-format SSE chunk (AUD-10 / NENYA-218).
// Gemini functionCall parts arrive COMPLETE (name + args in one part — no
// deltas to join); each gets a synthetic index in the shared accumulators.
// A part's thoughtSignature is carried into tcExtraAccum so the NENYA-51
// history preservation covers native-Gemini tool calls too.
func (acc *sseAccumulator) processGeminiData(data string) {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}

	if mv, ok := chunk["modelVersion"].(string); ok && mv != "" {
		acc.model = mv
	}

	candidates, ok := chunk["candidates"].([]any)
	if !ok {
		return
	}
	for _, candAny := range candidates {
		if cand, ok := candAny.(map[string]any); ok {
			acc.geminiCandidate(cand)
		}
	}
}

// geminiCandidate handles one Gemini candidate: its finish reason and its
// content parts.
func (acc *sseAccumulator) geminiCandidate(cand map[string]any) {
	if fr, ok := cand["finishReason"].(string); ok && fr != "" && acc.finishReason == "" {
		acc.finishReason = fr
	}
	content, ok := cand["content"].(map[string]any)
	if !ok {
		return
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return
	}
	for _, partAny := range parts {
		if part, ok := partAny.(map[string]any); ok {
			acc.geminiPart(part)
		}
	}
}

// geminiPart handles one content part: text accumulation and a complete
// functionCall (synthetic index in the shared accumulators).
func (acc *sseAccumulator) geminiPart(part map[string]any) {
	if text, ok := part["text"].(string); ok && text != "" {
		acc.hasContent = true
		acc.content.WriteString(text)
	}
	fc, ok := part["functionCall"].(map[string]any)
	if !ok {
		return
	}
	name, _ := fc["name"].(string)
	if name == "" {
		return
	}
	idx := acc.geminiToolSeq
	acc.geminiToolSeq++
	acc.tcNameAccum[idx] = name
	// No-args calls (common for parameterless Gemini tools) must carry an
	// empty-object argument JSON, not a nil accumulator (which the history
	// builder would render as "arguments":"null").
	if args, ok := fc["args"].(map[string]any); ok && len(args) > 0 {
		if encoded, err := json.Marshal(args); err == nil {
			acc.tcArgsAccum[idx] = &strings.Builder{}
			acc.tcArgsAccum[idx].WriteString(string(encoded))
		}
	} else {
		acc.tcArgsAccum[idx] = &strings.Builder{}
		acc.tcArgsAccum[idx].WriteString("{}")
	}
	if sig, ok := part["thoughtSignature"]; ok && sig != nil {
		acc.tcExtraAccum[idx] = sig
	}
}

// boundedIndex converts a JSON index value (float64) guarding against
// hostile values (negative, fractional, astronomically large — the NENYA-48
// class): out-of-shape indices are dropped rather than trusted.
func boundedIndex(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f < 0 || f != float64(int(f)) || f > 1e9 {
		return 0, false
	}
	return int(f), true
}

// buildToolCalls assembles the final tool call list from accumulators.
func (acc *sseAccumulator) buildToolCalls() []mcpToolCall {
	indices := make([]int, 0, len(acc.tcNameAccum))
	for idx := range acc.tcNameAccum {
		indices = append(indices, idx)
	}
	sort.Ints(indices)

	calls := make([]mcpToolCall, 0, len(indices))
	for _, idx := range indices {
		name := acc.tcNameAccum[idx]
		if name == "" {
			continue
		}
		id := acc.tcIDAccum[idx]
		if id == "" {
			id = util.SyntheticToolCallID(idx)
		}
		var args map[string]any
		if argsBuilder := acc.tcArgsAccum[idx]; argsBuilder != nil {
			argsStr := argsBuilder.String()
			if err := json.Unmarshal([]byte(argsStr), &args); err != nil {
				if acc.logger != nil {
					acc.logger.Warn("MCP: failed to parse tool arguments, calling with empty args",
						"tool", name, "err", err)
				}
				args = make(map[string]any)
			}
		}
		calls = append(calls, mcpToolCall{ID: id, Name: name, Arguments: args, ExtraContent: acc.tcExtraAccum[idx]})
	}
	return calls
}

// result produces the final bufferedSSE from accumulated state.
func (acc *sseAccumulator) result() *bufferedSSE {
	if acc.dataLines == 0 && acc.totalLines > 0 {
		if acc.logger != nil {
			acc.logger.Warn("MCP stream: no SSE 'data:' lines found; possible non-SSE response",
				"total_lines", acc.totalLines)
		}
	}

	toolCalls := acc.buildToolCalls()
	reasoningStr := acc.reasoning.String()

	var assistantMsg map[string]any
	if len(toolCalls) > 0 {
		assistantMsg = map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": buildOpenAIToolCalls(toolCalls),
		}
		if reasoningStr != "" {
			assistantMsg["reasoning_content"] = reasoningStr
		}
	} else if acc.hasContent {
		assistantMsg = map[string]any{
			"role":    "assistant",
			"content": acc.content.String(),
		}
		if reasoningStr != "" {
			assistantMsg["reasoning_content"] = reasoningStr
		}
	}

	return &bufferedSSE{
		rawBytes:         []byte(acc.raw.String()),
		toolCalls:        toolCalls,
		assistantMessage: assistantMsg,
		finishReason:     acc.finishReason,
		hasContent:       acc.hasContent,
		model:            acc.model,
		providerName:     "",
		reasoningContent: reasoningStr,
		streamError:      acc.streamError,
		emptyResponse:    acc.dataLines == 0,
	}
}

func bufferStreamResponse(ctx context.Context, r io.Reader, logger *slog.Logger, servingModel string, servingTarget routing.UpstreamTarget) (*bufferedSSE, error) {
	acc := newSSEAccumulator(logger)
	// AUD-10 (NENYA-218): tool-call extraction follows the serving target's
	// wire format — an Anthropic/Gemini-format target's tool calls must be
	// extracted or the loop replays them unexecuted.
	acc.format = servingTarget.Format
	scanner := bufio.NewScanner(r)
	// The shared SSE splitter (LF, CRLF or lone CR) so non-canonical framing is
	// parsed per line instead of as one glued line (NENYA-197).
	scanner.Split(stream.SplitSSELines)
	scanner.Buffer(make([]byte, stream.SSEScannerInitialBuf), stream.SSEScannerMaxBuf)
	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		if acc.processLine(scanner.Text()) {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		if err == bufio.ErrTooLong {
			return nil, fmt.Errorf("MCP SSE line exceeded buffer limit: %w", err)
		}
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}
	out := acc.result()
	out.calibrationModel = servingModel
	out.servingTarget = servingTarget
	// NENYA-155: the replay write's client-stall telemetry is labeled with
	// the producing provider.
	out.providerName = servingTarget.Provider
	return out, nil
}

// buildOpenAIToolCalls converts internal mcpToolCall structs to OpenAI function call format.
func buildOpenAIToolCalls(calls []mcpToolCall) []any {
	result := make([]any, 0, len(calls))
	for _, call := range calls {
		argsBytes, _ := json.Marshal(call.Arguments)
		tc := map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(argsBytes),
			},
		}
		if call.ExtraContent != nil {
			// NENYA-51: keep the model's thought signature inline so the
			// Gemini sanitizer sees it and does not strip the tool pair.
			tc["extra_content"] = call.ExtraContent
		}
		result = append(result, tc)
	}
	return result
}

// partitionMCPToolCalls splits tool calls into MCP and non-MCP categories.
// This is used to route MCP tools to the MCP server while passing other tools upstream.
func partitionMCPToolCalls(calls []mcpToolCall, toolIndex *mcp.ToolRegistry) (mcpCalls, nonMcpCalls []mcpToolCall) {
	for _, call := range calls {
		if toolIndex.IsMCPTool(call.Name) {
			mcpCalls = append(mcpCalls, call)
		} else {
			nonMcpCalls = append(nonMcpCalls, call)
		}
	}
	return
}

// mcpExecOptions groups the MCP tool-call execution inputs (NENYA-162): the
// session's unavailable-tool set is set only by the MCP loop (nil elsewhere —
// the NENYA-162 non-injected-tool guard is then inert).
type mcpExecOptions struct {
	ctx         context.Context
	calls       []mcpToolCall
	gw          *gateway.NenyaGateway
	agentName   string
	canary      pipeline.CanarResult
	screen      *egressScreenRuntime
	apiKey      *config.ApiKey
	unavailable map[string]bool
}

// executeMCPCallsWithUnavailable executes MCP tool calls concurrently using the registered MCP servers.
// When the canary tripwire is armed, call arguments are scanned first:
// a canary hit means the model reproduced gateway-injected context into
// an outbound tool call — the signature of injection-driven exfiltration.
// Block action refuses the call with an error result; log action allows
// it after recording. When the egress screen is enabled, tool arguments
// flagged by a deterministic signal (canary hit, unscannable arguments,
// or argument-guard violations) get one advisory adjudication per
// request (shared budget); an exfil verdict under strict action refuses
// the call. Returns a slice of tool results in the same order
// as the input calls.
func executeMCPCallsWithUnavailable(in mcpExecOptions) []*mcp.CallToolResult {
	ctx, calls, gw, agentName := in.ctx, in.calls, in.gw, in.agentName
	canary, screen, apiKey, unavailable := in.canary, in.screen, in.apiKey, in.unavailable
	if len(calls) == 0 {
		return nil
	}

	results := make([]*mcp.CallToolResult, len(calls))
	var wg sync.WaitGroup

	for i, call := range calls {
		wg.Add(1)
		go func(idx int, c mcpToolCall) {
			defer wg.Done()

			logger := gw.Logger
			if logger == nil {
				logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			}

			ctxLogger := logger.With(
				"mcp_operation", "tool_call",
				"agent", agentName,
				"mcp_call_id", c.ID,
			)

			start := time.Now()

			// Canary tripwire on the tool-args egress channel: scan
			// BEFORE dispatching the call to the MCP server. Block
			// action refuses the call (unscannable arguments fail
			// closed); log action records and allows. The scan runs even
			// for a non-injected tool (below): the arguments are still an
			// egress channel worth recording.
			tripped, refuse := scanToolArgsCanary(canary, c.Arguments, gw.Metrics, ctxLogger, c.Name)
			if refuse {
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "[blocked by egress policy: canary detected in tool arguments]"}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall("unknown", c.Name, agentName, time.Since(start), errMCPCanaryBlock)
				return
			}

			route, ok := gw.MCPToolIndex.Lookup(c.Name)
			if !ok {
				ctxLogger.Warn("MCP tool call failed: unknown tool", "tool", c.Name)
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "unknown MCP tool: " + c.Name}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall("unknown", c.Name, agentName, time.Since(start), fmt.Errorf("unknown tool"))
				return
			}

			ctxLogger = ctxLogger.With(
				"mcp_server", route.ServerName,
				"mcp_tool", route.MCPToolName,
			)

			// NENYA-183: dispatch is bounded by the agent's own server list,
			// the caller's tool grant, and the server's credential health.
			if refusal, reason, refusalKind := mcpDispatchRefusal(gw, agentName, route.ServerName, route.MCPToolName, apiKey); refusal != "" {
				ctxLogger.Warn("MCP tool call refused", "reason", reason)
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: refusal}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall(route.ServerName, c.Name, agentName, time.Since(start), refusalKind)
				return
			}

			// NENYA-162 loop correctness: a model naming an MCP tool the
			// session did not inject (cut by the server's tool policy or by
			// relevance-based selection) is refused with a STRUCTURED error
			// result the model can act on — never a silent drop, never an
			// upstream dispatch on a guessed name. unavailable is nil for
			// callers outside the MCP loop (the guard is then inert). The
			// refusal is counted by the selection metric, not as a server
			// call: nothing was dispatched.
			if unavailable[c.Name] {
				ctxLogger.Warn("MCP tool call refused: not injected in this session",
					"tool", c.Name, "agent", agentName)
				gw.Metrics.RecordMCPToolSearch("noninjected_call_refused")
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "[unavailable tool: " + c.Name + "]"}},
					IsError: true,
				}
				return
			}

			client := gw.MCPClients[route.ServerName]
			if client == nil {
				ctxLogger.Warn("MCP tool call failed: server not available")
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "MCP tool call refused: server unavailable"}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall(route.ServerName, c.Name, agentName, time.Since(start), fmt.Errorf("server not available"))
				return
			}

			toolCtx, cancel := context.WithTimeout(ctx, mcpExecTimeout)
			defer cancel()

			// Tool-argument guard (NENYA-78): schema validation, size
			// cap, and URL destination policy — the last line before a
			// model-controlled argument reaches an MCP server.
			violations, rejected := mcp.ValidateArgs(c.Arguments, route.Schema, mcp.GuardConfigFromServer(
				gw.Config.Governance.MCPGuard,
				gw.Config.MCPServers[route.ServerName].AllowedHosts,
			))
			if len(violations) > 0 && !rejected {
				// log policy: record the flags and let the call proceed.
				ctxLogger.Warn("MCP tool arguments flagged by policy (log action)",
					"violations", len(violations), "detail", mcp.SummarizeArgViolations(violations))
			}
			if rejected {
				summary := mcp.SummarizeArgViolations(violations)
				ctxLogger.Warn("MCP tool call rejected by argument guard",
					"violations", len(violations), "detail", summary)
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "[blocked by argument policy: " + summary + "]"}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall(route.ServerName, route.MCPToolName, agentName, time.Since(start), errMCPArgumentPolicy)
				return
			}

			// Egress screen on the tool-args channel: advisory second
			// opinion when a deterministic signal fired (canary hit or
			// argument-guard flags; unscannable arguments are already
			// refused or unadjudicable). One judgment per request;
			// strict action refuses the call.
			screenTriggered := tripped || len(violations) > 0
			if screen.screenToolArgs(ctx, gw, agentName, c.Arguments, screenTriggered) {
				results[idx] = &mcp.CallToolResult{
					Content: []mcp.ContentBlock{{Type: "text", Text: "[blocked by egress policy: data exfiltration detected in tool arguments]"}},
					IsError: true,
				}
				gw.Metrics.RecordMCPToolCall(route.ServerName, route.MCPToolName, agentName, time.Since(start), errMCPEgressScreen)
				return
			}

			result, err := client.CallTool(toolCtx, route.MCPToolName, c.Arguments)
			duration := time.Since(start)
			results[idx] = recordMCPCallOutcome(gw, ctxLogger, route, result, err, agentName, duration)
		}(i, call)
	}

	wg.Wait()
	return results
}

// recordMCPCallOutcome turns one dispatched tool call into its result and
// records the outcome: a refused credential marks the server unavailable so
// later requests fail closed instead of degrading to per-call errors, and a
// successful call re-proves the credential (NENYA-183).
func recordMCPCallOutcome(gw *gateway.NenyaGateway, ctxLogger *slog.Logger, route mcp.ToolRouting, result *mcp.CallToolResult, err error, agentName string, duration time.Duration) *mcp.CallToolResult {
	if err != nil {
		if errors.Is(err, mcp.ErrAuthRejected) {
			// A refused credential (a token that expired or was revoked
			// mid-session) takes the server out of service so later requests
			// fail closed instead of degrading to per-call errors. The mark is
			// transient: it expires so a restored credential is re-probed
			// (NENYA-183). A 403 may be a single tool's ACL, which the
			// cooldown bounds.
			if _, alreadyMarked := gw.MCPUnavailableReason(route.ServerName); !alreadyMarked {
				ctxLogger.Warn("MCP server refused the configured credential, marking it unusable",
					"server", route.ServerName, "cooldown", gateway.MCPCredentialRejectionCooldown().String())
				gw.Metrics.IncMCPCredentialRejection(route.ServerName)
			}
			gw.MarkMCPCredentialRejected(route.ServerName, "credential rejected")
		}
		ctxLogger.Warn("MCP tool call failed",
			"err", err,
			"duration_ms", duration.Milliseconds())
		gw.Metrics.RecordMCPToolCall(route.ServerName, route.MCPToolName, agentName, duration, err)
		return &mcp.CallToolResult{
			Content: []mcp.ContentBlock{{Type: "text", Text: fmt.Sprintf("MCP tool call failed: %v", err)}},
			IsError: true,
		}
	}
	gw.ClearMCPUnavailable(route.ServerName)
	gw.Metrics.RecordMCPToolCall(route.ServerName, route.MCPToolName, agentName, duration, err)
	textLen := 0
	if result != nil {
		textLen = len(result.Text())
	}
	ctxLogger.Debug("MCP tool call completed",
		"duration_ms", duration.Milliseconds(),
		"result_bytes", textLen)
	return result
}

// mcpDispatchRefusal returns the model-facing refusal text and the operator
// reason for a tool dispatch that is not allowed, or ("", "") when the call
// may proceed (NENYA-183): the server must be in the agent's own MCP
// configuration (the registry is global), the caller's tool grant must cover
// the tool, and the server must not be marked unusable for a credential
// reason. The texts share one shape and name no server or header, so a model
// cannot echo configured server names to the client.
func mcpDispatchRefusal(gw *gateway.NenyaGateway, agentName, serverName, toolName string, apiKey *config.ApiKey) (string, string, error) {
	if !gw.AgentHasMCPServer(agentName, serverName) {
		return "MCP tool call refused: server not configured for this agent",
			"server not configured for this agent", errMCPNotGranted
	}
	if !auth.AuthorizeMCPTool(apiKey, serverName, toolName) {
		return "MCP tool call refused: not permitted for this API key",
			"outside the key's tool grant", errMCPNotGranted
	}
	if reason, unavailable := gw.MCPUnavailableReason(serverName); unavailable {
		return "MCP tool call refused: server unusable", reason, errMCPServerUnavailable
	}
	return "", "", nil
}

// errMCPNotGranted is the sentinel recorded when a model asks for an MCP tool
// outside the authenticated key's grant (NENYA-183).
var errMCPNotGranted = errors.New("tool outside the key's grant")

// errMCPServerUnavailable marks a dispatch refused because the server is
// recorded as unusable for a credential reason (NENYA-183).
var errMCPServerUnavailable = errors.New("MCP server unusable")

// errMCPArgumentPolicy is the sentinel returned when the argument
// guard rejects a dispatch (any MCP path).
var errMCPArgumentPolicy = errors.New("argument policy")

// errMCPCanaryBlock marks a tool call refused by the canary tripwire.
var errMCPCanaryBlock = errors.New("canary block")

// errMCPEgressScreen marks a tool call refused by the advisory egress
// screen (distinct from the deterministic argument guard).
var errMCPEgressScreen = errors.New("egress screen")

// mcpGuardDispatch groups the identity and payload of a non-registry
// MCP dispatch (auto-search, auto-save) for the argument guard.
type mcpGuardDispatch struct {
	ServerName string
	ToolName   string
	Purpose    string
	Args       map[string]any
	// ApiKey is the authenticated key whose tool grant must cover the
	// dispatch (NENYA-183). Nil denies (fail closed): auto-search and
	// auto-save always run on behalf of an authenticated request.
	ApiKey *config.ApiKey
}

// guardMCPArgs runs the tool-argument guard for dispatches that are not
// routed through the tool registry (auto-search, auto-save): size cap
// and URL policy apply; schema validation is skipped (no declared
// schema at these call sites). Rejections log once; log-policy flags
// are recorded and allowed. The caller turns rejections into its own
// failure semantics (auto-search failure, auto-save fallback) and
// records its own outcome metric.
// guardMCPArgs returns nil when the dispatch is allowed, or the sentinel that
// names why it was refused (errMCPNotGranted, errMCPArgumentPolicy) so callers
// can record the right outcome.
func guardMCPArgs(gw *gateway.NenyaGateway, logger *slog.Logger, d mcpGuardDispatch) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	// NENYA-183: auto-search and auto-save are tool dispatches too, so the
	// caller's grant caps them as well.
	if !auth.AuthorizeMCPTool(d.ApiKey, d.ServerName, d.ToolName) {
		logger.Warn("MCP dispatch refused: outside the key's grant",
			"purpose", d.Purpose, "server", d.ServerName, "tool", d.ToolName)
		return errMCPNotGranted
	}
	if reason, unavailable := gw.MCPUnavailableReason(d.ServerName); unavailable {
		logger.Warn("MCP dispatch refused: server unusable",
			"purpose", d.Purpose, "server", d.ServerName, "tool", d.ToolName, "reason", reason)
		return errMCPServerUnavailable
	}
	cfg := mcp.GuardConfigFromServer(gw.Config.Governance.MCPGuard, gw.Config.MCPServers[d.ServerName].AllowedHosts)
	violations, rejected := mcp.ValidateArgs(d.Args, nil, cfg)
	if len(violations) > 0 {
		summary := mcp.SummarizeArgViolations(violations)
		if rejected {
			logger.Warn("MCP dispatch rejected by argument guard",
				"purpose", d.Purpose, "server", d.ServerName, "tool", d.ToolName, "detail", summary)
			return errMCPArgumentPolicy
		}
		// log policy: flag and allow.
		logger.Warn("MCP dispatch flagged by argument policy (log action)",
			"purpose", d.Purpose, "server", d.ServerName, "tool", d.ToolName, "detail", summary)
	}
	return nil
}

// mcpAppendContext groups what appendMCPResults needs beyond the payload, the
// calls, and the results themselves.
type mcpAppendContext struct {
	settings  pipeline.SpotlightSettings
	metrics   *infra.Metrics
	toolIndex *mcp.ToolRegistry
	// vision gates the result's image blocks on the routed target (NENYA-149);
	// the zero gate drops them.
	vision mcpVisionGate
	logger *slog.Logger
}

// applySpotlightToParts applies the untrusted-content spotlight envelope to a
// tool result's content array (NENYA-149): only its text parts are enveloped —
// an image part is binary and has no envelope home. Returns whether any part
// was actually enveloped, so the spotlight counter does not fire for an
// image-only result.
func applySpotlightToParts(content any, settings pipeline.SpotlightSettings, server, tool string) (any, bool) {
	parts, ok := content.([]any)
	if !ok {
		return content, false
	}
	enveloped := false
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if part["type"] != "text" {
			continue
		}
		text, _ := part["text"].(string)
		part["text"] = settings.Apply(text, pipeline.SpotlightSourceTool(server, tool))
		enveloped = true
	}
	return parts, enveloped
}

// buildToolResultMessage formats one MCP call/result pair as an OpenAI tool
// message: the tool text (spotlight-enveloped), plus the result's image blocks
// gated on the routed target (NENYA-149).
func buildToolResultMessage(call mcpToolCall, result *mcp.CallToolResult, opts mcpAppendContext) map[string]any {
	settings, metrics, toolIndex := opts.settings, opts.metrics, opts.toolIndex
	content := result.Text()
	if result.IsError {
		content = "[MCP Error] " + content
	}
	// NENYA-149: image blocks ride only when the routed model and target
	// format can carry them; rejected images are dropped with a counter and
	// a debug log, and the omission is made visible in the tool text.
	contentAny, drops := toolResultContent(content, result, opts.vision, opts.logger)
	recordVisionDrops(metrics, drops)
	server, tool := "unknown", call.Name
	// The registry is the source of truth for the (server, tool) pair: the
	// advertised name is sanitized and may be disambiguated, so re-splitting
	// it would mislabel the spotlight source (NENYA-172).
	if toolIndex != nil {
		if route, ok := toolIndex.Lookup(call.Name); ok {
			server, tool = route.ServerName, route.MCPToolName
		}
	}
	if server == "unknown" {
		if s, t, ok := mcp.ParseMCPCall(call.Name); ok {
			server, tool = s, t
		}
	}
	if text, ok := contentAny.(string); ok {
		contentAny = settings.Apply(text, pipeline.SpotlightSourceTool(server, tool))
		if settings.Enabled {
			metrics.RecordSpotlighted("mcp:" + server)
		}
	} else {
		var enveloped bool
		contentAny, enveloped = applySpotlightToParts(contentAny, settings, server, tool)
		if settings.Enabled && enveloped {
			metrics.RecordSpotlighted("mcp:" + server)
		}
	}
	return map[string]any{
		"role":         "tool",
		"tool_call_id": call.ID,
		"content":      contentAny,
	}
}

// appendMCPResults appends MCP tool results to the request payload's messages array.
// The results are formatted as OpenAI tool messages with the corresponding tool call IDs.
// appendMCPResults appends the assistant tool-call message and one tool
// result message per call to the payload's conversation, applying
// spotlight enveloping to result content. It returns the appended message
// maps (the same maps now referenced by the payload) so the MCP loop can
// re-scan them through the deterministic security interceptors (NENYA-136).
func appendMCPResults(payload map[string]any, calls []mcpToolCall, results []*mcp.CallToolResult, assistantMsg map[string]any, opts mcpAppendContext) []map[string]any {
	if len(calls) == 0 || len(results) == 0 {
		return nil
	}

	messages, ok := payload["messages"].([]any)
	if !ok {
		return nil
	}

	var appended []map[string]any

	// Inject assistant message if provided
	if assistantMsg != nil {
		messages = append(messages, assistantMsg)
		appended = append(appended, assistantMsg)
	}

	var toolResults []any
	for i, call := range calls {
		if i >= len(results) || results[i] == nil {
			continue
		}
		toolResult := buildToolResultMessage(call, results[i], opts)
		toolResults = append(toolResults, toolResult)
		appended = append(appended, toolResult)
	}

	newMessages := make([]any, 0, util.AddCap(len(messages), len(toolResults)))
	newMessages = append(newMessages, messages...)
	newMessages = append(newMessages, toolResults...)

	payload["messages"] = newMessages
	return appended
}
