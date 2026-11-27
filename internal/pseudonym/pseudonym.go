// Package pseudonym implements the C010 spike (NENYA-356) for Module C
// (Reversible Pseudonymization): deterministic, salt-scoped pseudonyms for
// declared terms, a text patcher, and a restorer — with the metrics the
// go/no-go gate measures (apply rate, unresolved rate, round-trip fidelity,
// case fidelity).
//
// The spike is NOT wired into the request pipeline: C020 (declared-term
// pseudonyms on request/response text) is gated on the go/no-go decision
// this package's harness produces. The properties that matter for that
// decision and are pinned by the package tests:
//
//   - Determinism within a salt epoch: the same declared term always
//     renders the same pseudonym, so a session's patched prompts keep
//     byte-identical prefixes across turns — the property provider prompt
//     caches need.
//   - Salt rotation changes every pseudonym: the cross-session isolation
//     boundary (a rotated salt is also a cache bust, measured by the
//     harness's live protocol).
//   - Round-trip fidelity: occurrences whose casing matches the declared
//     term restore byte-exact; case variants restore to the declared
//     casing (a recorded fidelity loss the gate counts).
//   - Honest accounting: inflected/possessive variants are detected,
//     counted unresolved, and left untouched — patching them would corrupt
//     the text (that is later-phase language-pack work, C060).
//
// Zero external dependencies (stdlib only), per the project dependency
// policy. The benchmark harness lives in cmd/pseudonym-spike and the
// spike report in docs/SPIKE_REVERSIBLE_PSEUDONYM.md.
package pseudonym

import "strings"

// Kind classifies a declared term so its pseudonym renders in a plausible
// shape for the surface it appears on (ORG-… reads like a company token in
// code review prose; DOM-….invalid cannot resolve even if a model leaks it).
type Kind string

// The declared-term kinds the spike recognizes. The kinds are labels only —
// matching and replacement are kind-agnostic; the kind shapes the
// pseudonym prefix (and, for domains, adds a non-resolvable suffix).
const (
	KindCompany    Kind = "company"
	KindPerson     Kind = "person"
	KindDomain     Kind = "domain"
	KindIdentifier Kind = "identifier"
	KindCustom     Kind = "custom"
)

// Term is one declared term: the exact value to protect and its kind.
// Matching is case-insensitive against Value; Value itself is the
// canonical form restorations render.
type Term struct {
	Value string `json:"value"`
	Kind  Kind   `json:"kind"`
}

// Table is the ordered declared-term set of one pseudonymization domain.
// Terms are deduplicated case-insensitively and held longest-first so
// overlapping terms patch without fragmenting a longer one.
type Table struct {
	// terms longest-first; canonical (first-seen) casing kept.
	terms []Term
}

// NewTable builds a table from the declared terms. Duplicate values
// (case-insensitive) keep their first occurrence; the result is sorted
// longest-first. An empty table is valid and patches nothing.
func NewTable(terms []Term) *Table {
	seen := make(map[string]bool, len(terms))
	kept := make([]Term, 0, len(terms))
	for _, t := range terms {
		key := strings.ToLower(t.Value)
		if t.Value == "" || seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, t)
	}
	// Longest-first: an overlapping shorter term must never split a
	// longer one (pinned by TestOverlappingTermsLongestFirst).
	for i := 1; i < len(kept); i++ {
		for j := i; j > 0 && len(kept[j].Value) > len(kept[j-1].Value); j-- {
			kept[j], kept[j-1] = kept[j-1], kept[j]
		}
	}
	return &Table{terms: kept}
}

// Terms returns the table's terms in matching order (longest-first).
func (t *Table) Terms() []Term {
	if t == nil {
		return nil
	}
	out := make([]Term, len(t.terms))
	copy(out, t.terms)
	return out
}
