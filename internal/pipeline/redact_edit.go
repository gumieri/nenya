package pipeline

import (
	"regexp"
	"sort"
	"strings"
)

// edit is one pending original-byte replacement shared by the span-
// mapping redaction passes (the JSON-aware args redactor, NENYA-348, and
// the confusable candidate pass, NENYA-349).
type edit struct{ start, end int }

// collectValidatedEdits runs one pattern over the mapped text and
// appends an edit for every match that passes the pattern's checksum
// validator (validator semantics identical to the flat pass:
// replaceValidated — the normalized match carries ASCII digits when a
// fold produced it). toOrig maps a mapped-text offset back to its
// original-byte span.
func collectValidatedEdits(re *regexp.Regexp, mapped string, edits []edit, toOrig func(int) (int, int)) []edit {
	validate, hasValidator := matchValidators[re.String()]
	for _, loc := range re.FindAllStringSubmatchIndex(mapped, -1) {
		start, end := loc[0], loc[1]
		if start < 0 || start >= end {
			continue
		}
		if hasValidator && !validate(mapped[start:end]) {
			continue
		}
		s, _ := toOrig(start)
		_, e := toOrig(end - 1)
		edits = append(edits, edit{s, e})
	}
	return edits
}

// applyEdits sorts edits by start ascending (end descending on ties, so
// two patterns matching at the same start deterministically keep the
// longer redaction under unstable sort.Slice) and splices the label over
// them first-wins-on-overlap.
func applyEdits(s string, edits []edit, label string) string {
	if len(edits) == 0 {
		return s
	}
	sort.Slice(edits, func(i, j int) bool {
		if edits[i].start != edits[j].start {
			return edits[i].start < edits[j].start
		}
		return edits[i].end > edits[j].end
	})
	var b strings.Builder
	last := 0
	for _, e := range edits {
		if e.start < last {
			continue
		}
		b.WriteString(s[last:e.start])
		b.WriteString(label)
		last = e.end
	}
	b.WriteString(s[last:])
	return b.String()
}
