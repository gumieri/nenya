package mcpproxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// ErrorType represents the type of error that occurred during request
// processing. Maps to upstream provider error types (provider_error,
// rate_limit_error, etc.). Mirrors the parent package's ErrorType spellings
// (the gateway's stream-error writer owns the wire values).
type ErrorType = string

// ErrorType constants mirror the parent package's values (the gateway's
// stream-error writer owns the spellings). Keep byte-identical with
// internal/proxy/error_normalizer.go — a drift here silently changes the
// SSE error wire values the client sees.
const (
	ErrorTypeRateLimit ErrorType = "rate_limit_error"
	ErrorTypeGateway   ErrorType = "gateway_error"
)

// ---- Types that moved from the parent package (exported; the parent
// keeps unexported aliases so existing call sites compile unchanged) ----

// ChatRequest holds the validated request data extracted from an incoming
// /v1/chat/completions or /v1/messages payload.
type ChatRequest struct {
	Payload     map[string]any
	ModelName   string
	TokenCount  int
	Stream      bool
	Targets     []routing.UpstreamTarget
	AgentName   string
	Cooldown    time.Duration
	MaxRetries  int
	CacheKey    string
	HasMCPTools bool
	// InjectedMCPTools is the set of MCP function names the GATEWAY injected
	// for this request (NENYA-162). It is the trust anchor for the loop's
	// non-injected-call guard: re-deriving the set from the client-writable
	// payload would let a caller declare an excluded tool name and defeat the
	// policy.
	InjectedMCPTools []string
	SoftLimit        int
	HardLimit        int
	WindowMaxCtx     int
	Profile          pipeline.ClientProfile
	Messages         []any
	KeyRef           string
	SourceFormat     string // "openai" or "anthropic" - indicates the original client request format
	// BYOK reports that this request is served with a caller-supplied
	// credential (NENYA-195): the response cache is bypassed and shared
	// account rotation/breaker state is never written.
	BYOK bool

	// BYOKRequested reports that the request PRESENTED a BYOK credential
	// (the global switch is on and the header is non-empty). It is resolved
	// before routing so session affinity stands down for a caller-funded
	// request, whose target list is narrowed to opt-in providers afterwards
	// (NENYA-195). BYOK is set only once a credential was actually applied.
	BYOKRequested bool
	// BYOKCredential is the trimmed header value, read once.
	BYOKCredential string
	// Agent is the resolved agent config when ModelName names an agent
	// (zero value for direct model routes). Set once in resolveRouting;
	// all downstream consumers (sticky routing, MCP flow, session header
	// synthesis) share this resolution. HasAgent carries the has/has-not
	// bit so consumers can take the config's ADDRESS without re-resolving
	// (a re-resolution could diverge after a mid-request SIGHUP reload).
	Agent    config.AgentConfig
	HasAgent bool

	// SessionKey/SessionKeyOK/SessionKeyDone memoize the sticky-session key
	// (see the parent's derivedSessionKey). Memoization sentinel:
	// SessionKeyDone; the fields are only touched on the request goroutine
	// before upstream dispatch begins, so no lock is needed.
	SessionKey     string
	SessionKeyOK   bool
	SessionKeyDone bool

	// APIKey is the authenticated key for this request, set once in
	// validateChatRequest. It carries the per-key MCP tool grant (NENYA-183)
	// and is never nil on a served request path.
	APIKey *config.ApiKey

	// SessionSubject binds all session-derived state (sticky pins, affinity
	// forks, synthesized session headers) to the authenticated caller
	// (NENYA-196): the digest of the API key's token, set once in
	// validateChatRequest. Empty for a nil key (defensive/test path); the
	// synthetic primary-token key hashes its empty token, giving primary
	// traffic its own single-operator namespace.
	SessionSubject string

	// AffinityKey/AffinityDone memoize the affinity-resolved session identity
	// (see the parent's sessionIdentity): the root session key, or a
	// deterministic fork key when the leading turns collide with a different
	// conversation (NENYA-39). Same single-goroutine discipline as the raw
	// key fields.
	AffinityKey  string
	AffinityDone bool
}

// AgentPointer returns the resolved agent config's address when the request
// resolved to an agent, else nil.
func (req *ChatRequest) AgentPointer() *config.AgentConfig {
	if req.HasAgent {
		return &req.Agent
	}
	return nil
}

// HTTPError is a terminal request-validation error: the HTTP status, the
// model-facing message, and the structured error kind.
type HTTPError struct {
	Code    int
	Message string
	Kind    infra.ErrorKind
}

// Error implements the error interface over the model-facing message.
func (e *HTTPError) Error() string { return e.Message }

// ForwardOptions holds the parameters for forwarding a request upstream.
type ForwardOptions struct {
	Targets    []routing.UpstreamTarget
	Payload    map[string]any
	Stream     bool
	Cooldown   time.Duration
	TokenCount int
	AgentName  string
	// Agent is the resolved agent config for AgentName (zero value for
	// direct model routes), resolved once in resolveRouting.
	Agent        config.AgentConfig
	MaxRetries   int
	CacheKey     string
	KeyRef       string
	SourceFormat string
	APIKey       *config.ApiKey
	// Canary carries the per-request tripwire token (empty when the
	// guard is disabled).
	Canary pipeline.CanarResult
	// Screen carries the per-request egress screen runtime (nil when
	// the judgment site is disabled); its budget is shared across all
	// checkpoints of the request.
	Screen *EgressScreenRuntime
	// WindowCompacted reports that the content pipeline rewrote the history
	// (window compaction): auto cache-breakpoint anchoring stands down
	// (NENYA-185).
	WindowCompacted bool
	// InjectedMCPTools is the set of MCP function names the gateway injected
	// for this request (NENYA-162): the loop's non-injected-call guard is
	// anchored on it, never on the client-writable payload.
	InjectedMCPTools []string
	// EntropyRedacted reports whether the entropy interceptor redacted
	// high-entropy spans from this request. Consumed only by the
	// buffered-response checkpoint (the tool-args checkpoint triggers
	// off its own canary/guard signals).
	EntropyRedacted bool
}

// DispatchParams groups the per-dispatch parameters PrepareAndSend consumes
// (AUD-60): everything that varies per upstream attempt.
type DispatchParams struct {
	Idx                  int
	Targets              []routing.UpstreamTarget
	Target               routing.UpstreamTarget
	Payload              map[string]interface{}
	Cooldown             time.Duration
	TokenCount           int
	AgentName            string
	APIKey               *config.ApiKey
	Streaming            bool
	SourceFormat         string
	WindowCompacted      bool
	UnknownFieldsAudited *routing.RequestTelemetry
}

// StreamFailoverReason distinguishes why a stream attempt handed the dispatch
// back to the retry loop, so the loop can pick the right failover reason (and
// with it the sticky_provider behavior) and log the real diagnosis.
type StreamFailoverReason int

const (
	// The unnamed zero value: no failover context. A zero StreamResult reads
	// as "no failover story" without a sentinel bool; the loop only switches
	// on the named reasons below.
	_ StreamFailoverReason = iota
	// StreamFailoverPolicyRefusal: a policy refusal/safeguard terminal
	// (NENYA-191) — not a provider fault, so the target was not charged and
	// the sticky lenient policy allows the failover.
	StreamFailoverPolicyRefusal
)

// StreamResult carries the outcome of the streaming response back to the
// retry loop. Empty signals a zero-byte upstream stream; Err signals a
// transport-level read failure before any stream bytes were produced
// (distinct from empty).
type StreamResult struct {
	Empty bool
	// Terminal marks a response already fully written to the client
	// (e.g. a content-policy block): the retry loop must stop instead of
	// failing over and appending another response to the committed one.
	Terminal bool
	Err      error
	// FailoverReason, when non-zero, explains an empty result's cause so the
	// retry loop records the matching failover reason (sticky policy + logging).
	FailoverReason StreamFailoverReason
}

// UpstreamAction holds the result of a single upstream HTTP request attempt.
// The Kind field distinguishes between streaming, response, and error outcomes.
// Release frees the per-model concurrency slot held for the round-trip; it is
// invoked at every terminal handling point and is idempotent.
type UpstreamAction struct {
	Kind    int
	Resp    *http.Response
	Body    []byte
	Cancel  context.CancelFunc
	Release func()
	// Retryable marks an ActionContinue that carried a transient local
	// failure (network error) eligible for a same-target retry when no
	// later target remains. Guard/skip actions leave it false.
	Retryable bool
	// SkipReason names why a guard skipped the target ("provider_budget",
	// "rate_limit", …), so the loop can report the reason that mattered when
	// every target was skipped (NENYA-188).
	SkipReason string
	// DispatchAt is when the upstream request was sent (NENYA-178): the start
	// of the time-to-first-effective-output clock the streaming path records.
	DispatchAt time.Time
	// UpstreamAttempted marks an action that performed an HTTP round-trip
	// (any outcome of a successful Do: stream, response, upstream error, HTML
	// rejection, network failure). The retry loop uses it to avoid reporting a
	// budget exhaustion when a target actually reached a provider (NENYA-188).
	UpstreamAttempted bool
}

// ReleaseSlot frees the concurrency slot held by this action, if any. The
// release function is Once-wrapped at acquire time, so double invocation is
// safe.
func (a UpstreamAction) ReleaseSlot() {
	if a.Release != nil {
		a.Release()
	}
}

// Upstream action kinds mirror the parent's constants (same values; the
// parent keeps its own spellings via aliases).
const (
	ActionContinue = iota
	ActionError
	ActionStream
)

// RetrySignal controls the outer loop from action handlers.
type RetrySignal int

const (
	// RetrySignalContinue advances to the next target (failover).
	RetrySignalContinue RetrySignal = iota
	// RetrySignalRetry is a transient failure eligible for a same-target
	// re-attempt when no later target remains.
	RetrySignalRetry
	RetrySignalBreak
	RetrySignalDone
)

// EgressScreenRuntime carries the per-request egress screen state: the
// judge (nil = screen disabled), whether strict action is configured,
// and the one-judgment-per-request budget. The runtime is created once
// per request and threaded through the forward options next to the canary
// result, so the tool-args and buffered checkpoints share the same
// single-judgment budget.
type EgressScreenRuntime struct {
	Judge  *pipeline.Judge
	Strict bool
	// Budget flips to true when the single judgment is spent; later
	// triggers in the same request keep their deterministic verdict.
	Budget atomic.Bool
}

// BufferedSSE holds a buffered streaming response from the upstream LLM.
// It contains raw SSE bytes, extracted tool calls, the assistant message,
// and metadata about the response.
type BufferedSSE struct {
	RawBytes         []byte
	ToolCalls        []MCPToolCall
	AssistantMessage map[string]any
	FinishReason     string
	HasContent       bool
	Model            string
	// ProviderName is the target that produced the buffer (NENYA-155): the
	// replay write's client-stall telemetry is labeled with it.
	ProviderName string
	// ExfilBlocked marks a buffer truncated by the egress guard: callers
	// must not replay it and should emit the structured block payload.
	ExfilBlocked bool
	// CanaryBlocked marks a buffer truncated by the canary tripwire
	// (routes to error_kind=exfil_detected instead of exfil_blocked).
	CanaryBlocked bool
	// StreamError marks a buffer carrying an in-stream error frame (NENYA-153):
	// the client is replayed the error, so the attempt must not be charged as a
	// breaker success.
	StreamError bool
	// EmptyResponse marks a buffer whose HTTP-200 carried NO SSE data frames
	// at all (empty body, keepalives only, or non-SSE junk): a failed upstream
	// attempt like any other — counted in the NENYA-160 degradation signal,
	// charged as a breaker failure, and failed over (AUD-16 / NENYA-258).
	EmptyResponse    bool
	ReasoningContent string
	// CalibrationModel is the serving target's model (set at construction
	// by HandleBufferedStream): the exact key the dispatch estimate for
	// this buffer was recorded under (NENYA-135).
	CalibrationModel string
	// ServingTarget is the target this buffer was dispatched to (NENYA-193):
	// cost and spend are attributed to its provider/account, which the model
	// string alone cannot identify when a chain lists one model under several
	// providers.
	ServingTarget routing.UpstreamTarget
	// usageScan caches the one-time parse of RawBytes: every data chunk
	// carrying a usage object, in order. The shared scan behind
	// UsageChunk/UsageTotals so multi-MB buffers are unmarshaled once
	// (NENYA-135, AUD-82 / NENYA-287). Contract: the usage accessors must
	// not be called before the egress/canary guards finish mutating
	// RawBytes; RawBytes is treated as immutable once parsed (current
	// ordering — guards inside ForwardBuffered, both keeping usage
	// frames — satisfies this).
	usageScan       []map[string]interface{}
	usageScanParsed bool
	// usageMerged caches the per-dimension high-water merge of every usage
	// object in the buffer (NENYA-167).
	usageMerged     stream.UsageTotals
	usageMergedSeen bool
	// UsageClaimed records that this buffer's usage was claimed by
	// RecordMCPIterationUsage (one claim per dispatch); the MCP terminal paths
	// check it and are therefore idempotent no-ops for a claimed buffer
	// (NENYA-193).
	UsageClaimed bool
}

// MCPToolCall represents a tool call extracted from an upstream LLM response.
type MCPToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	// ExtraContent carries the Gemini thought signature (extra_content) so
	// synthetic assistant tool_calls keep the model's signatures inline
	// (NENYA-51). Nil for providers that do not emit signatures.
	ExtraContent any
}

// ImmediateFlushWriter wraps an http.ResponseWriter and flushes after every
// Write call when the underlying writer supports http.Flusher.
type ImmediateFlushWriter struct {
	Dst     http.ResponseWriter
	Flusher http.Flusher
	// StallTimeout bounds a single client write (NENYA-155): re-armed before
	// every write/flush via the ResponseController, so only a write that
	// cannot complete within the window — a client that stopped reading —
	// trips it. A slow provider with nothing to send never arms a pending
	// write, so idle gaps never fire the stale deadline. 0 disables.
	StallTimeout time.Duration
}

// NewImmediateFlushWriter creates an ImmediateFlushWriter if the response writer
// supports http.Flusher. The boolean indicates whether Flusher was available.
// stallTimeout is the per-write client-stall deadline (0 disables).
func NewImmediateFlushWriter(w http.ResponseWriter, stallTimeout time.Duration) (*ImmediateFlushWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return &ImmediateFlushWriter{Dst: w, StallTimeout: stallTimeout}, false
	}
	return &ImmediateFlushWriter{Dst: w, Flusher: flusher, StallTimeout: stallTimeout}, true
}

// Header forwards to the wrapped ResponseWriter.
func (fw *ImmediateFlushWriter) Header() http.Header {
	return fw.Dst.Header()
}

// WriteHeader forwards to the wrapped ResponseWriter.
func (fw *ImmediateFlushWriter) WriteHeader(statusCode int) {
	fw.Dst.WriteHeader(statusCode)
}

// Write re-arms the per-write client-stall deadline (NENYA-155), writes
// through to the wrapped ResponseWriter, and flushes when available.
func (fw *ImmediateFlushWriter) Write(p []byte) (int, error) {
	ArmClientWriteDeadline(fw.Dst, fw.StallTimeout)
	n, err := fw.Dst.Write(p)
	if err == nil && fw.Flusher != nil {
		fw.Flusher.Flush()
	}
	return n, err
}

// ArmClientWriteDeadline re-arms the per-write client-stall deadline
// (NENYA-155). Best effort: writers that cannot set deadlines (httptest
// recorders, custom wrappers) return an error that is deliberately ignored.
func ArmClientWriteDeadline(w http.ResponseWriter, timeout time.Duration) {
	if timeout <= 0 || w == nil {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(timeout))
}

// IsWriteTimeout reports whether an error is a deadline/timeout from a write.
func IsWriteTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// SSEAccumulator accumulates SSE stream state during buffered reading,
// decomposing the high-cyclomatic-complexity bufferStreamResponse into
// single-responsibility methods.
type SSEAccumulator struct {
	Raw          strings.Builder
	FinishReason string
	HasContent   bool
	Content      strings.Builder
	Reasoning    strings.Builder
	Model        string
	Logger       *slog.Logger

	TCArgsAccum map[int]*strings.Builder
	TCNameAccum map[int]string
	TCIDAccum   map[int]string
	// TCExtraAccum carries Gemini thought signatures (extra_content) seen in
	// tool_call deltas so synthetic assistant messages keep them inline
	// (NENYA-51); without this the MCP loop's own history lost the model's
	// signatures and the sanitizer stripped the just-completed tool pairs.
	TCExtraAccum map[int]any
	TotalLines   int
	DataLines    int
	// StreamError marks an in-stream error frame (NENYA-153).
	StreamError bool
	// Format is the serving target's wire format (UpstreamTarget.Format).
	// Tool-call extraction is format-aware (AUD-10 / NENYA-218): the OpenAI
	// delta parse always runs, and the Anthropic content_block / Gemini
	// functionCall parsers feed the SAME index-keyed accumulators when the
	// target's format says so — pre-fix, a non-OpenAI-format target's tool
	// calls were invisible to the loop and replayed unexecuted.
	Format string
	// GeminiToolSeq assigns synthetic indices to Gemini's complete
	// functionCall parts (they arrive whole — no deltas to join).
	GeminiToolSeq int
}

// NewSSEAccumulator constructs an SSEAccumulator with its index-keyed
// accumulators initialized.
func NewSSEAccumulator(logger *slog.Logger) *SSEAccumulator {
	return &SSEAccumulator{
		TCArgsAccum:  make(map[int]*strings.Builder),
		TCNameAccum:  make(map[int]string),
		TCIDAccum:    make(map[int]string),
		TCExtraAccum: make(map[int]any),
		Logger:       logger,
	}
}

// VisionGate decides whether an MCP tool result's image blocks may ride to
// the routed target. The zero gate (no target context) drops every image: the
// pre-NENYA-149 behavior was text-only, so the zero value is fail-closed and
// never widens what a caller sends.
type VisionGate struct {
	Model    string
	Format   string
	Vision   bool
	Resolved bool
}

// VisionDrop records one dropped image with its reason.
type VisionDrop struct {
	Reason string
	Mime   string
}

// MCPExecOptions groups the MCP tool-call execution inputs (NENYA-162): the
// session's unavailable-tool set is set only by the MCP loop (nil elsewhere —
// the NENYA-162 non-injected-tool guard is then inert).
type MCPExecOptions struct {
	Ctx         context.Context
	Calls       []MCPToolCall
	GW          *gateway.NenyaGateway
	AgentName   string
	Canary      pipeline.CanarResult
	Screen      *EgressScreenRuntime
	APIKey      *config.ApiKey
	Unavailable map[string]bool
}

// MCPGuardDispatch groups the identity and payload of a non-registry
// MCP dispatch (auto-search, auto-save) for the argument guard.
type MCPGuardDispatch struct {
	ServerName string
	ToolName   string
	Purpose    string
	Args       map[string]any
	// APIKey is the authenticated key whose tool grant must cover the
	// dispatch (NENYA-183). Nil denies (fail closed): auto-search and
	// auto-save always run on behalf of an authenticated request.
	APIKey *config.ApiKey
}

// MCPAppendContext groups what AppendMCPResults needs beyond the payload, the
// calls, and the results themselves.
type MCPAppendContext struct {
	Settings  pipeline.SpotlightSettings
	Metrics   *infra.Metrics
	ToolIndex *mcp.ToolRegistry
	// Vision gates the result's image blocks on the routed target (NENYA-149);
	// the zero gate drops them.
	Vision VisionGate
	Logger *slog.Logger
}

// AutoSearchResult carries one completed auto-search outcome.
type AutoSearchResult struct {
	Text      string
	ToolName  string
	Duration  time.Duration
	Server    string
	AgentName string
}

// AutoSearchContextOpts groups the parameters for injecting auto-search
// memory context (AGENTS.md §11 parameter grouping).
type AutoSearchContextOpts struct {
	Payload    map[string]interface{}
	Messages   []interface{}
	ServerName string
	Result     *AutoSearchResult
	ToolName   string
	AgentName  string
	Settings   pipeline.SpotlightSettings
}
