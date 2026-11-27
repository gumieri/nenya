package pseudonym

import (
	"strings"
	"testing"
)

// benchCorpus builds a deterministic synthetic document (~16 KiB) with the
// reference terms recurring at conversational density.
func benchCorpus() string {
	var b strings.Builder
	line := "Acme Corp asked Maria Silva whether acme.com traffic using sk-live-9f4a7b2c was rotated; Acme Corp's runbook said yes. "
	for b.Len() < 16<<10 {
		b.WriteString(line)
	}
	return b.String()
}

// BenchmarkPatchCorpus measures the patch pass (the request-side hot path
// C020 would pay). The patcher (and its regex compile) is hoisted out of
// the loop — the measurement is the patch itself; the mapping reaches its
// full size on the first iteration and stays stable after.
func BenchmarkPatchCorpus(b *testing.B) {
	text := benchCorpus()
	table := spikeTable()
	salt := []byte("bench-salt")
	p := NewPatcher(table, salt)
	b.SetBytes(int64(len(text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if r := p.Patch(text); r.Stats.Patched == 0 {
			b.Fatal("patched nothing")
		}
	}
}

// BenchmarkRestoreCorpus measures the restore pass (the response-side hot
// path C020 would pay).
func BenchmarkRestoreCorpus(b *testing.B) {
	text := benchCorpus()
	table := spikeTable()
	salt := []byte("bench-salt")
	p := NewPatcher(table, salt)
	patched := p.Patch(text)
	r := NewRestorer(p.Mapping())
	b.SetBytes(int64(len(patched.Text)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if out := r.Restore(patched.Text); out.Restored == 0 {
			b.Fatal("restored nothing")
		}
	}
}
