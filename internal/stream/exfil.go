package stream

import (
	"encoding/json"
	"log/slog"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/netutil"
	"github.com/nenya/internal/pipeline"
)

// exfilStripPlaceholder replaces a stripped markdown image/link or bare
// URL when the egress guard runs in strip mode.
const exfilStripPlaceholder = "[NENYA: EXFIL LINK REMOVED]"

// exfilQueryEntropyBits is the Shannon-entropy threshold (bits/char)
// above which a query string is treated as smuggled data. Encoded
// payloads (base64/hex) measure 5.5-6.5; human query text stays under 4.5.
const exfilQueryEntropyBits = 5.0

// exfilMinEntropyQueryChars is the minimum query length for entropy
// evaluation: shorter queries produce too much noise.
const exfilMinEntropyQueryChars = 16

// exfilMaxTrackedURLs bounds the verdict memo per stream. Beyond the
// cap, new URLs bypass memoization (they are still evaluated and
// enforced; only the duplicate-suppression guarantee is lost).
const exfilMaxTrackedURLs = 4096

// exfilWindowRunes is the sliding-window size for straddling-construct
// detection (matches the StreamFilter default).
const exfilWindowRunes = 4096

// Exfil violation reasons (metric label values).
const (
	exfilReasonScheme         = "scheme"
	exfilReasonIPLiteral      = "ip_literal"
	exfilReasonPrivateHost    = "private_host"
	exfilReasonPrivateIP      = "private_ip"
	exfilReasonHostNotAllowed = "host_not_allowed"
	exfilReasonQueryLength    = "query_length"
	exfilReasonQueryEntropy   = "query_entropy"
)

// Markdown image ![alt](url) and link [text](url) constructs, plus bare
// http(s) URLs. The markdown URL group forbids whitespace and the closing
// paren; bare URLs end at whitespace or a markdown/HTML delimiter.
var (
	// markdownLinkRe matches both [text](url) and ![alt](url) forms: the
	// optional "!" is group 1, the URL group 2.
	markdownLinkRe = regexp.MustCompile(`(!?)\[[^\]]*\]\(([^)\s]+)[^)]*\)`)
	bareURLRe      = regexp.MustCompile(`(?i)https?://[^\s)\]>"'<>]+`)
)

// ExfilGuard applies deterministic URL policy to model output (markdown
// images/links and bare http(s) URLs) on the response path: the third
// leg of the lethal trifecta that neither client covers. It keeps a
// rune window over the stream (same mechanism as StreamFilter) so
// constructs split across SSE delta chunks are evaluated once complete;
// the whitespace-free tail that could still grow into a construct is
// withheld (NENYA-205) and re-evaluated prepended to the next delta, so
// a construct split across any chunking is spliced whole; Flush emits
// the held tail at stream end. Instances are per-response and not safe
// for concurrent use.
type ExfilGuard struct {
	action          string // config.ExfilAction* value
	allowedHosts    []string
	maxQueryChars   int
	allowIPLiterals bool

	window     []rune
	windowSize int
	windowLen  int
	// memo caches URL verdicts ("" = passed) so a construct straddling
	// chunks is evaluated exactly once; violations are re-spliced on
	// every occurrence without re-counting metrics.
	memo map[string]string

	metrics *infra.Metrics

	// pending holds the withheld whitespace-free tail (NENYA-205): text
	// that could still grow into a URL construct. Always a suffix of the
	// window and bounded by the window size (holdCut's force floor).
	pending []rune
	// drained is set by Flush: the stream is ending, FilterContent must
	// not re-hold (the synthetic flush chunk re-enters it).
	drained bool

	blocked       bool
	blockedReason string
}

// NewExfilGuard builds an egress guard from the effective config. The
// metrics receiver is nil-safe.
func NewExfilGuard(cfg config.ExfilGuardConfig, metrics *infra.Metrics) *ExfilGuard {
	action := cfg.Action
	if action == "" {
		action = config.ExfilActionLog
	}
	maxQueryChars := cfg.MaxQueryChars
	if maxQueryChars <= 0 {
		// Zero and negatives (rejected at the config boundary) fall back
		// to the default so direct construction cannot disable the check.
		maxQueryChars = config.DefaultExfilMaxQueryChars
	}
	allowIPLiterals := cfg.AllowIPLiterals != nil && *cfg.AllowIPLiterals
	hosts := make([]string, 0, len(cfg.AllowedHosts))
	for _, h := range cfg.AllowedHosts {
		// The same normalization the URL host gets (lowercase, every trailing
		// dot, IPv6 zone), so an entry cannot fail to match by spelling.
		hosts = append(hosts, netutil.NormalizeHost(h))
	}
	return &ExfilGuard{
		action:          action,
		allowedHosts:    hosts,
		maxQueryChars:   maxQueryChars,
		allowIPLiterals: allowIPLiterals,
		window:          make([]rune, 0, exfilWindowRunes),
		windowSize:      exfilWindowRunes,
		memo:            make(map[string]string),
		metrics:         metrics,
	}
}

// FilterContent evaluates one SSE delta against the policy. It returns
// the client-visible content for this delta, the resulting filter action
// (Pass, Redact for strip, Block), and the violation reason. The return
// can be SHORTER than the input on every action: the whitespace-free
// tail that could still grow into a URL construct is withheld (NENYA-205)
// and re-evaluated prepended to the next delta, so a construct split
// across any chunking is spliced whole instead of streaming its head.
// Callers must apply the return even on Pass. In log mode content passes
// modulo the hold; in block mode the first violation flips the guard
// permanently (subsequent calls keep returning Block; on Block the INPUT
// is returned and callers suppress the frame entirely — the return must
// not be delivered). Flush emits the held tail at stream end.
func (g *ExfilGuard) FilterContent(content string) (string, FilterAction, string) {
	if g.drained {
		// Flush already ran: the stream is ending and nothing more is
		// evaluated. The terminal flush re-enters this path with its own
		// synthetic chunk, which must pass through unmodified.
		return content, ActionPass, ""
	}
	if g.blocked {
		return content, ActionBlock, g.blockedReason
	}
	if content == "" {
		return content, ActionPass, ""
	}

	fresh := string(g.pending) + content
	g.pending = nil

	// Only the NEW bytes join the window: the held tail was absorbed on
	// arrival (it is a suffix of the window — holdCut bounds it by the
	// window size), so appending fresh would duplicate it.
	g.windowLen = AppendRuneWindow(&g.window, &g.windowLen, g.windowSize, content)
	windowStr := string(g.window)
	// Byte offset of fresh's start inside windowStr: the window head
	// before fresh is already-emitted text. Negative when the chunk
	// exceeded the window (windowStr is a strict suffix of fresh): the
	// value is the true offset of the window start, and
	// span-prevWindowBytes maps window coordinates into fresh coordinates
	// correctly. Do NOT clamp to zero.
	prevWindowBytes := len(windowStr) - len(fresh)

	spans, reason := g.scanWindow(windowStr, prevWindowBytes, len(fresh))
	if g.blocked {
		return content, ActionBlock, reason
	}

	cut := holdCut(fresh, spans, g.windowSize)
	g.pending = []rune(fresh[cut:])
	emit := fresh[:cut]
	if len(spans) == 0 {
		// Log mode (or nothing strippable): the proven-complete prefix
		// passes untouched, the reason carries the violation for
		// observability.
		return emit, ActionPass, reason
	}
	return spliceSpans(emit, clipSpansWithin(spans, cut), exfilStripPlaceholder), ActionRedact, reason
}

// Flush emits the guard's withheld tail at stream end (NENYA-205): the
// hold can no longer grow, so the tail is final. A bare URL running to
// the very end of the stream has no in-band terminator; it is evaluated
// with EOF standing in for one (the padding byte never joins the
// output). After Flush the guard is drained: FilterContent passes
// content through untouched. A blocked guard returns the tail with
// ActionBlock — callers suppress blocked content, so nothing held can
// outlive a block.
func (g *ExfilGuard) Flush() (string, FilterAction) {
	if g.drained {
		return "", ActionPass
	}
	g.drained = true
	out := string(g.pending)
	g.pending = nil
	if out == "" {
		return "", ActionPass
	}
	if g.blocked {
		return out, ActionBlock
	}
	// The window is `out` itself plus a padding byte standing in for the
	// URL terminator; prevWindowBytes is 0 because EVERY byte of out is
	// fresh content at Flush (a nonzero value shifts every span left and
	// lets the construct's final byte escape the placeholder — AUD-03).
	spans, _ := g.scanWindow(out+" ", 0, len(out))
	if g.blocked {
		// The EOF-terminated evaluation flipped the guard: the tail is
		// the violating construct — never deliver it.
		return "", ActionBlock
	}
	if len(spans) == 0 {
		return out, ActionPass
	}
	return spliceSpans(out, spans, exfilStripPlaceholder), ActionRedact
}

// Rearm re-enables content filtering after a Flush (NENYA-205/AUD-02). The
// pre-terminal drain composes the held tail while the stream may continue,
// so the drained pass-through must not become a permanent egress bypass: a
// spoofed mid-stream message_stop drains what is held at that instant and
// every later chunk is filtered again. The composition itself stays
// pass-through — Rearm runs only after the synthetic frame has been built.
func (g *ExfilGuard) Rearm() {
	g.drained = false
}

// holdCut returns the byte offset in fresh up to which content is
// client-visible this round. Base rule: the last whitespace boundary —
// a URL construct contains no whitespace, so everything before it is
// proven complete (whitespace bytes never occur inside a multi-byte
// rune, so the byte scan is rune-safe; a whitespace-free stream holds
// everything, bounded below). Refinements, in order:
//
//   - a detected span CROSSING the cut pulls it to the span's start (a
//     markdown construct's alt text may contain whitespace — the
//     construct is only emitted complete);
//   - a span starting exactly at the cut extends it to the span's end
//     (every span scanWindow returns is a COMPLETE construct — its
//     replacement is final and streams immediately instead of delaying
//     to the next chunk or Flush);
//   - the force floor keeps at most the last windowSize runes held:
//     pending stays a suffix of the window (the window sees everything
//     still withheld) and memory stays bounded on whitespace-free
//     streams (minified JSON). A span crossing the floor splices a
//     partial placeholder; the remainder re-splices next round or at
//     Flush — a duplicate placeholder, never a leaked head.
func holdCut(fresh string, spans []windowSpan, windowSize int) int {
	cut := 0
	for i := len(fresh) - 1; i >= 0; i-- {
		if c := fresh[i]; c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			cut = i + 1
			break
		}
	}
	for _, s := range spans {
		if s.start < cut && s.end > cut {
			cut = s.start
			break
		}
	}
	cut = extendCompletedSpans(fresh, spans, cut)
	if floor := len(fresh) - windowSize; floor > cut {
		// windowSize counts RUNES; walk back to the nearest rune start so
		// cut never splits a multi-byte rune (a split would corrupt the
		// emit and break pending's suffix-of-the-window invariant).
		for floor > 0 && !utf8.RuneStart(fresh[floor]) {
			floor--
		}
		if floor > cut {
			cut = floor
		}
	}
	return cut
}

// extendCompletedSpans grows the cut across spans starting exactly at it:
// every span is a COMPLETE construct, so its replacement is final and
// streams immediately instead of delaying to the next chunk or Flush.
// Adjacent constructs ("...(u1)[x](u2)...") chain.
func extendCompletedSpans(fresh string, spans []windowSpan, cut int) int {
	for {
		extended := false
		for _, s := range spans {
			if s.start == cut && s.end > cut && s.end <= len(fresh) {
				cut = s.end
				extended = true
				break
			}
		}
		if !extended {
			return cut
		}
	}
}

// clipSpansWithin drops spans entirely beyond cut and truncates spans
// crossing it (the crossing remainder is held and re-spliced next round
// or at Flush).
func clipSpansWithin(spans []windowSpan, cut int) []windowSpan {
	var out []windowSpan
	for _, s := range spans {
		if s.start >= cut {
			break
		}
		end := s.end
		if end > cut {
			end = cut
		}
		out = append(out, windowSpan{s.start, end})
	}
	return out
}

// exfilSurface records the wire shape of the last content-bearing chunk
// so the terminal flush (NENYA-205) can synthesize the held tail as a
// chunk format-identical to the stream that carried it.
type exfilSurface struct {
	anthropic bool   // content_block_delta (top-level delta) vs OpenAI chunk
	key       string // the model-text field the content was read from
	index     int    // Anthropic content-block index
	hasIndex  bool
}

// exfilSurfaceOf inspects a parsed chunk for the surface
// extractExfilContent reads from.
func exfilSurfaceOf(parsed map[string]interface{}) exfilSurface {
	if delta, ok := parsed["delta"].(map[string]interface{}); ok {
		s := exfilSurface{anthropic: true, key: exfilKeyOf(delta)}
		if idx, ok := parsed["index"].(float64); ok {
			s.index, s.hasIndex = int(idx), true
		}
		return s
	}
	if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				return exfilSurface{key: exfilKeyOf(delta)}
			}
		}
	}
	return exfilSurface{}
}

// exfilKeyOf returns the first model-text field carrying a non-empty
// string, defaulting to the OpenAI text surface.
func exfilKeyOf(delta map[string]interface{}) string {
	for _, key := range exfilContentKeys {
		if text, ok := delta[key].(string); ok && text != "" {
			return key
		}
	}
	return "content"
}

// buildExfilFlushChunk encodes held text as a synthetic content chunk in
// the recorded surface's wire shape, falling back to the OpenAI text
// surface when nothing was recorded (unreachable in practice: the hold
// requires prior content).
func buildExfilFlushChunk(s exfilSurface, text string) []byte {
	payload := map[string]interface{}{}
	switch {
	case s.anthropic && s.key == "thinking":
		payload["type"] = "content_block_delta"
		if s.hasIndex {
			payload["index"] = s.index
		}
		payload["delta"] = map[string]interface{}{"type": "thinking_delta", "thinking": text}
	case s.anthropic:
		payload["type"] = "content_block_delta"
		if s.hasIndex {
			payload["index"] = s.index
		}
		payload["delta"] = map[string]interface{}{"type": "text_delta", "text": text}
	case s.key == "reasoning_content":
		payload["choices"] = []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{"reasoning_content": text}}}
	default:
		payload["choices"] = []interface{}{map[string]interface{}{
			"index": 0, "delta": map[string]interface{}{"content": text}}}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return encoded
}

// FlushExfilHeldFrame drains a guard's held tail at stream end and
// encodes it as a synthetic SSE data frame in the shape of lastParsed
// (the last content-bearing chunk of the stream). Returns nil when
// nothing is held or the guard blocked. Exported for the buffered MCP
// replay path, which applies the guard outside the streaming reader.
func FlushExfilHeldFrame(guard *ExfilGuard, lastParsed map[string]interface{}) []byte {
	if guard == nil {
		return nil
	}
	held, action := guard.Flush()
	if held == "" || action == ActionBlock {
		return nil
	}
	encoded := buildExfilFlushChunk(exfilSurfaceOf(lastParsed), held)
	if encoded == nil {
		return nil
	}
	return append(append([]byte("data: "), encoded...), '\n')
}

// InspectFull evaluates complete output (non-streaming responses). It
// returns the rewritten text when the guard strips, and the first
// violation reason (empty when clean). Window state is not used: the
// full text is available at once.
func (g *ExfilGuard) InspectFull(content string) (string, FilterAction, string) {
	if g.blocked {
		return content, ActionBlock, g.blockedReason
	}
	if content == "" {
		return content, ActionPass, ""
	}
	// The padding byte stands in for the URL terminator at end-of-content
	// (AUD-03): without it a bare violating URL running to the very end of
	// a buffered surface has no in-band terminator and is skipped by the
	// terminator rule — delivered unstripped. prevWindowBytes is 0 because
	// every byte of content is fresh; the padding can never join a span
	// (bareURLRe excludes whitespace, a markdown link must close with ')').
	spans, reason := g.scanWindow(content+" ", 0, len(content))
	if g.blocked {
		return content, ActionBlock, reason
	}
	if len(spans) == 0 {
		return content, ActionPass, reason
	}
	return spliceSpans(content, spans, exfilStripPlaceholder), ActionRedact, reason
}

// IsBlocked reports whether the guard terminated the stream.
func (g *ExfilGuard) IsBlocked() bool { return g.blocked }

// ExtractExfilContent pulls the model-text surface from a parsed SSE
// chunk (either wire format). Exported for the buffered MCP replay path.
func ExtractExfilContent(parsed map[string]interface{}) string {
	return extractExfilContent(parsed)
}

// SetExfilContent writes rewritten text back to the extracted field.
// Exported for the buffered MCP replay path.
func SetExfilContent(parsed map[string]interface{}, newContent string) bool {
	return setExfilContent(parsed, newContent)
}

// exfilContentKeys is the model-text field priority shared by
// extraction and write-back, so the rewritten value always lands in the
// exact field the original was read from.
var exfilContentKeys = []string{"content", "text", "thinking", "reasoning_content"}

// extractExfilContent pulls the model-text surface from a parsed SSE
// chunk in BOTH wire formats: OpenAI (choices[0].delta.*) and Anthropic
// (content_block_delta top-level delta). Returns "" when the chunk
// carries no model text so non-content events (pings, tool_calls,
// usage) skip evaluation.
func extractExfilContent(parsed map[string]interface{}) string {
	if parsed == nil {
		return ""
	}
	if delta := sseDelta(parsed); delta != nil {
		for _, key := range exfilContentKeys {
			if text, ok := delta[key].(string); ok && text != "" {
				return text
			}
		}
	}
	return ""
}

// sseDelta locates the delta object in either wire format: Anthropic
// content_block_delta carries it at top level, OpenAI chunks inside
// choices[0].
func sseDelta(parsed map[string]interface{}) map[string]interface{} {
	if delta, ok := parsed["delta"].(map[string]interface{}); ok {
		return delta
	}
	if choices, ok := parsed["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := choice["delta"].(map[string]interface{}); ok {
				return delta
			}
		}
	}
	return nil
}

// setExfilContent writes rewritten text back to the field extracted by
// extractExfilContent, mutating the chunk in place (no marshal; the
// reader re-encodes). Returns false when no known field is present.
func setExfilContent(parsed map[string]interface{}, newContent string) bool {
	if parsed == nil {
		return false
	}
	if delta := sseDelta(parsed); delta != nil {
		for _, key := range exfilContentKeys {
			if text, ok := delta[key].(string); ok && text != "" {
				delta[key] = newContent
				return true
			}
		}
	}
	return false
}

// scanWindow finds violating URL constructs whose fresh bytes lie in
// [prevWindowBytes, len(windowStr)) and returns their spans in content
// coordinates (window minus prevWindowBytes, clamped) plus the first
// violation reason. Each unique violation is counted once in metrics;
// in block mode the first violation terminates: the guard flips to
// blocked and the caller observes it via IsBlocked.
func (g *ExfilGuard) scanWindow(windowStr string, prevWindowBytes, contentLen int) ([]windowSpan, string) {
	var spans []windowSpan
	reason := ""
	for _, hit := range g.extractTargets(windowStr, prevWindowBytes) {
		verdict, cached := g.memo[hit.url]
		if !cached {
			verdict = g.evaluate(hit.url)
			if len(g.memo) < exfilMaxTrackedURLs {
				g.memo[hit.url] = verdict
			}
		}
		if verdict == "" {
			continue
		}
		if !cached {
			g.metrics.RecordExfilDetection(verdict, g.action)
		}
		if g.action == config.ExfilActionBlock {
			g.blocked = true
			g.blockedReason = verdict
			return nil, verdict
		}
		start := hit.span.start - prevWindowBytes
		if start < 0 {
			start = 0
		}
		end := hit.span.end - prevWindowBytes
		if end > contentLen {
			end = contentLen
		}
		if start >= end {
			// Defense-in-depth: `consider` already filters targets whose
			// fresh bytes lie entirely outside this window, so the mapping
			// above cannot produce an empty span on any current call path
			// (FilterContent, Flush, InspectFull). Kept as a guard against
			// future callers.
			continue
		}
		if g.action == config.ExfilActionStrip {
			spans = append(spans, windowSpan{start, end})
		}
		if reason == "" {
			reason = verdict
		}
	}
	sort.SliceStable(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	return spans, reason
}

// exfilTarget is a complete URL construct found in the window with its
// span in window coordinates.
type exfilTarget struct {
	url  string
	span windowSpan
}

// extractTargets finds URL constructs in the window whose END lies
// within the fresh chunk (constructed by it). Spans are in window
// coordinates; InspectFull passes 0 for prevWindowBytes so every
// construct counts as fresh. Bare URLs are only considered when
// TERMINATED (followed by whitespace or a delimiter in the window): a
// still-growing prefix streamed across chunks would otherwise be
// evaluated (and memoized, and counted) once per chunk.
func (g *ExfilGuard) extractTargets(windowStr string, prevWindowBytes int) []exfilTarget {
	var targets []exfilTarget
	// URL-group spans of markdown constructs: bare-URL dedupe range.
	var mdURLSpans []windowSpan
	consider := func(rawURL string, start, end int) {
		if end <= prevWindowBytes || start >= end {
			return
		}
		targets = append(targets, exfilTarget{url: rawURL, span: windowSpan{start, end}})
	}

	for _, loc := range markdownLinkRe.FindAllStringSubmatchIndex(windowStr, -1) {
		// Group 1 covers an optional "!" so images and links share one
		// pass; the URL is group 2.
		mdURLSpans = append(mdURLSpans, windowSpan{loc[4], loc[5]})
		consider(windowStr[loc[4]:loc[5]], loc[0], loc[1])
	}
	for _, loc := range bareURLRe.FindAllStringIndex(windowStr, -1) {
		// Bare URLs inside a markdown URL group were already considered;
		// bare URLs in the markdown TEXT portion are separate targets.
		if overlaps(mdURLSpans, loc[0], loc[1]) {
			continue
		}
		if loc[1] >= len(windowStr) || !isURLTerminator(windowStr[loc[1]]) {
			continue
		}
		consider(windowStr[loc[0]:loc[1]], loc[0], loc[1])
	}
	return targets
}

// isURLTerminator reports whether the byte following a bare URL ends it
// (whitespace, markdown/HTML delimiters, or punctuation that cannot
// appear unencoded in a URL).
func isURLTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', ')', ']', '>', '"', '\'', ',', ';', '!':
		return true
	}
	return false
}

// overlaps reports whether [start,end) intersects an already-collected
// span.
func overlaps(spans []windowSpan, start, end int) bool {
	for _, s := range spans {
		if start < s.end && s.start < end {
			return true
		}
	}
	return false
}

// evaluate applies the URL policy: scheme, IP literals, host allowlist,
// and query length/entropy. Order matters for diagnostics: cheap
// structural checks first. An empty verdict means the URL passed.
func (g *ExfilGuard) evaluate(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return exfilReasonScheme
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return exfilReasonScheme
	}

	// Normalize before matching (NENYA-147): lowercase, strip every trailing
	// dot (not just one), cut an IPv6 zone, and recognize the legacy integer
	// IPv4 spellings (2130706433, 0x7f000001, 0177.0.0.1, 127.1) that OS
	// resolvers still accept — otherwise a private destination spelled that way
	// reads as an ordinary hostname and passes with no allowlist configured.
	host := netutil.NormalizeHost(u.Hostname())
	if ip, isLiteral := netutil.IsIPLiteral(host); isLiteral {
		if !g.allowIPLiterals {
			return exfilReasonIPLiteral
		}
		if netutil.DenyIP(ip) {
			return exfilReasonPrivateIP
		}
		return g.evaluateQuery(u.RawQuery)
	}
	// An explicitly allowed host wins over the built-in deny set.
	if !hostAllowed(host, g.allowedHosts) {
		// A special-use name or localhost variant (NENYA-147): the same
		// definition the MCP argument policy uses, so "http://db.internal/"
		// cannot pass as an ordinary hostname.
		if netutil.DenyHost(host) {
			return exfilReasonPrivateHost
		}
		if len(g.allowedHosts) > 0 {
			return exfilReasonHostNotAllowed
		}
	}
	return g.evaluateQuery(u.RawQuery)
}

// evaluateQuery applies the data-smuggling checks: query length and
// Shannon entropy. Empty verdict means the query passed.
func (g *ExfilGuard) evaluateQuery(rawQuery string) string {
	if len(rawQuery) > g.maxQueryChars {
		return exfilReasonQueryLength
	}
	if len(rawQuery) >= exfilMinEntropyQueryChars && pipeline.ShannonEntropy(rawQuery) > exfilQueryEntropyBits {
		return exfilReasonQueryEntropy
	}
	return ""
}

// hostAllowed matches a host against the allowlist: exact match or
// subdomain of an entry ("example.com" matches "api.example.com").
func hostAllowed(host string, allowed []string) bool {
	for _, entry := range allowed {
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// spliceSpans replaces content-coordinate spans with the placeholder in
// a single left-to-right pass; overlapping spans are merged by the
// last-writer-wins extension rule (same as StreamFilter redaction).
func spliceSpans(content string, spans []windowSpan, placeholder string) string {
	var b strings.Builder
	last := 0
	for _, s := range spans {
		if s.end <= last {
			continue
		}
		if s.start > last {
			b.WriteString(content[last:s.start])
		}
		b.WriteString(placeholder)
		if s.end > last {
			last = s.end
		}
	}
	if last < len(content) {
		b.WriteString(content[last:])
	}
	return b.String()
}

// CanaryWatcher is the per-request canary tripwire for the output
// stream: exact substring scan over the sliding window (chunk-boundary
// safe via the same rune window the egress guard uses). One watcher per
// response; not safe for concurrent use.
type CanaryWatcher struct {
	canary  string
	action  string // config.CanaryAction* value
	channel string // metric channel label for this watcher's surface
	window  []rune
	windowN int

	metrics *infra.Metrics
	logger  *slog.Logger

	// Tripped reports whether the canary was detected on this stream.
	Tripped bool
	// logged dedupes the one-time detection log line.
	logged bool
}

// NewCanaryWatcher builds a stream watcher for the given canary token.
// channel labels the metric (stream, buffered); logger receives the
// one-time detection warning in log mode (nil-safe). An empty token
// (guard disabled) yields an inert watcher.
func NewCanaryWatcher(canary, action, channel string, metrics *infra.Metrics, logger *slog.Logger) *CanaryWatcher {
	return &CanaryWatcher{
		canary:  canary,
		action:  action,
		channel: channel,
		window:  make([]rune, 0, exfilWindowRunes),
		metrics: metrics,
		logger:  logger,
	}
}

// FilterContent scans one SSE delta for the canary. In log mode content
// always passes; in block mode the first detection reports Block so the
// reader terminates the stream with ErrCanaryDetected.
func (w *CanaryWatcher) FilterContent(content string) (string, FilterAction, string) {
	if w.canary == "" || (w.Tripped && w.action == config.CanaryActionLog) {
		return content, ActionPass, ""
	}
	if content == "" {
		return content, ActionPass, ""
	}
	w.windowN = AppendRuneWindow(&w.window, &w.windowN, exfilWindowRunes, content)
	if w.windowN < len(w.canary) { // rune count vs byte len: canary is ASCII by construction
		return content, ActionPass, ""
	}
	if !pipeline.CanaryTripped(w.canary, string(w.window)) {
		return content, ActionPass, ""
	}
	if w.Tripped {
		// Defense in depth: current callers stop scanning after a block
		// or short-circuit log trips, so this branch is not reachable
		// today; kept so future callers cannot double-report.
		return content, ActionPass, ""
	}
	w.Tripped = true
	w.metrics.RecordExfilEvent(w.channel)
	if !w.logged {
		w.logged = true
		if w.logger != nil {
			w.logger.Warn("canary detected in output",
				"channel", w.channel, "action", w.action)
		}
	}
	if w.action == config.CanaryActionBlock {
		return content, ActionBlock, "canary detected in output"
	}
	return content, ActionPass, ""
}

// IsBlocked reports whether the watcher terminated the stream.
func (w *CanaryWatcher) IsBlocked() bool {
	return w.Tripped && w.action == config.CanaryActionBlock
}
