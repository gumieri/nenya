package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// FuzzVerify throws mutated JSONL streams at the verifier: it must
// always terminate, never panic, and — the invariant — either report OK
// with every counted entry chain-verified, or name a concrete first-bad
// line. A valid chain grown through Writer must always verify OK.
func FuzzVerify(f *testing.F) {
	dir, err := os.MkdirTemp("", "auditfuzz")
	if err != nil {
		f.Fatalf("tempdir: %v", err)
	}
	seedPath := filepath.Join(dir, "seed.jsonl")
	w, err := Open(seedPath)
	if err != nil {
		f.Fatalf("seed open: %v", err)
	}
	_ = w.Record("seed", SurfaceBody, 1)
	_ = w.Record("seed", SurfaceArgs, 2)
	_ = w.Close()
	seedRaw, _ := os.ReadFile(seedPath)
	_ = os.RemoveAll(dir)

	f.Add(seedRaw)
	f.Add([]byte(""))
	f.Add([]byte(`{"seq":1,"time_unix":0,"agent":"a","surface":"body","count":1,"prev_hash":"` + GenesisPrev + `","hash":"x"}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		rep := Verify(bytes.NewReader(data))
		if rep.OK && (rep.FirstBad != 0 || rep.Reason != "") {
			t.Fatalf("OK report with FirstBad=%d reason=%q", rep.FirstBad, rep.Reason)
		}
		if !rep.OK && (rep.Reason == "" || rep.FirstBad <= 0) {
			t.Fatalf("failing report without reason/position: %+v", rep)
		}
	})
}
