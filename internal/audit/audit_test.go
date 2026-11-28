package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// NENYA-362 (Module D Phase 010) pins: the tamper-evident redaction
// audit log. The chain is SHA-256 over a canonical field join with the
// previous entry's hash; verification walks the JSONL file and reports
// the FIRST broken line with a reason. Content (the secret, the text)
// must never reach the file — only structure.

// openW opens a Writer or fails the test.
func openW(t *testing.T, path string) *Writer {
	t.Helper()
	w, err := Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	return w
}

// rec records one entry or fails the test.
func rec(t *testing.T, w *Writer, agent, surface string, count int) {
	t.Helper()
	if err := w.Record(agent, surface, count); err != nil {
		t.Fatalf("record(%q,%s,%d): %v", agent, surface, count, err)
	}
}

// closeW closes a Writer or fails the test.
func closeW(t *testing.T, w *Writer) {
	t.Helper()
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// writeAuditFile writes bytes or fails the test.
func writeAuditFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestChainAppendAndVerify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	for i := 0; i < 5; i++ {
		rec(t, w, "agent-a", SurfaceBody, 3)
	}
	closeW(t, w)
	rep := VerifyFile(path)
	if !rep.OK {
		t.Fatalf("verify failed: %+v", rep)
	}
	if rep.Entries != 5 {
		t.Fatalf("entries = %d, want 5", rep.Entries)
	}
	if rep.FirstBad != 0 {
		t.Fatalf("FirstBad = %d, want 0", rep.FirstBad)
	}
}

func TestTamperedEntryDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	for i := 0; i < 4; i++ {
		rec(t, w, "agent-a", SurfaceArgs, 1)
	}
	closeW(t, w)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Flip the count on line 3 (1-based): entries 3 and 4 must fail
	// verification (the tampered hash and every hash that chains from it).
	lines := bytes.Split(raw, []byte("\n"))
	var e Entry
	if unmarshalErr := json.Unmarshal(lines[2], &e); unmarshalErr != nil {
		t.Fatalf("unmarshal: %v", unmarshalErr)
	}
	e.Count = 99
	mut, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	lines[2] = mut
	writeAuditFile(t, path, bytes.Join(lines, []byte("\n")))
	rep := VerifyFile(path)
	if rep.OK {
		t.Fatal("tampered chain verified OK")
	}
	if rep.FirstBad != 3 {
		t.Fatalf("FirstBad = %d, want 3 (the first tampered line)", rep.FirstBad)
	}
	if rep.Reason != ReasonChainMismatch {
		t.Fatalf("Reason = %q, want %q", rep.Reason, ReasonChainMismatch)
	}
}

func TestDeletedMiddleEntryDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	for i := 0; i < 4; i++ {
		rec(t, w, "agent-a", SurfaceBody, 1)
	}
	closeW(t, w)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := bytes.Split(raw, []byte("\n"))
	// Delete line 2 (1-based; index 1) and its newline.
	kept := append(append([][]byte{}, lines[:1]...), lines[2:]...)
	writeAuditFile(t, path, bytes.Join(kept, []byte("\n")))
	rep := VerifyFile(path)
	if rep.OK {
		t.Fatal("chain with deleted entry verified OK")
	}
	// Entry 2's line now carries seq 3 while the chain expects seq 2 —
	// a sequence gap surfaces at the deleted position.
	if rep.FirstBad != 2 {
		t.Fatalf("FirstBad = %d, want 2", rep.FirstBad)
	}
	if rep.Reason != ReasonSeqGap {
		t.Fatalf("Reason = %q, want %q", rep.Reason, ReasonSeqGap)
	}
}

func TestTornTailIsNotTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	for i := 0; i < 3; i++ {
		rec(t, w, "agent-a", SurfaceBody, 1)
	}
	closeW(t, w)
	// Simulate a crash mid-write: append a partial line without a newline.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"seq":4,"time`); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()
	rep := VerifyFile(path)
	if !rep.OK {
		t.Fatalf("torn tail reported as tampering: %+v", rep)
	}
	if !rep.TornTail {
		t.Fatal("torn tail not reported")
	}
	if rep.Entries != 3 {
		t.Fatalf("entries = %d, want 3 (torn bytes are not an entry)", rep.Entries)
	}
}

func TestReopenContinuesChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	rec(t, w, "agent-a", SurfaceBody, 2)
	// Simulate a reload: close and reopen against the same file.
	closeW(t, w)
	w2 := openW(t, path)
	if w2.last.Seq != 1 {
		t.Fatalf("reopened seq = %d, want 1", w2.last.Seq)
	}
	rec(t, w2, "agent-b", SurfaceArgs, 5)
	closeW(t, w2)
	rep := VerifyFile(path)
	if !rep.OK || rep.Entries != 2 {
		t.Fatalf("chain after reopen broken: %+v", rep)
	}
}

func TestOpenTruncatesTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	rec(t, w, "agent-a", SurfaceBody, 1)
	closeW(t, w)
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Crash remnant.
	writeAuditFile(t, path, append(good, []byte(`{"seq":2,"tim`)...))
	w2 := openW(t, path)
	rec(t, w2, "agent-a", SurfaceBody, 1)
	closeW(t, w2)
	rep := VerifyFile(path)
	if !rep.OK || rep.Entries != 2 {
		t.Fatalf("chain after torn-tail truncation broken: %+v", rep)
	}
	if !bytes.HasPrefix(mustRead(t, path), good) {
		t.Fatal("truncation disturbed the intact prefix")
	}
}

func TestRecordNeverWritesContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	rec(t, w, "agent-a", SurfaceBody, 2)
	closeW(t, w)
	raw := mustRead(t, path)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		t.Fatalf("entry does not decode as exactly the Entry shape: %v", err)
	}
	if e.Seq != 1 || e.Agent != "agent-a" || e.Surface != SurfaceBody || e.Count != 2 {
		t.Fatalf("entry fields drifted: %+v", e)
	}
	if e.PrevHash != GenesisPrev {
		t.Fatalf("genesis PrevHash = %q", e.PrevHash)
	}
	if len(e.Hash) != 64 {
		t.Fatalf("hash length = %d, want 64 hex chars", len(e.Hash))
	}
	// No trailing JSON value on the line (the file is one entry per line).
	if dec.More() {
		t.Fatal("line carries more than one JSON value")
	}
}

func TestAgentNameClipped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	rec(t, w, strings.Repeat("x", 5000), SurfaceBody, 1)
	closeW(t, w)
	raw := mustRead(t, path)
	if len(raw) > 2048 {
		t.Fatalf("clipped entry still large: %d bytes", len(raw))
	}
	rep := VerifyFile(path)
	if !rep.OK {
		t.Fatalf("clipped entry breaks chain: %+v", rep)
	}
}

func TestConcurrentRecordRace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if err := w.Record(fmt.Sprintf("agent-%d", n), SurfaceBody, 1); err != nil {
					t.Errorf("record: %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	closeW(t, w)
	rep := VerifyFile(path)
	if !rep.OK || rep.Entries != 80 {
		t.Fatalf("concurrent chain broken: entries=%d ok=%v firstBad=%d", rep.Entries, rep.OK, rep.FirstBad)
	}
	// Sequence numbers must be exactly 1..80 — the mutex orders them.
	seqs := map[uint64]bool{}
	lines := bytes.Split(bytes.TrimRight(mustRead(t, path), "\n"), []byte("\n"))
	for _, ln := range lines {
		var e Entry
		if err := json.Unmarshal(ln, &e); err != nil {
			t.Fatalf("line: %v", err)
		}
		if seqs[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seqs[e.Seq] = true
	}
}

func TestChainShapeAnchor(t *testing.T) {
	// A generation anchor: the hash of a fixed entry is a pinned constant,
	// so a canonical-format change (field order, join character) cannot
	// slip in silently and invalidate existing logs.
	prev := strings.Repeat("00", sha256.Size)
	e := Entry{Seq: 1, TimeUnix: 1700000000, Agent: "a", Surface: SurfaceBody, Count: 1, PrevHash: prev}
	h := entryHash(e)
	want := "3c57fd463e88c636e80026424d34117fd24aaca9e7237100172c9db3484049dc"
	if hex.EncodeToString(h) != want {
		t.Fatalf("canonical hash drifted: got %s want %s", hex.EncodeToString(h), want)
	}
}

// TestOpenRefusesCorruptedEntries pins the tamper-evidence contract
// (round-1 MED): Open truncates ONLY a torn crash tail — corrupted
// COMPLETE entries (a tampered count, a deleted middle entry, garbage)
// make Open refuse the file so the evidence survives for the operator.
func TestOpenRefusesCorruptedEntries(t *testing.T) {
	build := func(t *testing.T) (string, []byte) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "audit.jsonl")
		w := openW(t, path)
		for i := 0; i < 3; i++ {
			rec(t, w, "agent-a", SurfaceBody, 1)
		}
		closeW(t, w)
		return path, mustRead(t, path)
	}

	t.Run("tampered count", func(t *testing.T) {
		path, raw := build(t)
		lines := bytes.Split(raw, []byte("\n"))
		var e Entry
		if err := json.Unmarshal(lines[1], &e); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		e.Count = 42
		mut, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		lines[1] = mut
		writeAuditFile(t, path, bytes.Join(lines, []byte("\n")))
		if _, err := Open(path); err == nil {
			t.Fatal("Open accepted a file with a tampered complete entry")
		}
	})

	t.Run("deleted middle entry", func(t *testing.T) {
		path, raw := build(t)
		lines := bytes.Split(raw, []byte("\n"))
		kept := append(append([][]byte{}, lines[:1]...), lines[2:]...)
		writeAuditFile(t, path, bytes.Join(kept, []byte("\n")))
		if _, err := Open(path); err == nil {
			t.Fatal("Open accepted a file with a deleted entry")
		}
	})

	t.Run("garbage line", func(t *testing.T) {
		path, raw := build(t)
		writeAuditFile(t, path, append(raw, []byte("not json\n")...))
		if _, err := Open(path); err == nil {
			t.Fatal("Open accepted a file with a garbage line")
		}
	})

	t.Run("blank lines are not evidence", func(t *testing.T) {
		path, raw := build(t)
		writeAuditFile(t, path, append(raw, []byte("\n\n")...))
		w := openW(t, path)
		rec(t, w, "agent-a", SurfaceBody, 1)
		closeW(t, w)
		if rep := VerifyFile(path); !rep.OK || rep.Entries != 4 {
			t.Fatalf("blank-line handling broke the chain: %+v", rep)
		}
	})
}

// TestRecordGuardsPins pins the closed-set guards (round-1 LOW): the
// unknown-surface rejection and the positive-count invariant.
func TestRecordGuardsPins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w := openW(t, path)
	if err := w.Record("agent-a", "sidechannel", 1); err == nil {
		t.Fatal("unknown surface accepted")
	}
	if err := w.Record("agent-a", SurfaceBody, 0); err == nil {
		t.Fatal("zero count accepted")
	}
	if err := w.Record("agent-a", SurfaceBody, -3); err == nil {
		t.Fatal("negative count accepted")
	}
	closeW(t, w)
	// None of the rejected calls may have touched the file.
	if rep := VerifyFile(path); !rep.OK || rep.Entries != 0 {
		t.Fatalf("rejected calls wrote entries: %+v", rep)
	}
}

// TestVerifyFileErrorReport pins the open-failure report shape
// (round-1 NIT): a missing file is FileError, distinct from a
// malformed chain.
func TestVerifyFileErrorReport(t *testing.T) {
	rep := VerifyFile(filepath.Join(t.TempDir(), "missing.jsonl"))
	if rep.OK || !rep.FileError || rep.FirstBad != 0 {
		t.Fatalf("missing-file report: %+v", rep)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return raw
}
