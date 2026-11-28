package proxy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
)

// errDispatchBlocked is the sentinel the dispatch verification pass
// returns in block mode; callers map it to a structured 403
// (infra.ErrorKindDispatchBlocked) and — on the chat retry loop — to a
// terminal no-failover outcome: the content is what's blocked, another
// provider would be blocked identically.
var errDispatchBlocked = fmt.Errorf("dispatch verification blocked the request body")

// dispatchVerifyMatch is one matched span in the final body.
type dispatchVerifyMatch struct {
	start   int
	end     int
	pattern int // index into the gateway pattern slice — identity, never content
}

// verifyDispatchBody is the NENYA-363 dispatch verification pass: a
// final scan of the marshaled upstream body — after every transform —
// for secret patterns that survived all earlier stages. Disabled or
// pattern-less gateways pass the body through untouched. Actions:
//
//   - log (default): metric + warn, body unchanged;
//   - redact: matched spans replaced with the redaction label (the
//     config validation forbids labels containing quotes/backslashes so
//     the marshaled JSON stays valid); each replacement appends a
//     structure-only entry to the tamper-evident audit log (NENYA-362,
//     surface "dispatch") when the gateway carries the sink — the agent
//     identity when the funnel has one, empty on utility paths;
//   - block: errDispatchBlocked — nothing is sent upstream.
//
// Overlapping matches collapse to the first pattern's span, and a
// straddler (starting inside, extending past) is clipped to the
// unclaimed tail. The matched TEXT is never logged, counted into
// metrics, or passed to the audit sink — only span counts and pattern
// indices leave this function.
func (p *Proxy) verifyDispatchBody(gw *gateway.NenyaGateway, agentName, modelName string, body []byte) ([]byte, error) {
	if !gw.Config.Governance.EffectiveDispatchVerifyEnabled() || len(gw.SecretPatterns) == 0 || len(body) == 0 {
		return body, nil
	}
	action := gw.Config.Governance.EffectiveDispatchVerifyAction()
	matches := collectDispatchMatches(gw.SecretPatterns, body)
	if len(matches) == 0 {
		return body, nil
	}
	switch action {
	case config.DispatchActionBlock:
		gw.Metrics.RecordDispatchVerify(config.DispatchActionBlock)
		return nil, fmt.Errorf("%w (%d pattern match(es) in the final body)", errDispatchBlocked, len(matches))
	case config.DispatchActionRedact:
		out := redactDispatchMatches(body, matches, gw.Config.Bouncer.RedactionLabel)
		if !json.Valid(out) {
			// A quote-spanning pattern match can splice across a JSON
			// structural boundary; the guard never ships corrupted JSON —
			// fall back to the log outcome with the original body (the
			// metric records the EFFECTIVE outcome, log).
			gw.Logger.Warn("dispatch verification: redaction would corrupt the marshaled JSON; falling back to log",
				"model", modelName, "matches", len(matches))
			gw.Metrics.RecordDispatchVerify(config.DispatchActionLog)
			p.logDispatchVerify(gw, modelName, matches)
			return body, nil
		}
		gw.Metrics.RecordDispatchVerify(config.DispatchActionRedact)
		if gw.RedactionAudit != nil {
			gw.RedactionAudit.RecordRedactionEvent(agentName, "dispatch", len(matches))
		}
		return out, nil
	default: // log
		gw.Metrics.RecordDispatchVerify(config.DispatchActionLog)
		p.logDispatchVerify(gw, modelName, matches)
		return body, nil
	}
}

// dispatchVerifyBlocked is the verdict-only probe the utility handlers
// (embeddings, systemone) run before their retry budget: it answers
// exactly one question — would the dispatch verification pass BLOCK
// this body? — with NO side effects: no metric, no Warn, no audit
// entry. The pass whose body actually ships records exactly once; a
// probe that double-fired would write phantom duplicate entries into
// the tamper-evident audit chain (round-2 review MED).
func (p *Proxy) dispatchVerifyBlocked(gw *gateway.NenyaGateway, body []byte) bool {
	if !gw.Config.Governance.EffectiveDispatchVerifyEnabled() ||
		gw.Config.Governance.EffectiveDispatchVerifyAction() != config.DispatchActionBlock ||
		len(gw.SecretPatterns) == 0 || len(body) == 0 {
		return false
	}
	return len(collectDispatchMatches(gw.SecretPatterns, body)) > 0
}

// logDispatchVerify warns about survived secrets at the dispatch
// boundary. Only the count and pattern indices are logged — never the
// matched text.
func (p *Proxy) logDispatchVerify(gw *gateway.NenyaGateway, modelName string, matches []dispatchVerifyMatch) {
	patterns := make([]int, 0, len(matches))
	seen := map[int]bool{}
	for _, m := range matches {
		if !seen[m.pattern] {
			seen[m.pattern] = true
			patterns = append(patterns, m.pattern)
		}
	}
	gw.Logger.Warn("dispatch verification: secret pattern survived every earlier stage into the final upstream body",
		"model", modelName, "matches", len(matches), "pattern_indices", patterns)
}

// collectDispatchMatches scans body with every compiled pattern and
// returns non-overlapping spans ordered by start.
func collectDispatchMatches(patterns []*regexp.Regexp, body []byte) []dispatchVerifyMatch {
	var matches []dispatchVerifyMatch
	for i, re := range patterns {
		for _, loc := range re.FindAllIndex(body, -1) {
			matches = append(matches, dispatchVerifyMatch{start: loc[0], end: loc[1], pattern: i})
		}
	}
	sort.Slice(matches, func(a, b int) bool {
		if matches[a].start != matches[b].start {
			return matches[a].start < matches[b].start
		}
		return matches[a].end > matches[b].end
	})
	// First-wins de-overlap: an earlier (or longer, at equal start) span
	// claims its territory; a straddler (starts inside, extends past) is
	// CLIPPED to the unclaimed tail — its surviving portion still gets
	// processed, because dropping it whole would leave pattern-matched
	// bytes in the shipped body (round-1 review MED).
	kept := matches[:0]
	var cursor int
	for _, m := range matches {
		if m.end <= cursor {
			continue
		}
		if m.start < cursor {
			m.start = cursor
		}
		kept = append(kept, m)
		cursor = m.end
	}
	return kept
}

// redactDispatchMatches replaces the matched spans back-to-front so
// earlier offsets stay valid.
func redactDispatchMatches(body []byte, matches []dispatchVerifyMatch, label string) []byte {
	out := body
	for i := len(matches) - 1; i >= 0; i-- {
		m := matches[i]
		repl := append([]byte(nil), []byte(label)...)
		out = append(out[:m.start], append(repl, out[m.end:]...)...)
	}
	return out
}
