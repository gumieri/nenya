package pseudonym

import "strings"

// Restorer maps a patch run's pseudonyms back to their canonical declared
// terms on response text. Restore is not destructive: pseudonyms absent
// from the mapping pass through untouched, and text without pseudonyms is
// returned as-is.
//
// Byte-exact round-trip verification (restore(patch(x)) == x) belongs to
// the caller — the harness holds the original text — and case-fidelity
// accounting comes from PatchStats.CaseVariants at patch time (every
// case-variant patch restores to the declared casing; the restorer cannot
// recover the original casing from the patched text alone).
type Restorer struct {
	mapping *Mapping
}

// NewRestorer builds a restorer over one patch run's mapping. A nil
// mapping is valid and restores nothing (pinned by
// TestRestoreUnknownPseudonymUntouched).
func NewRestorer(mapping *Mapping) *Restorer {
	return &Restorer{mapping: mapping}
}

// RestoreResult carries one restore run's output and accounting.
type RestoreResult struct {
	// Text is the restored text.
	Text string
	// Restored counts pseudonym occurrences replaced with their canonical
	// term.
	Restored int
}

// Restore maps every known pseudonym in text back to its canonical term.
// The replacement is a single pass over all pseudonyms at once
// (strings.NewReplacer), so no canonical term that happens to embed
// another pseudonym's spelling is double-replaced and the occurrence
// count is order-free.
func (r *Restorer) Restore(text string) RestoreResult {
	if r == nil || r.mapping == nil || len(r.mapping.byPseudonym) == 0 {
		return RestoreResult{Text: text, Restored: 0}
	}
	pairs := make([]string, 0, len(r.mapping.byPseudonym)*2)
	restored := 0
	for pseudo, canonical := range r.mapping.byPseudonym {
		pairs = append(pairs, pseudo, canonical)
		restored += strings.Count(text, pseudo)
	}
	out := strings.NewReplacer(pairs...).Replace(text)
	return RestoreResult{Text: out, Restored: restored}
}
