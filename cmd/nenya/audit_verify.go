package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/nenya/internal/audit"
)

// auditVerifyUsage is the usage text for the audit-verify subcommand.
const auditVerifyUsage = `usage: nenya audit-verify [--json] <audit-file>

Walks a redaction audit log (governance.redaction_audit) and verifies
its tamper-evidence hash chain. Exits 0 when the chain is intact,
1 when verification fails (with the first broken line), 2 on usage
errors. A trailing partial line is reported as a torn tail — a crash
remnant, not tampering.`

// handleAuditVerify implements the audit-verify subcommand (NENYA-362):
// the operator-facing half of the tamper-evident redaction audit log.
// It reports whether the chain is intact and names the first broken
// line when it is not.
func handleAuditVerify(w io.Writer, errW io.Writer, args []string) (bool, error) {
	if len(args) == 0 || args[0] != "audit-verify" {
		return false, nil
	}
	if len(args) >= 2 && isHelpArg(args[1]) {
		_, _ = fmt.Fprintln(errW, auditVerifyUsage)
		return true, flag.ErrHelp
	}
	fs := flag.NewFlagSet("audit-verify", flag.ContinueOnError)
	fs.SetOutput(errW)
	jsonOut := fs.Bool("json", false, "emit the verification report as JSON")
	if err := parseCommandFlags(fs, args[1:]); err != nil {
		return true, err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		_, _ = fmt.Fprintln(errW, auditVerifyUsage)
		return true, errUsage
	}

	rep := audit.VerifyFile(rest[0])
	if *jsonOut {
		if err := writeAuditReportJSON(w, rep); err != nil {
			return true, err
		}
	} else {
		if err := writeAuditReportText(w, rep); err != nil {
			return true, err
		}
	}
	if !rep.OK {
		if rep.FileError {
			return true, fmt.Errorf("cannot read audit file %s", rest[0])
		}
		return true, fmt.Errorf("audit chain broken at line %d: %s", rep.FirstBad, rep.Reason)
	}
	return true, nil
}

// writeAuditReportJSON emits the machine-readable verification report.
func writeAuditReportJSON(w io.Writer, rep audit.Report) error {
	encoded, err := json.Marshal(struct {
		OK        bool   `json:"ok"`
		Entries   int    `json:"entries"`
		FirstBad  int    `json:"first_bad,omitempty"`
		Reason    string `json:"reason,omitempty"`
		TornTail  bool   `json:"torn_tail,omitempty"`
		FileError bool   `json:"file_error,omitempty"`
	}{rep.OK, rep.Entries, rep.FirstBad, rep.Reason, rep.TornTail, rep.FileError})
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	_, err = fmt.Fprintln(w, string(encoded))
	return err
}

// writeAuditReportText emits the human-readable verification report.
func writeAuditReportText(w io.Writer, rep audit.Report) error {
	if rep.FileError {
		_, err := fmt.Fprintln(w, "cannot read audit file")
		return err
	}
	if _, err := fmt.Fprintf(w, "entries verified: %d\n", rep.Entries); err != nil {
		return err
	}
	if rep.TornTail {
		if _, err := fmt.Fprintln(w, "torn tail: trailing partial line (crash remnant, not tampering)"); err != nil {
			return err
		}
	}
	if rep.OK {
		_, err := fmt.Fprintln(w, "chain intact")
		return err
	}
	_, err := fmt.Fprintf(w, "chain BROKEN at line %d: %s\n", rep.FirstBad, rep.Reason)
	return err
}
