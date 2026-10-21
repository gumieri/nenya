package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"reflect"
	"strconv"
	"strings"
	"sync"

	"github.com/nenya/internal/util"
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
const canonicalDoneFrame = "data: [DONE]"

// ResponseTransformer transforms one SSE data payload. TransformSSEChunk
// receives the JSON payload of a data: line (without the field prefix) and must
// return a SINGLE-LINE payload: the reader re-prefixes exactly one data: line
// per call, so a payload containing a raw newline would corrupt the frame.
type ResponseTransformer interface {
	TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error)
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
	injectedError   bool
	ctx             context.Context
	poolBuf         *[]byte
	logger          *slog.Logger

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

// trackStreamProgress marks whether assistant content or a completion
// signal has been observed in the stream. Used together with SawDone to
// classify no-[DONE] endings: finish_reason without DONE is benign truncation;
// content without finish_reason is a genuine cut.
// trackStreamProgress records the terminal-state signals used to classify a
// stream ending (SawContent/SawFinishReason) and to gate continuation and the
// synthetic finish_reason.
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
func (r *SSETransformingReader) trackStreamProgress(transformed []byte) {
	if r.sawContent && r.sawFinishReason {
		return
	}
	data := bytes.TrimSpace(transformed)
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
	content := ExtractDeltaContentFromMap(parsed)
	if content != "" {
		r.sawContent = true
	}
	if !r.sawContent && hasReasoningDelta(parsed) {
		r.sawContent = true
	}
	if !r.sawFinishReason {
		fr := ExtractFinishReasonFromMap(parsed)
		if fr != "" {
			r.sawFinishReason = true
		}
	}
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
	if sep := r.eventSeparator(); len(sep) > 0 && len(p) > 0 {
		n := copy(p, sep)
		r.lastLineBlank = true
		return n, nil
	}
	return r.finishWithError(r.ctx.Err())
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
		if value, isData := SSEDataField(line); isData && bytes.Equal(bytes.TrimSpace(value), []byte("[DONE]")) {
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
	buf := append(r.eventSeparator(), []byte("data: [DONE]\n\n")...)
	if heldOK {
		pre := append(r.eventSeparator(), held...)
		pre = append(pre, '\n', '\n')
		buf = append(pre, buf...)
	}
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
	r.buffer = append(append(append(r.eventSeparator(), []byte("data: ")...), errPayload...), []byte("\n\ndata: [DONE]\n\n")...)
	if heldOK {
		pre := append(r.eventSeparator(), held...)
		pre = append(pre, '\n', '\n')
		r.buffer = append(pre, r.buffer...)
	}
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
	if bytes.Equal(bytes.TrimSpace(origData), []byte("[DONE]")) {
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

	data = r.applyContentFilters(data, parsed)

	r.callUsageAndContentCallbacks(parsed)

	if r.transformer == nil {
		result := r.handleNoTransformer(parsed, data, origData, line)
		if result == nil {
			return nil
		}
		return result
	}

	result := r.handleWithTransformer(parsed, data, origData, line)
	if result == nil {
		return nil
	}
	return result
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

// finishStreamFrames composes the clean-terminal frame sequence at
// [DONE]: the exfil guard's held tail first (NENYA-205 — the response's
// last fragment must not be withheld), then the synthetic finish_reason
// chunk when one is due, then the canonical [DONE]. Each synthesized
// frame is a complete blank-line-separated event (NENYA-169 convention).
// With nothing synthesized the upstream line passes through unchanged.
func (r *SSETransformingReader) finishStreamFrames(upstreamLine, line []byte) []byte {
	heldLine, heldOK := r.synthesizeHeldFlush()
	synthLine, synthOK := r.synthesizeTerminalFinish()
	r.notifySSEObserver(upstreamLine, nil, "done")
	if !heldOK && !synthOK {
		return line
	}
	separator := r.eventSeparator()
	out := []byte{}
	for _, part := range [][]byte{heldLine, synthLine, line} {
		if len(part) == 0 {
			continue
		}
		out = append(out, separator...)
		out = append(out, part...)
		out = append(out, '\n', '\n')
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
		return nil, false
	}
	encoded := buildExfilFlushChunk(r.lastSurface, held)
	if encoded == nil {
		return nil, false
	}
	out := r.transformSSEData(append([]byte("data: "), encoded...))
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
// [DONE] passthrough branch bypasses this path entirely).
func (r *SSETransformingReader) synthesizeTerminalFinish() ([]byte, bool) {
	if r.transformer != nil || !r.sawContent || r.sawFinishReason {
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

// applyStreamFilter runs the secret/policy filter over one SSE delta,
// reporting the resulting action so callers can detect rewrites.
func applyStreamFilter(parsed map[string]interface{}, filter *StreamFilter) (map[string]interface{}, FilterAction) {
	content := ExtractDeltaContentFromMap(parsed)
	if content == "" {
		return parsed, ActionPass
	}
	redacted, action, _ := filter.FilterContent(content)
	if action == ActionBlock {
		return nil, ActionBlock
	}
	if action == ActionRedact && redacted != content {
		parsed = copyMap(parsed)
		_ = ReplaceDeltaContentMap(parsed, redacted)
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

// applyCanaryWatcher scans one SSE delta for the request canary. A
// block verdict nils the chunk so the reader raises ErrCanaryDetected.
func applyCanaryWatcher(parsed map[string]interface{}, watcher *CanaryWatcher) map[string]interface{} {
	content := extractExfilContent(parsed)
	if content == "" {
		return parsed
	}
	if _, action, _ := watcher.FilterContent(content); action == ActionBlock {
		return nil
	}
	return parsed
}

// applyExfilStage runs the URL egress policy over one parsed chunk: it
// records the wire shape being evaluated (the terminal flush mimics it,
// NENYA-205), applies the guard's client-visible return, and reports
// whether the guard blocked this chunk.
func (r *SSETransformingReader) applyExfilStage(parsed map[string]interface{}) (out map[string]interface{}, mutated, blocked bool) {
	if content := extractExfilContent(parsed); content != "" {
		r.lastSurface = exfilSurfaceOf(parsed)
	}
	out, _, mutated = applyExfilGuard(parsed, r.exfilGuard)
	blocked = r.exfilGuard.IsBlocked()
	return out, mutated, blocked
}

// applyExfilGuard runs the URL egress policy over one SSE delta in
// either wire format (OpenAI or Anthropic). The guard's return is the
// client-visible content for this delta and is applied on EVERY action:
// stripped spans replace the delta field, and a shortened emit (the
// guard withheld a whitespace-free tail, NENYA-205) must not stream the
// original — the withheld remainder is delivered at the terminal flush.
// A block verdict nils the chunk so the reader raises ErrExfilBlocked
// (the guard itself flips to blocked). The bool reports whether the
// chunk was rewritten (callers re-encode).
func applyExfilGuard(parsed map[string]interface{}, guard *ExfilGuard) (map[string]interface{}, FilterAction, bool) {
	content := extractExfilContent(parsed)
	if content == "" {
		return parsed, ActionPass, false
	}
	rewritten, action, _ := guard.FilterContent(content)
	if action == ActionBlock {
		return nil, ActionBlock, true
	}
	if rewritten != content {
		parsed = copyMap(parsed)
		if !setExfilContent(parsed, rewritten) {
			return parsed, ActionPass, false
		}
		return parsed, action, true
	}
	return parsed, action, false
}

// applyEntropyFilter runs the entropy redactor over one SSE delta,
// reporting whether it rewrote the chunk.
func applyEntropyFilter(parsed map[string]interface{}, filter *StreamEntropyFilter) (map[string]interface{}, bool) {
	content := ExtractDeltaContentFromMap(parsed)
	if content == "" {
		return parsed, false
	}
	redacted, action := filter.FilterContent(content)
	if action == ActionRedact && redacted != content {
		parsed = copyMap(parsed)
		ReplaceDeltaContentMap(parsed, redacted)
		return parsed, true
	}
	return parsed, false
}

func copyMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *SSETransformingReader) applyContentFilters(data []byte, parsed map[string]interface{}) []byte {
	// TODO: Implement content filtering if needed (currently no-op)
	return data
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

	if bytes.Equal(transformed, []byte("[DONE]")) {
		r.sawDone = true
		r.notifySSEObserver([]byte(canonicalDoneFrame), nil, "done")
		return []byte(canonicalDoneFrame)
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

	finalLine := append([]byte("data: "), transformed...)
	r.notifySSEObserver(finalLine, parsed, "")
	return finalLine
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
	return transformed
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
		Model:               r.responseModel(chunk),
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
