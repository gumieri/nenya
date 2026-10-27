package stream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
)

// ErrEventConsumed signals that the transformer consumed the SSE event
// and no output should be forwarded to the client.
var ErrEventConsumed = errors.New("sse event consumed by transformer")

// ErrTransformedSizeExceeded is returned when the accumulated transformed
// SSE output exceeds the maximum allowed size.
var ErrTransformedSizeExceeded = errors.New("transformed SSE output size exceeded")

const (
	SSEScannerInitialBuf = 64 * 1024
	SSEScannerMaxBuf     = 1024 * 1024
	// maxTransformedBytes caps the accumulated transformed output to
	// prevent memory exhaustion from malicious or buggy upstreams that send
	// many small events producing unbounded transformed output.
	DefaultMaxTransformedEventBytes = 50 * 1024 * 1024
)

// SplitSSELines is the split function for every SSE scanner in Nenya (the
// transforming reader, the buffered MCP path, the stream-head probe): it splits
// on LF, CRLF and a lone CR — all three are legal SSE line terminators (WHATWG)
// — and returns the line without its terminator. bufio.ScanLines only splits on
// LF, so a CR-only stream would otherwise be read as one glued line and a
// [DONE] inside it would never be recognized (NENYA-197).
//
// A CR at the end of the buffered data is held back until the next read, so a
// CRLF split across read-chunk boundaries is never mistaken for a lone CR (the
// line is therefore not delivered until the next byte or EOF — the same
// requirement ScanLines has for LF). A raw CR inside a payload is a line
// terminator by the same rule; the payload bytes are re-emitted inside the same
// event with LF in place of CR.
func SplitSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	// The searches are bounded by the line, never by the whole buffer: the
	// scanner re-invokes the split function on the remaining buffer for every
	// line, so scanning past the current line would make a pure-LF stream
	// quadratic in lines per buffer.
	if nl := bytes.IndexByte(data, '\n'); nl >= 0 {
		if cr := bytes.IndexByte(data[:nl], '\r'); cr >= 0 {
			if cr == nl-1 {
				return nl + 1, data[:cr], nil // CRLF
			}
			return cr + 1, data[:cr], nil // lone CR before the LF
		}
		return nl + 1, data[:nl], nil
	}
	// No LF in the buffer: the CR scan is bounded by the line itself.
	cr := bytes.IndexByte(data, '\r')
	switch {
	case cr < 0:
	case cr+1 < len(data), atEOF:
		return cr + 1, data[:cr], nil
	default:
		// A CR at the buffer edge: wait for the next read to learn whether it
		// is a CRLF.
		return 0, nil, nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// SSEDataField reports whether line is a data field and returns its value, with
// the single optional leading space the SSE grammar allows removed (WHATWG
// strips at most one U+0020; a tab after the colon is part of the value).
// Tolerance (NENYA-197): a missing space ("data:{\"x\":1}") is accepted,
// because real providers emit it and a strict parser silently drops the whole
// field — the client then receives a stream with no content and the stream
// filters never see the payload. Field names are case-sensitive, so "DATA:" is
// not a data field. Frames the reader re-serializes or injects use the canonical
// "data: " form; a passthrough line keeps its original spelling.
func SSEDataField(line []byte) ([]byte, bool) {
	if !bytes.HasPrefix(line, sseDataPrefix) {
		return nil, false
	}
	value := line[len(sseDataPrefix):]
	if len(value) > 0 && value[0] == ' ' {
		value = value[1:]
	}
	return value, true
}

// sseDataPrefix is hoisted so the hot path does not convert the constant on
// every SSE line.
var sseDataPrefix = []byte("data:")

// canonicalDoneFrame is the canonical [DONE] frame the reader forwards. It is a
// constant so no caller can mutate the shared bytes.
// DonePayload is the OpenAI SSE terminal sentinel carried in a data line.
const DonePayload = "[DONE]"

// IsDonePayload reports whether v is the OpenAI terminal sentinel,
// tolerating surrounding whitespace (the canonical frame is exactly
// DonePayload; the same tolerance as the field recognition, NENYA-197).
func IsDonePayload(v []byte) bool {
	return bytes.Equal(bytes.TrimSpace(v), []byte(DonePayload))
}

const canonicalDoneFrame = "data: " + DonePayload

// ResponseTransformer transforms one SSE data payload. TransformSSEChunk
// receives the JSON payload of a data: line (without the field prefix) and
// returns the converted payload: a single JSON object (any shape) or several
// complete JSON objects concatenated back to back. The reader frames the
// result — each top-level object becomes its own SSE event (with an `event:`
// line when the object carries a type field, AUD-12); a transformer must NOT
// pre-frame its output with data:/event: lines.
type ResponseTransformer interface {
	TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error)
}

// TailFinisher is an optional ResponseTransformer extension for stateful
// transformers that HOLD their closing events past the finish chunk
// (AUD-29 / NENYA-241): the OpenAI→Anthropic converter withholds
// message_delta/message_stop so the standard `stream_options.include_usage`
// ordering — the trailing usage-only chunk after the finish chunk — lands in
// the published usage instead of a 0/0 delta. The reader pulls the tail at
// the stream terminal ([DONE] or injected end-of-stream) and composes it
// into the final frame sequence.
type TailFinisher interface {
	// FinishTail returns the held closing events, or nil when nothing is
	// held. Calling it twice must return nil the second time.
	FinishTail() []byte
}

// UsageCallback is invoked by the SSE transformer when usage metadata is received.
// UsageCallback receives token usage statistics from the stream.
// Parameters are deltas since the last callback (each field >= 0).
type UsageCallback func(UsageData)

type UsageData struct {
	CompletionTokens    int
	PromptTokens        int
	TotalTokens         int
	CacheHitTokens      int
	CacheMissTokens     int
	CacheCreationTokens int
	ReasoningTokens     int
	// AmbiguousCacheSemantics reports that the source usage object carried an
	// OpenAI-style input total together with the Anthropic-native cache field
	// names and no cache decomposition, so the cache semantics were inferred
	// (NENYA-167). Observability only: it does not change the deltas.
	AmbiguousCacheSemantics bool
	// Model is the model string the upstream reported on the usage-bearing
	// chunk (empty when the provider reports none). Silent-substitution
	// detection compares it with the routed model (NENYA-173).
	Model string
}

type ContentCallback func(content string)

type ThinkingCallback func(active bool)

// FirstOutputCallback is the one-shot notification fired at the first
// output-bearing frame of a stream (NENYA-178). See SetOnFirstEffectiveOutput.
type FirstOutputCallback func()

// SSEObserver receives notifications about SSE events during streaming.
// Observers are called after transformation, so they see what the client receives;
// the Raw line is the PRE-filter upstream bytes when a filter rewrote the chunk
// (Data carries the post-filter text).
type SSEObserver interface {
	// OnSSEEvent is called for each SSE event (data line, [DONE], error, etc.)
	OnSSEEvent(event SSEEvent)
	// OnStreamClose is called when the stream ends (with any error, or nil on clean EOF)
	OnStreamClose(err error)
}

// SSEEvent represents a single SSE event.
type SSEEvent struct {
	ID   string
	Type string // "content", "usage", "tool_call", "done", "error"
	Data map[string]interface{}
	Raw  []byte
	// GatewayInjected marks events synthesized by the gateway itself (e.g. a
	// gateway_error injected when an upstream stream ends without [DONE]).
	// Observers use this to distinguish gateway failures from real provider
	// errors, and should not attribute them to the provider.
	GatewayInjected bool
}

// SSETransformingReader reads from an SSE source and transforms SSE data lines.
type SSETransformingReader struct {
	src         io.Reader
	scanner     *bufio.Scanner
	transformer ResponseTransformer
	onUsage     UsageCallback
	onContent   ContentCallback
	onThinking  ThinkingCallback
	// onEmptyStream, when set, is invoked when the scanner ends cleanly with
	// NO content at all — the empty-200 degradation signal (NENYA-160). Set
	// via SetEmptyStreamCallback.
	onEmptyStream func()
	// onFirstOutput fires once, at the first output-bearing frame (NENYA-178):
	// the TTFT clock's stop signal. See SetOnFirstEffectiveOutput.
	onFirstOutput        FirstOutputCallback
	ttftIncludeReasoning bool
	observer             SSEObserver
	streamFilter         *StreamFilter
	exfilGuard           *ExfilGuard
	// dropPrivateEvents (NENYA-174): when set, SSE frames whose parsed
	// event type names a provider-private/telemetry channel are suppressed
	// before any downstream surface.
	dropPrivateEvents bool
	// sawResumableStop (NENYA-174): set when any parsed frame carries a
	// resumable stop reason (pause_turn). Detected on the RAW surface —
	// format transformers erase the reason before capture — and consulted
	// by the response cache so a paused turn is never stored as complete.
	sawResumableStop    bool
	canaryWatcher       *CanaryWatcher
	streamEntropyFilter *StreamEntropyFilter
	// lastSurface is the wire shape of the last content-bearing chunk,
	// recorded so the exfil guard's terminal flush (NENYA-205) can
	// synthesize the held tail format-identically to the stream.
	lastSurface exfilSurface
	buffer      []byte
	pos         int
	err         error
	closed      bool
	sawDone     bool
	// lastResponseModel is the last model an upstream chunk reported (any
	// chunk, not only usage-bearing ones — aggregators often omit it on
	// the final usage-only chunk). Fed to the usage callback for
	// silent-substitution detection (NENYA-173).
	lastResponseModel string
	// sawContent is the terminal-state notion of content (see
	// trackStreamProgress), deliberately narrower than HasEffectiveOutput.
	sawContent      bool
	sawFinishReason bool
	// sawAnthropicFinish records that the stream COMPLETED through the native
	// Anthropic surface (message_delta stop_reason / message_stop). Set only
	// on the passthrough path (nil transformer — the transformed bytes ARE
	// the raw upstream wire); converted paths end through their transformer's
	// synthesized [DONE] and never consult it. handleScannerDone uses it to
	// end an Anthropic passthrough exactly where the upstream ended instead
	// of appending OpenAI framing to the client's own message_stop (AUD-01).
	// A completion recognized from an OpenAI finish_reason line — even on a
	// stream that also carried Anthropic deltas — keeps the injected [DONE].
	sawAnthropicFinish bool
	// sawConvertedAnthropicFinish records that a CONVERTED stream (OpenAI→
	// Anthropic transformer installed) completed through the transformer's
	// synthesized Anthropic terminal (message_delta stop_reason / message_stop
	// — AUD-12). Same EOF rule as the passthrough flag: appending the OpenAI
	// sentinel data: [DONE] onto an Anthropic client's wire after its own
	// message_stop is an unparseable frame (AUD-12 review, mirroring AUD-01).
	sawConvertedAnthropicFinish bool
	// sawAnthropicContent records that generated content arrived through the
	// Anthropic envelope (passthrough). The NENYA-46 synthetic finish_reason
	// is an OpenAI-shaped frame and must never be synthesized onto an
	// Anthropic-shaped stream, so synthesizeTerminalFinish stands down when
	// this is set.
	sawAnthropicContent bool
	// sawRawAnthropicDelta records that a raw Anthropic message_delta chunk
	// was seen (any surface) — the terminal-sequence evidence the
	// pre-terminal exfil drain requires before honoring a message_stop
	// (AUD-02: a spoofed bare message_stop must not drain early).
	sawRawAnthropicDelta bool
	injectedError        bool
	ctx                  context.Context
	poolBuf              *[]byte
	logger               *slog.Logger

	// last* fields track token usage between consecutive usage chunks.
	// They are accessed only from the single goroutine calling Read().
	// No mutex needed; callers must not call Read() concurrently.
	lastCompletionTokens    int
	lastPromptTokens        int
	lastTotalTokens         int
	lastCacheHitTokens      int
	lastCacheMissTokens     int
	lastCacheCreationTokens int
	lastReasoningTokens     int

	// tcState tracks tool call state across stream chunks.
	tcState toolCallState

	// transformedBytes tracks accumulated output size across the stream.
	// When it exceeds maxTransformedBytes, discarding is set to true.
	transformedBytes uint64
	// maxTransformedBytes caps the accumulated transformed output to
	// prevent memory exhaustion from malicious or buggy upstreams.
	maxTransformedBytes int
	// warnedAtThreshold tracks whether the 80% threshold warning was logged.
	warnedAtThreshold bool
	// discarding indicates output size limit was exceeded; new events are
	// dropped except [DONE] which passes through for clean stream end.
	discarding bool

	// suppressCutError, when true, prevents the interim gateway_error + [DONE]
	// injection for a genuine mid-generation cut (content seen, no finish_reason,
	// no [DONE]). Used by the stream continuation mechanism so the proxy can
	// resume the stream instead of terminating it. When suppressed, the reader
	// ends with a plain EOF; the proxy decides whether to continue or terminate.
	suppressCutError bool

	// Frame-framing state (NENYA-169). emittedAny records whether any line has
	// been forwarded to the client; lastLineBlank records whether the most
	// recently emitted line was an SSE event terminator (a blank line). SSE
	// frames are separated by a blank line, so a frame the gateway synthesizes
	// or injects must be preceded by one when the previous line did not end an
	// event — otherwise its data: line is appended to the previous event's data
	// buffer and the client sees one corrupted event. The same state terminates
	// the final event at EOF, without which a spec-compliant client discards a
	// pending event (losing e.g. the [DONE] marker).
	//
	// Like the last* fields above, these are read and written only from the
	// single goroutine calling Read(); no mutex is needed.
	emittedAny    bool
	lastLineBlank bool

	// lineBuf is the pooled scratch buffer backing the most recently fetched
	// line. The transformed result may alias it (transformLine passes the input
	// through for passthrough lines) and the emission is delivered across
	// several Read calls, so the buffer is retained here until the next line is
	// fetched or the stream ends — returning it to the process-wide pool
	// earlier would let another reader overwrite bytes this stream has not
	// delivered yet.
	lineBuf *[]byte
}

type pendingToolCall struct {
	id   string
	args string
}

type toolCallState struct {
	mu          sync.RWMutex
	seenIndices map[int]bool
	pending     map[int]*pendingToolCall
	// seenIDs tracks upstream tool-call IDs already claimed by an index in
	// this stream, so duplicates on other indexes are deconflicted
	// deterministically (NENYA-48).
	seenIDs map[string]bool
}

// pendingToolCallCount returns the number of tool-call entries still
// buffered waiting for their name chunk. Used for end-of-stream loss
// telemetry.
func (r *SSETransformingReader) pendingToolCallCount() int {
	r.tcState.mu.RLock()
	defer r.tcState.mu.RUnlock()
	return len(r.tcState.pending)
}

func newToolCallState() toolCallState {
	return toolCallState{
		seenIndices: make(map[int]bool),
		pending:     make(map[int]*pendingToolCall),
		seenIDs:     make(map[string]bool),
	}
}

// SetEmptyStreamCallback registers a callback invoked when the stream ends
// cleanly with no content at all (NENYA-160). Called from the reader
// goroutine; the callback must be safe for concurrent use with respect to its
// own state.
func (r *SSETransformingReader) SetEmptyStreamCallback(cb func()) {
	r.onEmptyStream = cb
}

// NewSSETransformingReader creates a new reader that transforms SSE data lines using the provided transformer.
// If ctx is nil, context.Background() is used as a safe default.
func NewSSETransformingReader(src io.Reader, transformer ResponseTransformer, ctx context.Context) *SSETransformingReader {
	if ctx == nil {
		ctx = context.Background()
	}
	poolBuf := getStreamBuffer()
	reader := &SSETransformingReader{
		src:                 src,
		scanner:             bufio.NewScanner(src),
		transformer:         transformer,
		ctx:                 ctx,
		tcState:             newToolCallState(),
		poolBuf:             poolBuf,
		maxTransformedBytes: DefaultMaxTransformedEventBytes,
		warnedAtThreshold:   false,
	}
	reader.scanner.Split(SplitSSELines)
	reader.scanner.Buffer(*poolBuf, SSEScannerMaxBuf)
	return reader
}

// SetOnUsage sets a callback that receives token usage statistics from the stream.
// The callback is invoked from the same goroutine that calls Read().
// The callback must not access the reader instance concurrently, as internal
// state (last* token fields) is updated without mutex protection.
// SawResumableStop reports whether any frame carried a resumable stop
// reason (pause_turn, NENYA-174).
func (r *SSETransformingReader) SawResumableStop() bool {
	return r.sawResumableStop
}

// isResumableStopPayload checks a parsed frame's stop/finish-reason
// surfaces (OpenAI choice level and delta level, Anthropic delta and
// top-level message shapes) for a resumable reason (NENYA-174).
func isResumableStopPayload(parsed map[string]interface{}) bool {
	if reason, ok := parsed["stop_reason"].(string); ok && IsResumableStopReasonName(reason) {
		return true
	}
	if delta, ok := parsed["delta"].(map[string]interface{}); ok {
		if reason, ok := delta["stop_reason"].(string); ok && IsResumableStopReasonName(reason) {
			return true
		}
	}
	choices, ok := parsed["choices"].([]interface{})
	if !ok {
		return false
	}
	for _, raw := range choices {
		choice, choiceOK := raw.(map[string]interface{})
		if !choiceOK {
			continue
		}
		if reason, ok := choice["finish_reason"].(string); ok && IsResumableStopReasonName(reason) {
			return true
		}
		delta, deltaOK := choice["delta"].(map[string]interface{})
		if !deltaOK {
			continue
		}
		if reason, ok := delta["stop_reason"].(string); ok && IsResumableStopReasonName(reason) {
			return true
		}
	}
	return false
}

// SetDropPrivateEvents toggles the provider-private/telemetry event filter
// (NENYA-174): frames whose parsed "type" names a private channel
// (otel/telemetry/debug/ping prefixes) are suppressed.
func (r *SSETransformingReader) SetDropPrivateEvents(drop bool) {
	r.dropPrivateEvents = drop
}

// privateSSETypePrefixes are the event-type prefixes treated as
// provider-private telemetry when the filter is enabled.
var privateSSETypePrefixes = []string{"otel", "telemetry", "debug", "ping"}

// isPrivateSSEType reports whether a parsed event type names a
// provider-private channel (NENYA-174).
func isPrivateSSEType(parsed map[string]interface{}) bool {
	typ, ok := parsed["type"].(string)
	if !ok || typ == "" {
		return false
	}
	lower := strings.ToLower(typ)
	for _, prefix := range privateSSETypePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// SetOnUsage registers the usage callback. It is invoked from the same
// goroutine that drives the reader, so implementations run without mutex
// protection.
func (r *SSETransformingReader) SetOnUsage(cb UsageCallback) {
	r.onUsage = cb
}

// SetStreamFilter sets a stream filter for content filtering.
func (r *SSETransformingReader) SetStreamFilter(sf *StreamFilter) {
	r.streamFilter = sf
}

// SetExfilGuard sets the output egress guard for URL policy enforcement.
func (r *SSETransformingReader) SetExfilGuard(g *ExfilGuard) {
	r.exfilGuard = g
}

// SetCanaryWatcher sets the per-request canary tripwire.
func (r *SSETransformingReader) SetCanaryWatcher(w *CanaryWatcher) {
	r.canaryWatcher = w
}

// SetStreamEntropyFilter sets an entropy filter for stream content.
func (r *SSETransformingReader) SetStreamEntropyFilter(ef *StreamEntropyFilter) {
	r.streamEntropyFilter = ef
}

// SetOnContent sets a callback that receives content chunks from the stream.
func (r *SSETransformingReader) SetOnContent(cb ContentCallback) {
	r.onContent = cb
}

// SetOnFirstEffectiveOutput registers a one-shot callback fired at the first
// output-bearing frame of the stream (NENYA-178). "Effective output" is defined
// by HasEffectiveOutput, which is the single source of truth for the per-format
// rules: an OpenAI content/tool-call/refusal/audio delta, an Anthropic
// content_block_delta carrying text or tool input (or a tool block opening), a
// Gemini part bearing text/functionCall/inlineData/executableCode/
// codeExecutionResult/fileData — never a keepalive, an empty
// delta, a block close, a role-only handshake, or a usage/metadata frame. With
// includeReasoning the first reasoning delta fires it too (a reasoning-only
// stream still produced a first token); the default excludes reasoning frames so
// slow-thinking models are not measured as slow-to-first-token.
// The callback fires at most once per stream (the hook is cleared when it
// fires, and by ResetCounters). A nil callback is a no-op. Configuration is
// per-reader: callers that reuse a reader must re-register after ResetCounters
// if they still need the signal.
func (r *SSETransformingReader) SetOnFirstEffectiveOutput(cb FirstOutputCallback, includeReasoning bool) {
	r.onFirstOutput = cb
	r.ttftIncludeReasoning = includeReasoning
}

// SetOnThinking sets a callback that receives thinking state changes.
// The callback is invoked when a stream starts or ends emitting reasoning/thinking tokens.
func (r *SSETransformingReader) SetOnThinking(cb ThinkingCallback) {
	r.onThinking = cb
}

func (r *SSETransformingReader) SetObserver(obs SSEObserver) {
	r.observer = obs
}

// SetLogger sets a logger for the transforming reader. Used for warning
// messages about malformed SSE data.
// NOTE: Must be called before the reader is used (before any Read calls).
func (r *SSETransformingReader) SetLogger(logger *slog.Logger) {
	r.logger = logger
}

// ResetCounters resets the transformed byte counter, discarding state, and the
// SSE framing state (NENYA-169). Used when a reader is reused across multiple
// requests to prevent state leakage between streams. Currently readers are
// created per-request, so this is reserved for future reader-pooling
// implementations.
//
// Precondition: the reader must be drained (no pending buffered bytes). The
// retained line-copy buffer is released only in that state, because it may back
// output that has not been delivered yet.
func (r *SSETransformingReader) ResetCounters() {
	r.transformedBytes = 0
	r.discarding = false
	r.warnedAtThreshold = false
	r.sawContent = false
	r.sawFinishReason = false
	r.sawAnthropicFinish = false
	r.sawConvertedAnthropicFinish = false
	r.sawAnthropicContent = false
	r.sawRawAnthropicDelta = false
	r.sawDone = false
	r.injectedError = false
	r.emittedAny = false
	r.lastLineBlank = false
	// Clear the one-shot first-output hook (NENYA-178): a caller that reuses
	// this reader must re-register it to measure a further stream. The
	// reasoning-inclusion flag is configuration, not stream state, so it
	// survives the reset.
	r.onFirstOutput = nil
	if r.pos >= len(r.buffer) {
		r.releaseLineBuffer()
	}
}

// SetSuppressCutError disables the interim gateway_error + [DONE] injection
// for a genuine mid-generation cut (content seen, no finish_reason, no [DONE]).
// When suppressed, the reader ends with a plain EOF; the calling proxy layer
// decides whether to resume the stream instead of terminating it.
func (r *SSETransformingReader) SetSuppressCutError(v bool) {
	r.suppressCutError = v
}

// trackStreamProgress records the terminal-state signals used to classify a
// stream ending (SawContent/SawFinishReason) and to gate continuation and the
// synthetic finish_reason; SawDone classifies no-[DONE] endings alongside
// them: finish_reason without DONE is benign truncation, content without
// finish_reason is a genuine cut.
//
// Deliberate divergence from HasEffectiveOutput (NENYA-178): this uses the
// narrower ExtractDeltaContentFromMap (OpenAI delta.content) plus a reasoning
// fallback, so a tool-call-only, array-content, or Gemini-native stream registers
// no content here. The two notions are NOT interchangeable: sawContent also
// drives isStreamCut and the continuation path, and the continuation capture is
// parsed by the OpenAI-shaped bufferStreamResponse. Broadening sawContent would
// therefore turn a Gemini-native cut into a "resumable" stream that immediately
// gives up (gave_up_no_content) and writes a gateway_error instead of ending
// cleanly. Unifying the notions needs its own item with continuation-format
// coverage.
//
// Native-Anthropic passthrough is recognized beyond the OpenAI shapes when no
// transformer is installed (nil transformer means the transformed bytes ARE the
// raw upstream wire): the Anthropic event envelope carries the same terminal
// signals, and without reading them a COMPLETE passthrough stream classified as
// empty at EOF — a gateway_error injected after the client's own message_stop,
// a breaker charge, and no cache store (AUD-01). CONVERTED paths additionally
// recognize a deliberately narrow Anthropic subset on their own frames
// (content_block_delta.delta.text for content; message_delta stop_reason /
// message_stop for completion — AUD-12): a converted stream is Anthropic-shaped
// on the client's wire, so the multi-event framing would otherwise lose the
// completion signal. That recognition sets sawConvertedAnthropicFinish (not
// sawAnthropicFinish, which stays passthrough-only) so the EOF path also
// refuses to inject the OpenAI sentinel [DONE] onto an Anthropic client's wire.
func (r *SSETransformingReader) trackStreamProgress(transformed []byte) {
	if r.sawContent && r.sawFinishReason {
		return
	}
	// One emission is normally a single line, but composed payloads (the
	// pre-terminal held flush + terminal line, finishStreamFrames sequences)
	// carry several frames: track every data line in the payload, or the
	// terminal frame's completion signal is lost whenever a tail composes
	// ahead of it (AUD-02). Allocation-free: IndexByte scanning only.
	rest := transformed
	for len(rest) > 0 {
		if r.sawContent && r.sawFinishReason {
			return
		}
		var line []byte
		if idx := bytes.IndexByte(rest, '\n'); idx >= 0 {
			line = rest[:idx]
			rest = rest[idx+1:]
		} else {
			line = rest
			rest = nil
		}
		r.trackProgressLine(line)
	}
}

// trackProgressLine records the terminal-state signals of ONE line of an
// emission (see trackStreamProgress).
func (r *SSETransformingReader) trackProgressLine(line []byte) {
	data := bytes.TrimSpace(line)
	if value, isData := SSEDataField(data); isData {
		// Tolerant field recognition (NENYA-197): a no-space data line must
		// still register content/finish_reason, or the terminal-state matrix
		// misclassifies a stream that completed (spurious gateway_error, no
		// synthetic finish_reason, continuation disabled, caching blocked).
		data = bytes.TrimSpace(value)
	}
	if len(data) == 0 || data[0] != '{' {
		return
	}
	parsed := ParseSSEChunk(data)
	if parsed == nil {
		return
	}
	r.trackAnthropicSurface(parsed)
	if r.progressContent(parsed) {
		r.sawContent = true
	}
	if !r.sawContent && hasReasoningDelta(parsed) {
		r.sawContent = true
	}
	if r.progressFinish(parsed) {
		r.sawFinishReason = true
		// AUD-12 review: on a CONVERTED surface (transformer installed) an
		// Anthropic-shaped completion must not receive the OpenAI sentinel
		// data: [DONE] at EOF either (AUD-01's passthrough argument applies
		// identically — the frame is unparseable on that client's wire), so
		// track the converted Anthropic completion independently of WHICH
		// frame first transitioned sawFinishReason: a converter passing a
		// raw OpenAI finish_reason through must not permanently block the
		// suppression for a later message_stop.
		if r.transformer != nil && !r.sawConvertedAnthropicFinish && r.progressFinishAnthropic(parsed) {
			r.sawConvertedAnthropicFinish = true
		}
	}
}

// progressContent reports whether one parsed frame carries generation text:
// the OpenAI delta spelling, or (AUD-12) an Anthropic-shaped converted frame's
// content_block_delta text.
func (r *SSETransformingReader) progressContent(parsed map[string]interface{}) bool {
	if content := ExtractDeltaContentFromMap(parsed); content != "" {
		return true
	}
	if t, _ := parsed["type"].(string); t == "content_block_delta" {
		if delta, ok := parsed["delta"].(map[string]interface{}); ok {
			s, _ := delta["text"].(string)
			return s != ""
		}
	}
	return false
}

// progressFinish reports whether one parsed frame completes the stream: the
// OpenAI choices[0].finish_reason, or (AUD-12) the Anthropic-shaped converted
// frames' message_delta stop_reason / message_stop. Without the Anthropic
// check the multi-event framing registers no completion and the terminal-state
// matrix misclassifies the stream.
func (r *SSETransformingReader) progressFinish(parsed map[string]interface{}) bool {
	if ExtractFinishReasonFromMap(parsed) != "" {
		return true
	}
	return r.progressFinishAnthropic(parsed)
}

// progressFinishAnthropic reports whether one parsed frame is an
// Anthropic-shaped completion (message_delta with a stop_reason, or
// message_stop). Deliberately narrow: the gateway only acts on it as a
// completion signal, and a false positive is fail-safe (a stream treated as
// completed early, never a stuck one).
func (r *SSETransformingReader) progressFinishAnthropic(parsed map[string]interface{}) bool {
	t, _ := parsed["type"].(string)
	if t != "message_delta" && t != "message_stop" {
		return false
	}
	if t == "message_stop" {
		return true
	}
	delta, ok := parsed["delta"].(map[string]interface{})
	return ok && delta["stop_reason"] != nil
}

// trackAnthropicSurface records the native-Anthropic terminal signals of a
// passthrough chunk onto the terminal-state matrix. Gated on the nil
// transformer: only there are the transformed bytes the raw upstream wire, so
// only there can the Anthropic envelope be trusted as the client's surface.
// Converted paths do NOT reach this function (their own narrow Anthropic
// recognition lives in progressContent/progressFinish instead) — this one
// establishes the PASSTHROUGH flags, including sawAnthropicFinish.
func (r *SSETransformingReader) trackAnthropicSurface(parsed map[string]interface{}) {
	if r.transformer != nil {
		return
	}
	content, finish := anthropicSurfaceProgress(parsed)
	if content {
		r.sawContent = true
		r.sawAnthropicContent = true
	}
	if finish {
		r.sawFinishReason = true
		// Remember WHICH surface completed the stream: handleScannerDone
		// suppresses the injected [DONE] only for Anthropic-surface
		// completions — the client already got the upstream's message_stop.
		r.sawAnthropicFinish = true
	}
}

// anthropicSurfaceProgress reads the terminal-state signals of a native
// Anthropic SSE chunk (the top-level "type" event envelope) for the
// terminal-state matrix. It reports whether the chunk carries generated
// content (content) and whether it carries a completion signal (finish);
// non-Anthropic chunks report false, false. Only consulted on the passthrough
// path (nil transformer): message_stop is a completion signal even when a
// malformed message_delta never preceded it, and content_block_delta counts
// text, thinking, and tool-argument deltas as generated content.
func anthropicSurfaceProgress(parsed map[string]interface{}) (content, finish bool) {
	typ, _ := parsed["type"].(string)
	switch typ {
	case "content_block_delta":
		delta, _ := parsed["delta"].(map[string]interface{})
		if text, _ := delta["text"].(string); text != "" {
			content = true
		}
		if thinking, _ := delta["thinking"].(string); thinking != "" {
			content = true
		}
		if args, _ := delta["partial_json"].(string); args != "" {
			content = true
		}
		if _, cited := delta["citation"]; cited {
			content = true
		}
	case "message_delta":
		delta, _ := parsed["delta"].(map[string]interface{})
		if stop, _ := delta["stop_reason"].(string); stop != "" {
			finish = true
		}
	case "message_stop":
		finish = true
	}
	return content, finish
}

// hasReasoningDelta reports whether an OpenAI-format chunk carries a non-empty
// reasoning_content delta (e.g. DeepSeek/Zai thinking streams). Such chunks
// indicate the model is generating even before visible content arrives.
func hasReasoningDelta(chunk map[string]interface{}) bool {
	choices, ok := chunk["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return false
	}
	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return false
	}
	delta, ok := choice["delta"].(map[string]interface{})
	if !ok {
		return false
	}
	rc, _ := delta["reasoning_content"].(string)
	return rc != ""
}

// SetMaxTransformedBytes sets the maximum allowed accumulated transformed output.
// If zero or negative, resets to the default (50MB).
func (r *SSETransformingReader) SetMaxTransformedBytes(maxBytes int) {
	if maxBytes > 0 {
		r.maxTransformedBytes = maxBytes
	} else {
		r.maxTransformedBytes = DefaultMaxTransformedEventBytes
	}
}

// SawDone returns true if the reader has observed a data: [DONE] event.
func (r *SSETransformingReader) SawDone() bool {
	return r.sawDone
}

// SawContent returns true if any assistant content chunk has been forwarded
// to the client. Used together with SawFinishReason to classify stream
// endings that lack a [DONE] marker.
func (r *SSETransformingReader) SawContent() bool {
	return r.sawContent
}

// SawFinishReason returns true if the model signaled a completion
// (non-null finish_reason) within the stream. A stream ending without [DONE]
// after a finish_reason is benign truncation; without one it is a genuine
// mid-generation cut.
func (r *SSETransformingReader) SawFinishReason() bool {
	return r.sawFinishReason
}

// SuppressCutError returns true if the reader is suppressing the interim
// gateway_error injection for a genuine mid-generation cut.
func (r *SSETransformingReader) SuppressCutError() bool {
	return r.suppressCutError
}

// InjectedError returns true if the reader replaced the upstream's terminal
// with a synthesized gateway_error frame (empty stream, mid-generation cut,
// oversize line, or size-limit discard). Such streams are sanitized
// truncations: they must never be persisted to the response cache (NENYA-27)
// because replaying them would serve a dead upstream's partial output.
func (r *SSETransformingReader) InjectedError() bool {
	return r.injectedError
}

// getTransformedLine returns the transformed line, using pooled buffers when possible.
func (r *SSETransformingReader) getTransformedLine(line []byte) []byte {
	// Release the previous line's buffer first: by the time a new line is
	// fetched, the previous emission has been fully delivered (Read only scans
	// again once its buffer is drained).
	r.releaseLineBuffer()
	if len(line) > maxPooledBufSize {
		lineCopy := make([]byte, len(line))
		copy(lineCopy, line)
		return r.transformLine(lineCopy)
	}
	buf := GetLineCopyBuffer()
	*buf = (*buf)[:len(line)]
	copy(*buf, line)
	r.lineBuf = buf
	return r.transformLine(*buf)
}

// releaseLineBuffer returns the retained line-copy buffer to the pool. It must
// only be called once the bytes derived from it have been fully delivered (or
// the stream has ended).
func (r *SSETransformingReader) releaseLineBuffer() {
	if r.lineBuf == nil {
		return
	}
	PutLineCopyBuffer(r.lineBuf)
	r.lineBuf = nil
}

// finishWithError releases the reader's pooled scratch buffers and returns err.
// Every terminal exit funnels through here so a retained line buffer can never
// outlive the stream (the pool is process-wide; keeping a buffer alive past the
// stream would waste it, and recycling it early would corrupt live output).
func (r *SSETransformingReader) finishWithError(err error) (int, error) {
	r.releaseLineBuffer()
	buf := r.poolBuf
	r.poolBuf = nil
	putStreamBuffer(buf)
	return 0, err
}

// safePercentage calculates percentage with overflow protection.
func safePercentage(part, total uint64) uint64 {
	if total == 0 {
		return 0
	}
	if part < math.MaxUint64/100 {
		return part * 100 / total
	}
	return 100
}

func (r *SSETransformingReader) Read(p []byte) (int, error) {
	// Deliver buffered bytes before honoring cancellation or a terminal error:
	// the buffer may hold the frame terminator that keeps the client's last
	// event dispatchable, and a partially served line must never be truncated
	// by a separator (NENYA-169).
	if r.pos < len(r.buffer) {
		n := copy(p, r.buffer[r.pos:])
		r.pos += n
		if r.pos >= len(r.buffer) {
			r.buffer = nil
			r.pos = 0
		}
		return n, nil
	}

	if r.ctx != nil {
		select {
		case <-r.ctx.Done():
			return r.finishOnContextDone(p)
		default:
		}
	}

	if r.err != nil {
		return r.finishWithError(r.err)
	}

	if r.discarding && !errors.Is(r.err, ErrTransformedSizeExceeded) {
		r.injectErrorBuffer("transformed output exceeded maximum size")
		r.err = ErrTransformedSizeExceeded
		return r.Read(p)
	}

	if !r.scanner.Scan() {
		r.handleScannerDone()
		return r.drainAfterScanDone(p)
	}

	for {
		transformed := r.getTransformedLine(r.scanner.Bytes())

		if transformed == nil {
			if r.anyFilterBlocked() {
				// The last scanned chunk flipped a filter to blocked: close the
				// pending event and surface the specific error instead of a
				// generic truncated-stream gateway error. The terminator is
				// delivered first (the caller writes its own policy-block frame
				// onto the committed stream next).
				r.err = r.blockedStreamError()
				r.closePendingEvent()
				return r.Read(p)
			}
			if !r.scanner.Scan() {
				r.handleScannerDone()
				return r.drainAfterScanDone(p)
			}
			continue
		}

		if r.anyFilterBlocked() {
			r.err = r.blockedStreamError()
			r.closePendingEvent()
			return r.Read(p)
		}

		if !bytes.HasSuffix(transformed, []byte("\n")) {
			transformed = append(transformed, '\n')
		}
		r.noteEmittedLine(transformed)

		r.trackTransformedSize(transformed)
		r.trackStreamProgress(transformed)

		r.buffer = transformed
		r.pos = 0

		return r.Read(p)
	}
}

// finishOnContextDone ends the stream when the request context is done.
// NENYA-169: the final event is closed first — the proxy may still write its own
// terminal frame (e.g. the timeout terminator) onto an already-committed stream,
// and an unterminated last line would glue that frame onto the previous event's
// data. Read serves buffered bytes before this path, so the separator can never
// truncate a partially delivered line. The cancellation is then surfaced on the
// following call.
func (r *SSETransformingReader) finishOnContextDone(p []byte) (int, error) {
	// A zero-length read must not discard the pending event terminator:
	// defer termination by one read so the separator is delivered
	// (AUD-30 / NENYA-249). Returning (0, nil) for len(p)==0 is legal per
	// the io.Reader contract.
	if sep := r.eventSeparator(); len(sep) > 0 {
		if len(p) == 0 {
			return 0, nil
		}
		n := copy(p, sep)
		r.lastLineBlank = true
		return n, nil
	}
	err := r.ctx.Err()
	// The terminal branch funnels through the same observer notification as
	// every other stream end — pre-fix OnStreamClose was skipped on this
	// path, so downstream observers (egress watchers, held-tail drains)
	// never saw the close (AUD-30 / NENYA-249). The closed flag guards a
	// double notification, mirroring the drain path.
	if r.observer != nil && !r.closed {
		r.closed = true
		r.observer.OnStreamClose(err)
	}
	return r.finishWithError(err)
}

// closePendingEvent buffers the blank line that terminates the final forwarded
// event when it was left unterminated, so a frame the caller writes afterwards
// (the proxy's policy-block, stall, timeout or interruption frame, or a resumed
// stream) starts a new SSE event instead of joining the previous event's data.
// It is a no-op when nothing was forwarded, when the last line already
// terminated its event, or when a buffer is already pending.
//
// Deliberately unconditional: the terminator is added even when the final line
// looks like a frame truncated mid-JSON. Withholding it cannot hide that line
// (it has already been forwarded), and it would corrupt whatever the caller
// writes next — the gateway's error frame would be absorbed into the truncated
// event and never reach the client. The injected gateway_error/[DONE] (or the
// proxy's own terminal frame) that follows is what tells the client the stream
// was cut.
func (r *SSETransformingReader) closePendingEvent() {
	if len(r.buffer) > 0 || !r.emittedAny || r.lastLineBlank {
		return
	}
	r.buffer = []byte("\n")
	r.pos = 0
	r.lastLineBlank = true
}

// noteEmittedLine records the framing state of an emission (one or more lines)
// about to be forwarded. Read guarantees each emitted line ends with '\n', so an
// emission terminates an SSE event exactly when it ends with a blank line. The
// state is recorded when the bytes are buffered, not when they are delivered;
// Read serves the buffer before any path that consumes this state.
func (r *SSETransformingReader) noteEmittedLine(emitted []byte) {
	r.emittedAny = true
	body := emitted
	if len(body) > 0 && body[len(body)-1] == '\n' {
		body = body[:len(body)-1]
	}
	r.lastLineBlank = len(body) == 0 || body[len(body)-1] == '\n'
}

// eventSeparator returns the bytes that must precede a frame the gateway
// synthesizes or injects so that frame starts a NEW SSE event. Frames are
// separated by a blank line: without it the injected data: line joins the
// previous event's data buffer and the client sees one corrupted event whose
// payload is the concatenation. It returns nil when the previous line already
// terminated its event, which keeps a well-formed passthrough byte-identical.
func (r *SSETransformingReader) eventSeparator() []byte {
	if r.emittedAny && !r.lastLineBlank {
		return []byte("\n")
	}
	return nil
}

// appendSSEEvent appends one TERMINATED SSE event — separator + frame + blank
// line — to out, and returns the extended buffer. This is the reader's single
// composition primitive (AUD-64 / NENYA-306): injected terminals, held-tail
// drains, and finishStreamFrames all frame their events through it, so the
// wire shape cannot drift between the sites. The separator honors the
// reader's line state at call time.
func (r *SSETransformingReader) appendSSEEvent(out, frame []byte) []byte {
	out = append(out, r.eventSeparator()...)
	out = append(out, frame...)
	return append(out, '\n', '\n')
}

// trackTransformedSize accumulates the transformed output size and sets the
// discarding flag when the total exceeds maxTransformedBytes.
func (r *SSETransformingReader) trackTransformedSize(transformed []byte) {
	blen := uint64(len(transformed))
	if r.transformedBytes > ^uint64(0)-blen {
		r.transformedBytes = ^uint64(0)
	} else {
		r.transformedBytes += blen
	}
	if r.logger != nil && !r.warnedAtThreshold && r.maxTransformedBytes > 0 {
		threshold := uint64(r.maxTransformedBytes) / 10 * 8
		if r.transformedBytes >= threshold {
			r.warnedAtThreshold = true
			r.logger.Warn("transformed SSE output approaching size limit",
				"accumulated", r.transformedBytes,
				"limit", r.maxTransformedBytes,
				"pct", safePercentage(r.transformedBytes, uint64(r.maxTransformedBytes)))
		}
	}
	if r.transformedBytes > uint64(r.maxTransformedBytes) && !r.discarding {
		if r.logger != nil {
			r.logger.Warn("transformed output exceeded size limit, will discard subsequent events",
				"limit", r.maxTransformedBytes, "accumulated", r.transformedBytes)
		}
		r.discarding = true
	}
}

func (r *SSETransformingReader) transformLine(line []byte) []byte {
	if r.discarding {
		// Allow [DONE] to pass through so the stream can end cleanly. This is
		// defensive: trackTransformedSize flips discarding mid-stream and the
		// next Read injects the size error before scanning resumes, so the
		// branch is normally unreachable.
		if value, isData := SSEDataField(line); isData && IsDonePayload(value) {
			r.sawDone = true
			return []byte(canonicalDoneFrame)
		}
		return nil
	}

	if len(line) == 0 {
		return line
	}

	if _, isData := SSEDataField(line); !isData {
		result := r.transformNonSSELine(line)
		if result == nil {
			return nil
		}
		return result
	}

	result := r.transformSSEData(line)
	if result == nil {
		return nil
	}
	return result
}

// handleScannerDone is called when the scanner has no more input.
// It sets r.err and optionally injects a terminal event into r.buffer.
// Classification of a clean EOF (scanner.Err() == nil, no [DONE]):
//   - finish_reason seen: benign truncation → inject silent [DONE], no error.
//   - no content at all: empty stream → inject gateway_error + [DONE].
//   - content seen but no finish_reason: genuine mid-generation cut → keep
//     interim gateway_error injection (Phase 1 continuation intercepts this
//     via SawContent/SawFinishReason).
