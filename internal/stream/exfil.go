package stream

import (
	"log/slog"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
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
// only the current chunk's bytes are rewritten on strip — a construct
// whose head streamed earlier is left as inert partial text. Instances
// are per-response and not safe for concurrent use.
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
		hosts = append(hosts, strings.ToLower(strings.TrimSuffix(h, ".")))
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
// the (possibly rewritten) chunk content, the resulting filter action
// (Pass, Redact for strip, Block), and the violation reason. In log
// mode content always passes; in block mode the first violation flips
// the guard permanently (subsequent calls keep returning Block).
func (g *ExfilGuard) FilterContent(content string) (string, FilterAction, string) {
	if g.blocked {
		return content, ActionBlock, g.blockedReason
	}
	if content == "" {
		return content, ActionPass, ""
	}

	g.windowLen = AppendRuneWindow(&g.window, &g.windowLen, g.windowSize, content)
	windowStr := string(g.window)
	// Negative when the chunk exceeded the window (windowStr is a strict
	// suffix of content): the value is the true chunk offset of the
	// window start, and span-prevWindowBytes maps window coordinates
	// into content coordinates correctly. Do NOT clamp to zero.
	prevWindowBytes := len(windowStr) - len(content)

	spans, reason := g.scanWindow(windowStr, prevWindowBytes, len(content))
	if g.blocked {
		return content, ActionBlock, reason
	}
	if len(spans) == 0 {
		// Log mode (or nothing strippable): content passes untouched,
		// the reason carries the violation for observability.
		return content, ActionPass, reason
	}
	return spliceSpans(content, spans, exfilStripPlaceholder), ActionRedact, reason
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
	spans, reason := g.scanWindow(content, 0, len(content))
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
			// The construct completed entirely in earlier chunks; its
			// bytes already streamed and only the fragment is fixable.
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

	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if ip := net.ParseIP(host); ip != nil {
		if !g.allowIPLiterals {
			return exfilReasonIPLiteral
		}
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return exfilReasonPrivateIP
		}
	} else if len(g.allowedHosts) > 0 && !hostAllowed(host, g.allowedHosts) {
		return exfilReasonHostNotAllowed
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
