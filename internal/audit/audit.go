// Package audit implements the tamper-evident redaction audit log
// (NENYA-362): an append-only, hash-chained JSONL file recording THAT
// redactions happened — never what was redacted. Each entry hashes the
// previous entry's hash, so any edit, injection, or deletion inside the
// file breaks verification at the first affected line. The package is
// stdlib-only: it is wired in by cmd/nenya, which owns the file path
// and lifecycle (boot open, reload reuse, shutdown close).
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Surface names the redaction surface an entry covers. The set is
// closed so a typo cannot fork the file format.
const (
	SurfaceBody = "body"
	SurfaceArgs = "args"
)

// Verification failure reasons (Report.Reason). Empty when OK.
const (
	ReasonChainMismatch = "chain_mismatch"
	ReasonSeqGap        = "seq_gap"
	ReasonMalformed     = "malformed"
	ReasonBadGenesis    = "bad_genesis"
)

// GenesisPrev is the previous-hash anchor of the first entry in a file.
const GenesisPrev = "0000000000000000000000000000000000000000000000000000000000000000"

// maxAgentBytes bounds the agent identity stored per entry; names are
// config-resolved identities, but the writer defends its file format
// against hostile strings regardless.
const maxAgentBytes = 256

// MaxScanBytes caps the boot-time continuity scan. An audit file past
// this size refuses to open and asks for rotation — entries are small,
// so reaching the cap means years of traffic.
const MaxScanBytes = 64 << 20

// Entry is one audit record. Hash covers the first five fields plus
// PrevHash; the Hash field itself is the output, never an input.
type Entry struct {
	Seq      uint64 `json:"seq"`
	TimeUnix int64  `json:"time_unix"`
	Agent    string `json:"agent"`
	Surface  string `json:"surface"`
	Count    int    `json:"count"`
	PrevHash string `json:"prev_hash"`
	Hash     string `json:"hash"`
}

// entryHash computes the chain hash: SHA-256 over a canonical,
// version-stable field join. The format must never change casually —
// TestChainShapeAnchor pins it against a fixed digest.
func entryHash(e Entry) []byte {
	canonical := fmt.Sprintf("%d|%d|%s|%s|%d|%s",
		e.Seq, e.TimeUnix, e.Agent, e.Surface, e.Count, e.PrevHash)
	sum := sha256.Sum256([]byte(canonical))
	return sum[:]
}

// Report is the result of a chain verification walk.
type Report struct {
	// OK is true when every complete entry verified (a torn crash tail
	// does not break OK; it is reported separately).
	OK bool
	// Entries counts the complete entries that verified.
	Entries int
	// FirstBad is the 1-based line number of the first failure, 0 if none.
	FirstBad int
	// Reason is one of the Reason* constants, empty when OK.
	Reason string
	// TornTail reports a trailing partial line (a crash remnant — the
	// writer appends whole lines, so a non-newline-terminated tail is an
	// interrupted write, not tampering).
	TornTail bool
	// FileError reports that the file itself could not be read (missing,
	// unreadable) — distinct from a malformed chain: no line semantics
	// apply. VerifyFile only.
	FileError bool
}

// sanitizeAgent clips the agent identity to maxAgentBytes and replaces
// every rune outside the identity charset with '_' so the file format
// cannot carry control characters, newlines, or unbounded payloads.
func sanitizeAgent(agent string) string {
	var b []byte
	for _, r := range agent {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b = appendRune(b, r)
		default:
			b = append(b, '_')
		}
		if len(b) >= maxAgentBytes {
			break
		}
	}
	return string(b)
}

// appendRune appends r's UTF-8 encoding if it fits the budget.
func appendRune(b []byte, r rune) []byte {
	if len(b)+len(string(r)) > maxAgentBytes {
		return b
	}
	return append(b, string(r)...)
}

// Writer appends hash-chained entries to an audit file. All methods are
// safe for concurrent use; Record serializes under a mutex so sequence
// numbers and the chain are strictly ordered. The zero Writer is not
// usable — construct with Open.
type Writer struct {
	mu   sync.Mutex
	f    *os.File
	path string
	last Entry
}

// Open opens (creating if needed) the audit file and restores chain
// continuity: the writer resumes sequence numbers and hash chaining
// from the last valid entry, so a restart or configuration reload never
// forks the chain. A torn crash tail (partial final line — a valid
// entry always ends with its newline) is truncated with the intact
// prefix preserved. Complete but invalid entries are NEVER deleted:
// Open refuses the file (fatal at boot by the wiring) and names the
// first bad line, leaving the tamper evidence for the operator — run
// audit-verify before restarting a suspect instance. A file that fails
// continuity scanning past MaxScanBytes also refuses to open.
func Open(path string) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit open %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit stat %s: %w", path, err)
	}
	if info.Size() > MaxScanBytes {
		_ = f.Close()
		return nil, fmt.Errorf("audit file %s exceeds %d bytes; rotate before starting", path, MaxScanBytes)
	}
	last, keepBytes, scanErr := scanContinuity(f)
	if scanErr != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit scan %s: %w", path, scanErr)
	}
	_ = f.Close()
	// Reopen append-only and trim a torn tail so the next append lands
	// on a clean newline boundary.
	wf, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit open %s: %w", path, err)
	}
	if int64(keepBytes) != info.Size() {
		if err := wf.Truncate(int64(keepBytes)); err != nil {
			_ = wf.Close()
			return nil, fmt.Errorf("audit truncate %s: %w", path, err)
		}
	}
	return &Writer{f: wf, path: path, last: last}, nil
}

// scanContinuity walks the file and returns the last valid entry, the
// byte length of the intact newline-terminated prefix, and an error
// when any COMPLETE (newline-terminated) line fails validation — the
// tamper-evidence contract: Nenya never deletes corrupted entries, it
// refuses to operate and leaves the evidence for the operator (a torn
// crash tail — bytes without a trailing newline — is the only thing
// Open removes, because a valid entry always ends with its newline).
// prev starts as the virtual pre-genesis entry so the first real entry
// (PrevHash = GenesisPrev) verifies.
func scanContinuity(r io.Reader) (Entry, int, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxScanBytes+1))
	if err != nil {
		return Entry{}, 0, err
	}
	if len(raw) > MaxScanBytes {
		return Entry{}, 0, errors.New("file exceeds the continuity scan budget")
	}
	prev := Entry{Seq: 0, Hash: GenesisPrev}
	offset := 0
	keep := 0
	lineNo := 0
	corruptedAt := 0
	for offset < len(raw) {
		nl := bytes.IndexByte(raw[offset:], '\n')
		if nl < 0 {
			// Trailing bytes without a newline: torn tail, dropped.
			break
		}
		line := raw[offset : offset+nl]
		offset += nl + 1
		lineNo++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			if corruptedAt == 0 {
				corruptedAt = lineNo
			}
			continue
		}
		if !entryValid(e, prev) {
			if corruptedAt == 0 {
				corruptedAt = lineNo
			}
			continue
		}
		prev = e
		keep = offset
	}
	if corruptedAt != 0 {
		return Entry{}, 0, fmt.Errorf("invalid audit entry at line %d — run audit-verify and archive the file; Nenya never deletes corrupted entries", corruptedAt)
	}
	return prev, keep, nil
}

// entryValid reports whether e continues the chain from prev.
func entryValid(e, prev Entry) bool {
	if e.Seq != prev.Seq+1 || e.PrevHash != prev.Hash {
		return false
	}
	if e.PrevHash == GenesisPrev && prev.Seq != 0 {
		return false
	}
	return bytes.Equal(entryHash(e), mustUnhex(e.Hash))
}

func mustUnhex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// Record appends one entry. The caller passes only structure — the
// agent identity, the surface constant, and the replacement count; no
// redacted text or matched value can reach the file because no
// parameter carries it. A write failure returns an error for the caller
// to surface (metric + log); the request that produced the redaction is
// never failed by the audit path.
func (w *Writer) Record(agent string, surface string, count int) error {
	if surface != SurfaceBody && surface != SurfaceArgs {
		return fmt.Errorf("audit: unknown surface %q", surface)
	}
	if count <= 0 {
		return fmt.Errorf("audit: count must be positive, got %d", count)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	e := Entry{
		Seq:      w.last.Seq + 1,
		TimeUnix: time.Now().UTC().Unix(),
		Agent:    sanitizeAgent(agent),
		Surface:  surface,
		Count:    count,
		PrevHash: w.last.Hash,
	}
	if e.Seq == 1 {
		e.PrevHash = GenesisPrev
	}
	sum := entryHash(e)
	e.Hash = hex.EncodeToString(sum)
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit: encode: %w", err)
	}
	line = append(line, '\n')
	if _, err := w.f.Write(line); err != nil {
		return fmt.Errorf("audit: append: %w", err)
	}
	w.last = e
	return nil
}

// Close flushes and closes the file. Record after Close returns an
// error through the underlying write failing.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// Verify walks JSONL audit entries from r and reports the first broken
// line. Verification detects edits and injections (chain mismatch),
// deletions (sequence gap), a replaced genesis anchor (bad_genesis),
// and reports a torn crash tail without failing the chain: the writer
// only appends whole newline-terminated lines, so trailing bytes
// without a newline are an interrupted write, never an entry.
func Verify(r io.Reader) Report {
	rep := Report{OK: true}
	raw, err := io.ReadAll(io.LimitReader(r, MaxScanBytes+1))
	if err != nil || len(raw) > MaxScanBytes {
		return failReport(rep, 0, ReasonMalformed)
	}
	prev := Entry{Seq: 0, Hash: GenesisPrev}
	lineNo := 0
	offset := 0
	for offset < len(raw) {
		nl := bytes.IndexByte(raw[offset:], '\n')
		if nl < 0 {
			// Torn tail: trailing bytes without a newline.
			rep.TornTail = true
			break
		}
		line := raw[offset : offset+nl]
		offset += nl + 1
		lineNo++
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			return failReport(rep, lineNo, ReasonMalformed)
		}
		if !verifyEntryShape(e) {
			return failReport(rep, lineNo, ReasonMalformed)
		}
		if lineNo == 1 && e.PrevHash != GenesisPrev {
			return failReport(rep, lineNo, ReasonBadGenesis)
		}
		if e.Seq != prev.Seq+1 {
			return failReport(rep, lineNo, ReasonSeqGap)
		}
		if e.PrevHash != prev.Hash || !bytes.Equal(entryHash(e), mustUnhex(e.Hash)) {
			return failReport(rep, lineNo, ReasonChainMismatch)
		}
		prev = e
		rep.Entries++
	}
	return rep
}

// verifyEntryShape rejects entries whose fields cannot participate in
// the chain (empty hash, unknown surface, non-positive count).
func verifyEntryShape(e Entry) bool {
	if len(e.Hash) != sha256.Size*2 || len(e.PrevHash) != sha256.Size*2 {
		return false
	}
	if e.Surface != SurfaceBody && e.Surface != SurfaceArgs {
		return false
	}
	return e.Count > 0
}

func failReport(rep Report, line int, reason string) Report {
	rep.OK = false
	rep.FirstBad = line
	rep.Reason = reason
	return rep
}

// VerifyFile is Verify over a file path. A read failure (missing or
// unreadable file) reports !OK with FileError set — line semantics do
// not apply, so FirstBad stays 0 and the invariant the fuzz enforces
// holds for Verify (stream input) only.
func VerifyFile(path string) Report {
	f, err := os.Open(path)
	if err != nil {
		return Report{OK: false, Reason: ReasonMalformed, FileError: true}
	}
	defer func() { _ = f.Close() }()
	return Verify(f)
}
