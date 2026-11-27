package pseudonym

import (
	"strings"
	"testing"
)

// C010 (NENYA-356) contract pins for the reversible-pseudonymization spike.
// Written before the implementation: a greenfield spike has no behavioral
// pre-state to pin, so these tests ARE the contract the harness and the
// eventual C020 wiring must satisfy. Every property the go/no-go gate
// measures (determinism, round-trip fidelity, apply/unresolved accounting)
// is pinned here.

func spikeTable() *Table {
	return NewTable([]Term{
		{Value: "Acme Corp", Kind: KindCompany},
		{Value: "acme.com", Kind: KindDomain},
		{Value: "Maria Silva", Kind: KindPerson},
		{Value: "sk-live-9f4a7b2c", Kind: KindIdentifier},
	})
}

// TestPatchDeterministicWithinSalt pins the cache-stability property: the
// same salt produces IDENTICAL patched output across calls and instances —
// the property provider prompt caches need (same prefix bytes on every
// turn of a session).
func TestPatchDeterministicWithinSalt(t *testing.T) {
	text := "Acme Corp ships acme.com. Maria Silva owns the sk-live-9f4a7b2c key."
	first := NewPatcher(spikeTable(), []byte("salt-epoch-1")).Patch(text)
	second := NewPatcher(spikeTable(), []byte("salt-epoch-1")).Patch(text)
	if first.Text != second.Text {
		t.Fatalf("same salt produced different patched output:\n%s\n%s", first.Text, second.Text)
	}
	if first.Text == text {
		t.Fatal("patch left every declared term in place")
	}
}

// TestSaltRotationChangesPseudonyms pins the cache-bust property: rotating
// the salt changes every pseudonym (the cross-session isolation boundary).
func TestSaltRotationChangesPseudonyms(t *testing.T) {
	text := "Acme Corp ships acme.com."
	a := NewPatcher(spikeTable(), []byte("salt-epoch-1")).Patch(text)
	b := NewPatcher(spikeTable(), []byte("salt-epoch-2")).Patch(text)
	if a.Text == b.Text {
		t.Fatalf("salt rotation changed nothing:\n%s", a.Text)
	}
}

// TestRoundTripByteExactOnExactCase pins the core fidelity property: an
// occurrence whose casing matches the declared term restores byte-exact —
// restore(patch(x)) == x for exact-case documents.
func TestRoundTripByteExactOnExactCase(t *testing.T) {
	text := "Acme Corp ships acme.com. Maria Silva owns the sk-live-9f4a7b2c key."
	p := NewPatcher(spikeTable(), []byte("salt-epoch-1"))
	patched := p.Patch(text)
	r := NewRestorer(p.Mapping())
	restored := r.Restore(patched.Text)
	if restored.Text != text {
		t.Fatalf("round trip lost bytes:\n got: %s\nwant: %s", restored.Text, text)
	}
	if restored.Restored != 4 {
		t.Fatalf("restored %d occurrences, want 4", restored.Restored)
	}
}

// TestCaseVariantRestoreCanonical documents the recorded fidelity loss: a
// case-variant occurrence patches to the same deterministic pseudonym and
// restores to the DECLARED casing — byte-exact only for the declared form.
// The go/no-go gate measures how often real corpora hit this.
func TestCaseVariantRestoreCanonical(t *testing.T) {
	text := "acme corp ships acme.com."
	p := NewPatcher(spikeTable(), []byte("salt-epoch-1"))
	patched := p.Patch(text)
	if patched.Stats.CaseVariants == 0 {
		t.Fatalf("case variant not detected: %+v", patched.Stats)
	}
	r := NewRestorer(p.Mapping())
	restored := r.Restore(patched.Text)
	if restored.Text != "Acme Corp ships acme.com." {
		t.Fatalf("case variant restored to %q, want the declared casing", restored.Text)
	}
}

// TestInflectedVariantUnresolved pins the honest accounting: a distinct
// word built on a declared term ("Acme Corps" — the inflection pattern
// term+suffix) is DETECTED, counted unresolved, and left untouched —
// patching inside it would corrupt the word. A possessive of the declared
// term itself ("Acme Corp's") is the OPPOSITE case: the exact term matches
// and patches, the suffix stays natural.
func TestInflectedVariantUnresolved(t *testing.T) {
	text := "Acme Corps runs the show. Acme Corp ships. Acme Corp's API."
	p := NewPatcher(spikeTable(), []byte("salt-epoch-1"))
	patched := p.Patch(text)
	if patched.Stats.Unresolved != 1 {
		t.Fatalf("inflected variant not counted unresolved: %+v", patched.Stats)
	}
	if !strings.Contains(patched.Text, "Acme Corps") {
		t.Fatalf("inflected form was patched (would corrupt the word): %s", patched.Text)
	}
	org := p.PseudonymFor("Acme Corp")
	if n := strings.Count(patched.Text, org); n != 2 {
		t.Fatalf("expected the two exact occurrences patched, got %d: %s", n, patched.Text)
	}
	if !strings.Contains(patched.Text, org+"'s") {
		t.Fatalf("possessive suffix lost: %s", patched.Text)
	}
}

// TestWordBoundaryNoPartialMatch pins that matching never splits a single
// token: "AcmeCorp" (one word) is not an "Acme" hit. A two-word phrase is
// fair game — the first word of "Acme Corporation" is a real occurrence
// of the declared short name.
func TestWordBoundaryNoPartialMatch(t *testing.T) {
	table := NewTable([]Term{{Value: "Acme", Kind: KindCompany}})
	p := NewPatcher(table, []byte("s"))
	patched := p.Patch("AcmeCorp built tools for Acme.")
	if !strings.Contains(patched.Text, "AcmeCorp") {
		t.Fatalf("partial match split a single token: %s", patched.Text)
	}
	if n := strings.Count(patched.Text, p.PseudonymFor("Acme")); n != 1 {
		t.Fatalf("expected exactly one patched occurrence, got %d: %s", n, patched.Text)
	}
}

// TestOverlappingTermsLongestFirst pins that overlapping declared terms
// patch longest-first ("Acme Corp" before "Acme"), so a substring term
// never fragments a longer one.
func TestOverlappingTermsLongestFirst(t *testing.T) {
	table := NewTable([]Term{
		{Value: "Acme", Kind: KindCompany},
		{Value: "Acme Corp", Kind: KindCompany},
	})
	p := NewPatcher(table, []byte("s"))
	patched := p.Patch("Acme Corp and Acme differ.")
	orgLong := p.PseudonymFor("Acme Corp")
	orgShort := p.PseudonymFor("Acme")
	if !strings.Contains(patched.Text, orgLong) || !strings.Contains(patched.Text, orgShort) {
		t.Fatalf("longest-first patch broken: %s", patched.Text)
	}
	if strings.Contains(patched.Text, "Acme Corp") {
		t.Fatalf("longer term left in place: %s", patched.Text)
	}
}

// TestRestoreUnknownPseudonymUntouched pins that a restorer without the
// run's mapping leaves text untouched (restore is not destructive).
func TestRestoreUnknownPseudonymUntouched(t *testing.T) {
	r := NewRestorer(nil)
	text := "ORG-0123456789 stays as-is."
	if got := r.Restore(text); got.Text != text {
		t.Fatalf("restore without mapping mutated text: %s", got.Text)
	}
}

// TestApplyRateAccounting pins the go/no-go arithmetic on a known fixture:
// Occurrences = Patched + Unresolved, Patched = Exact + CaseVariants,
// and ApplyRate = Patched / Occurrences.
func TestApplyRateAccounting(t *testing.T) { // Patched: Acme Corp (exact), Maria Silva (exact), acme.com (exact),
	// acme Corp (case variant), Acme Corp's (exact, possessive suffix
	// kept). Unresolved: Acme Corps (distinct word). 6 occurrences.
	text := "Acme Corp hires Maria Silva. acme.com is theirs; acme Corp ships. Acme Corps tools. Acme Corp's API."
	p := NewPatcher(spikeTable(), []byte("s"))
	patched := p.Patch(text)
	s := patched.Stats
	if s.Occurrences != 6 || s.Patched != 5 || s.Exact != 4 || s.CaseVariants != 1 || s.Unresolved != 1 {
		t.Fatalf("accounting wrong: %+v", s)
	}
	if rate := s.ApplyRate(); rate < 0.82 || rate > 0.84 {
		t.Fatalf("apply rate = %v, want 5/6", rate)
	}
}

// TestCanonicalTextMatchesPatchSemantics pins the classifier contract the
// harness's RoundTrip classification depends on (C010 round 2): the
// expected post-restore rendering consumes the patcher's OWN accepted-span
// stream, so an honestly-handled document — a lowercase word-suffix
// inflection of a multi-word term accepted unresolved, with a declared
// interior-word alias whose exact span lies swallowed inside it — can
// never diverge from its restore output and produce a false MISMATCH.
func TestCanonicalTextMatchesPatchSemantics(t *testing.T) {
	table := NewTable([]Term{
		{Value: "Acme Corp Global", Kind: KindCompany},
		{Value: "Corp", Kind: KindCompany},
	})
	text := "acme corp globals went down; the corp dashboard held."
	p := NewPatcher(table, []byte("s"))
	patched := p.Patch(text)
	if patched.Stats.Unresolved != 1 || patched.Stats.Patched != 1 {
		t.Fatalf("fixture accounting wrong: %+v (want 1 unresolved + 1 patched)", patched.Stats)
	}
	r := NewRestorer(p.Mapping())
	restored := r.Restore(patched.Text)
	if restored.Text != p.CanonicalText(text) {
		t.Fatalf("honestly-handled document classified MISMATCH:\n restored:  %q\n canonical: %q", restored.Text, p.CanonicalText(text))
	}
	// And the round trip on a clean exact-case document stays byte-exact.
	clean := "Acme Corp Global ran; Corp held."
	p3 := NewPatcher(table, []byte("s"))
	patched3 := p3.Patch(clean)
	restored3 := NewRestorer(p3.Mapping()).Restore(patched3.Text)
	if restored3.Text != clean {
		t.Fatalf("clean exact-case round trip lost bytes: %q", restored3.Text)
	}
}
