package pseudonym

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Patcher replaces declared-term occurrences in text with deterministic
// pseudonyms scoped to a salt epoch. One Patcher per patch run: the run's
// Mapping() carries everything its Restorer needs.
//
// Matching rules (pinned by the package tests):
//   - word-boundary, case-insensitive exact match of the declared value;
//   - earliest occurrence wins; at the same start, an exact match beats an
//     inflected one — "Acme Corp's" patches the declared term and keeps
//     the possessive suffix natural;
//   - the exact alternation lists terms longest-first, so at one start the
//     longest term wins and a substring term never fragments a longer one;
//   - never inside a longer token (word boundary — "acme.community" is not
//     an "acme.com" hit; an email's domain tail patches mid-token, leaving
//     the mailbox readable);
//   - inflected/distinct-word forms ("Acme Corps") are detected, counted
//     unresolved, and left untouched.
//
// The matcher is two compiled alternation regexes (exact, inflected) over
// the whole table, merged in one linear pass — no per-position or
// per-match rescans.
type Patcher struct {
	table     *Table
	salt      []byte
	mapping   *Mapping
	exactRe   *regexp.Regexp
	inflectRe *regexp.Regexp
	// canonicalByLower resolves a matched span (lowercased) to the
	// table's canonical value.
	canonicalByLower map[string]string
}

// NewPatcher builds a patcher for one run. The salt scopes the pseudonym
// space: same salt -> same pseudonyms (cache-stable within the epoch),
// rotated salt -> different pseudonyms (cross-session isolation, cache
// bust). An empty salt is valid (tests) but operators should always set
// one; the harness generates a random default.
func NewPatcher(table *Table, salt []byte) *Patcher {
	p := &Patcher{
		table:            table,
		salt:             salt,
		canonicalByLower: make(map[string]string, len(table.terms)),
	}
	p.exactRe, p.inflectRe = compileTableRes(table.terms, p.canonicalByLower)
	return p
}

// compileTableRes builds the combined alternation matchers and fills the
// lowercase-term resolution map. Longest-first alternation ordering (the
// table's order) makes RE2 prefer the longest term at a shared start.
func compileTableRes(terms []Term, resolve map[string]string) (*regexp.Regexp, *regexp.Regexp) {
	exact := make([]string, 0, len(terms))
	inflected := make([]string, 0, len(terms))
	for _, t := range terms {
		quoted := regexp.QuoteMeta(t.Value)
		exact = append(exact, `\b`+quoted+`\b`)
		inflected = append(inflected, `\b`+quoted+`(?:'s|'S|s|es)\b`)
		resolve[strings.ToLower(t.Value)] = t.Value
	}
	if len(exact) == 0 {
		// An empty table matches nothing; Patch guards on nil matchers.
		return nil, nil
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(exact, "|") + `)`),
		regexp.MustCompile(`(?i)(?:` + strings.Join(inflected, "|") + `)`)
}

// PatchResult carries one patch run's output and accounting.
type PatchResult struct {
	// Text is the patched text.
	Text string
	// Stats is the run's occurrence accounting (the go/no-go inputs).
	Stats PatchStats
}

// PatchStats is the per-run occurrence accounting. The go/no-go gate
// consumes Occurrences, Patched, CaseVariants and Unresolved:
// Occurrences = Patched + Unresolved, Patched = Exact + CaseVariants,
// ApplyRate = Patched / Occurrences.
type PatchStats struct {
	// Occurrences counts every detected declared-term occurrence
	// (patched or unresolved).
	Occurrences int
	// Patched counts occurrences replaced with a pseudonym.
	Patched int
	// Exact counts patched occurrences whose casing matched the declared
	// term exactly (these restore byte-exact).
	Exact int
	// CaseVariants counts patched occurrences whose casing differed from
	// the declared term (these restore to the declared casing — a
	// recorded fidelity loss).
	CaseVariants int
	// Unresolved counts detected inflected/possessive variants left
	// untouched (patching them would corrupt the word; language packs
	// are a later phase).
	Unresolved int
}

// ApplyRate returns Patched / Occurrences (1 when nothing was detected —
// vacuously perfect).
func (s PatchStats) ApplyRate() float64 {
	if s.Occurrences == 0 {
		return 1
	}
	return float64(s.Patched) / float64(s.Occurrences)
}

// UnresolvedRate returns Unresolved / Occurrences (0 when nothing was
// detected).
func (s PatchStats) UnresolvedRate() float64 {
	if s.Occurrences == 0 {
		return 0
	}
	return float64(s.Unresolved) / float64(s.Occurrences)
}

// Mapping is the run's pseudonym -> canonical-term table (the restorer's
// key material). It is not sensitive by itself (pseudonyms are derived
// from the salt), but WITH the salt it re-identifies everything — treat
// it like the salt: same store, same lifetime.
type Mapping struct {
	salt        string
	byPseudonym map[string]string
}

// Entries returns a copy of the pseudonym -> canonical-term table.
func (m *Mapping) Entries() map[string]string {
	out := make(map[string]string, len(m.byPseudonym))
	for k, v := range m.byPseudonym {
		out[k] = v
	}
	return out
}

// Mapping returns the run's pseudonym table. The restorer needs it.
func (p *Patcher) Mapping() *Mapping {
	return p.mapping
}

// acceptedSpan is one span the merge accepted: exact (patched) or
// inflected (counted unresolved, rendered verbatim).
type acceptedSpan struct {
	start, end int
	inflected  bool
}

// mergeSpans runs the two matcher passes and resolves them into the
// accepted-span stream: earliest start wins, an exact span displaces an
// inflected one at the same start, spans swallowed by an accepted span
// drop. Patch and CanonicalText both consume THIS stream — the expected
// post-restore rendering can never drift from what was actually patched.
func (p *Patcher) mergeSpans(text string) []acceptedSpan {
	if p.exactRe == nil {
		// Empty table: nothing is declared, nothing matches.
		return nil
	}
	eSpans := p.exactRe.FindAllStringIndex(text, -1)
	iSpans := p.inflectRe.FindAllStringIndex(text, -1)
	spans := make([]acceptedSpan, 0, len(eSpans)+len(iSpans))
	ei, ii := 0, 0
	for ei < len(eSpans) || ii < len(iSpans) {
		var start, end int
		inflected := false
		if ii >= len(iSpans) || (ei < len(eSpans) && eSpans[ei][0] <= iSpans[ii][0]) {
			start, end = eSpans[ei][0], eSpans[ei][1]
			ei++
		} else {
			start, end = iSpans[ii][0], iSpans[ii][1]
			inflected = true
			ii++
		}
		if len(spans) > 0 && start < spans[len(spans)-1].end {
			// Swallowed by an already-accepted span (the possessive case:
			// the exact term was accepted, its suffix span starts inside;
			// or an interior-word alias inside an accepted inflected
			// span).
			continue
		}
		spans = append(spans, acceptedSpan{start: start, end: end, inflected: inflected})
	}
	return spans
}

// Patch replaces every declared-term occurrence in text. Idempotent within
// a salt (the same text always yields the same output), so patching a
// conversation's turns keeps the patched prefix stable. Linear in the text
// length: two engine passes merged once (mergeSpans).
func (p *Patcher) Patch(text string) PatchResult {
	if p.mapping == nil {
		p.mapping = &Mapping{salt: string(p.salt), byPseudonym: map[string]string{}}
	}
	var b strings.Builder
	b.Grow(len(text) + 64)
	stats := PatchStats{}
	pos := 0
	for _, sp := range p.mergeSpans(text) {
		b.WriteString(text[pos:sp.start])
		matched := text[sp.start:sp.end]
		pos = sp.end
		if sp.inflected {
			b.WriteString(matched)
			stats.Occurrences++
			stats.Unresolved++
			continue
		}
		canonical := p.canonicalByLower[strings.ToLower(matched)]
		pseudo := p.PseudonymFor(canonical)
		b.WriteString(pseudo)
		p.mapping.byPseudonym[pseudo] = canonical
		stats.Occurrences++
		stats.Patched++
		if matched == canonical {
			stats.Exact++
		} else {
			stats.CaseVariants++
		}
	}
	b.WriteString(text[pos:])
	return PatchResult{Text: b.String(), Stats: stats}
}

// CanonicalText renders text with every PATCHED occurrence replaced by its
// canonical declared casing and unresolved spans left verbatim — the
// expected post-restore output of this run. The harness's RoundTrip
// classification compares restore output against THIS rendering: because
// it consumes the patcher's own accepted-span stream, an honestly-handled
// document (swallowed interior aliases, unresolved inflections) can never
// diverge and produce a false MISMATCH.
func (p *Patcher) CanonicalText(text string) string {
	var b strings.Builder
	b.Grow(len(text) + 64)
	pos := 0
	for _, sp := range p.mergeSpans(text) {
		b.WriteString(text[pos:sp.start])
		matched := text[sp.start:sp.end]
		pos = sp.end
		if sp.inflected {
			b.WriteString(matched)
			continue
		}
		b.WriteString(p.canonicalByLower[strings.ToLower(matched)])
	}
	b.WriteString(text[pos:])
	return b.String()
}

// PseudonymFor renders the deterministic pseudonym of a canonical declared
// term: <PREFIX>-<hex12> keyed on (salt, kind, lowercase term). Domains
// render DOM-<hex12>.invalid — a leaked pseudonym domain cannot resolve.
func (p *Patcher) PseudonymFor(term string) string {
	t := p.tableTerm(term)
	kind := KindCustom
	if t != nil && t.Kind != "" {
		kind = t.Kind
	}
	sum := sha256.New()
	sum.Write(p.salt)
	sum.Write([]byte{0})
	sum.Write([]byte(strings.ToLower(string(kind))))
	sum.Write([]byte{0})
	sum.Write([]byte(strings.ToLower(term)))
	// 48 bits: collision odds are negligible at realistic declared-term
	// counts (a 40-bit id's birthday bound sat near 1.2M terms, and the
	// mapping's last-write-wins would silently redirect both terms'
	// restorations — the wider id keeps that failure far beyond plausible).
	id := hex.EncodeToString(sum.Sum(nil)[:6])
	switch kind {
	case KindDomain:
		return "DOM-" + id + ".invalid"
	case KindCompany:
		return "ORG-" + id
	case KindPerson:
		return "PER-" + id
	case KindIdentifier:
		return "SEC-" + id
	default:
		return "TERM-" + id
	}
}

func (p *Patcher) tableTerm(term string) *Term {
	lower := strings.ToLower(term)
	for i := range p.table.terms {
		if strings.ToLower(p.table.terms[i].Value) == lower {
			return &p.table.terms[i]
		}
	}
	return nil
}
