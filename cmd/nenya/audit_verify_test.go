package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenya/internal/audit"
)

// NENYA-362 pins: the audit-verify subcommand reports chain status and
// maps a broken chain to an error (exit 1 through inspectionExitCode).

func growChain(t *testing.T, path string, n int) {
	t.Helper()
	w, err := audit.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := w.Record("agent-a", audit.SurfaceBody, 1); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestAuditVerifyIntactChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	growChain(t, path, 3)
	var out, errW bytes.Buffer
	handled, err := handleAuditVerify(&out, &errW, []string{"audit-verify", path})
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out.String(), "entries verified: 3") || !strings.Contains(out.String(), "chain intact") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestAuditVerifyBrokenChainErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	growChain(t, path, 3)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Tamper with the second entry's count (line 2, not the first
	// occurrence of the fragment).
	lines := bytes.Split(raw, []byte("\n"))
	lines[1] = bytes.Replace(lines[1], []byte(`"count":1`), []byte(`"count":7`), 1)
	if writeErr := os.WriteFile(path, bytes.Join(lines, []byte("\n")), 0o600); writeErr != nil {
		t.Fatalf("write: %v", writeErr)
	}
	var out, errW bytes.Buffer
	handled, err := handleAuditVerify(&out, &errW, []string{"audit-verify", path})
	if !handled {
		t.Fatal("not handled")
	}
	if err == nil {
		t.Fatal("broken chain returned nil error (would exit 0)")
	}
	if !strings.Contains(out.String(), "chain BROKEN at line 2") {
		t.Fatalf("output: %q", out.String())
	}
}

func TestAuditVerifyJSONAndTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.jsonl")
	growChain(t, path, 2)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	_, _ = f.WriteString(`{"seq":3,`)
	_ = f.Close()

	var out, errW bytes.Buffer
	handled, err := handleAuditVerify(&out, &errW, []string{"audit-verify", "--json", path})
	if !handled || err != nil {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !strings.Contains(out.String(), `"ok":true`) || !strings.Contains(out.String(), `"entries":2`) || !strings.Contains(out.String(), `"torn_tail":true`) {
		t.Fatalf("json output: %q", out.String())
	}
}

func TestAuditVerifyUsageAndDispatch(t *testing.T) {
	var out, errW bytes.Buffer
	handled, err := handleAuditVerify(&out, &errW, []string{"audit-verify"})
	if !handled || err != errUsage {
		t.Fatalf("missing file: handled=%v err=%v, want usage error", handled, err)
	}
	handled, err = handleAuditVerify(&out, &errW, []string{"not-audit-verify"})
	if handled {
		t.Fatal("unrelated command claimed")
	}
	if err != nil {
		t.Fatalf("unrelated command returned an error: %v", err)
	}
	// Dispatch wiring: the subcommand must be reachable from runSubcommands.
	code, handled := runSubcommands([]string{"audit-verify", "--help"})
	if !handled || code != 0 {
		t.Fatalf("dispatch: handled=%v code=%d", handled, code)
	}
}
