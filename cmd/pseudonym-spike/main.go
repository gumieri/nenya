// Command pseudonym-spike is the C010 benchmark harness (NENYA-356,
// Module A/C go-no-go gate for reversible pseudonymization). It measures
// the mechanical fidelity of declared-term patch/restore over a corpus:
//
//	apply rate      — patched occurrences / detected occurrences (gate: >= 99.5%)
//	unresolved rate — inflected/distinct-word occurrences left verbatim (gate: <= 0.5%)
//	round trip      — restore(patch(x)) == x per corpus document (must hold on
//	                  exact-case documents; case variants are a recorded loss)
//	throughput      — patch/restore ns/op via the package benchmarks
//
// and emits the operator runbook inputs for the LIVE half of the
// measurement (prompt-cache interaction on >= 2 real models), which needs
// provider credentials and is therefore operator-run: --emit-requests
// writes ready-to-dispatch request bodies; docs/SPIKE_REVERSIBLE_PSEUDONYM.md
// carries the runbook and the decision template.
//
// Usage:
//
//	pseudonym-spike --terms terms.json --corpus DIR [--salt S] [--out stats.json] [--emit-requests DIR]
//
// The terms file is [{"value":"Acme Corp","kind":"company"}, …]; the
// corpus is a directory of .txt/.md documents. Exit 0 always (the gate
// DECISION is human, not the harness's).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nenya/internal/pseudonym"
)

// termsFile is the on-disk declared-term list.
type termsFile struct {
	Terms []pseudonym.Term `json:"terms"`
}

// docStat is one corpus document's measured outcome.
type docStat struct {
	File           string  `json:"file"`
	Bytes          int     `json:"bytes"`
	Occurrences    int     `json:"occurrences"`
	Patched        int     `json:"patched"`
	Exact          int     `json:"exact"`
	CaseVariants   int     `json:"case_variants"`
	Unresolved     int     `json:"unresolved"`
	ApplyRate      float64 `json:"apply_rate"`
	UnresolvedRate float64 `json:"unresolved_rate"`
	RoundTrip      string  `json:"round_trip"` // "byte_exact" | "case_loss" | "MISMATCH"
	Restored       int     `json:"restored"`
	// MappingEntries is the per-doc distinct pseudonym count (a term
	// recurring inside one document counts once).
	MappingEntries int `json:"mapping_entries"`
}

// report is the aggregated harness output (the gate's evidence artifact).
// The Salt field is KEY MATERIAL (salt + guessed term -> confirmed via the
// deterministic pseudonym): treat the report like the mapping.
type report struct {
	Salt           string  `json:"salt"`
	Docs           int     `json:"docs"`
	Terms          int     `json:"terms"`
	Occurrences    int     `json:"occurrences"`
	Patched        int     `json:"patched"`
	Exact          int     `json:"exact"`
	CaseVariants   int     `json:"case_variants"`
	Unresolved     int     `json:"unresolved"`
	ApplyRate      float64 `json:"apply_rate"`
	UnresolvedRate float64 `json:"unresolved_rate"`
	ByteExactDocs  int     `json:"byte_exact_docs"`
	CaseLossDocs   int     `json:"case_loss_docs"`
	MismatchDocs   int     `json:"mismatch_docs"`
	// MappingEntries sums the per-doc distinct counts: a term recurring
	// across documents counts once per document.
	MappingEntries int       `json:"mapping_entries"`
	PerDoc         []docStat `json:"per_doc"`
}

func main() {
	termsPath := flag.String("terms", "", "declared-terms JSON file (required)")
	corpusDir := flag.String("corpus", "", "corpus directory of .txt/.md documents (required)")
	saltFlag := flag.String("salt", "", "salt epoch (default: generated at run, printed in the report)")
	outPath := flag.String("out", "", "write the JSON report here (default: stdout)")
	emitDir := flag.String("emit-requests", "", "emit operator live-run request bodies into this directory")
	flag.Parse()

	if *termsPath == "" || *corpusDir == "" {
		fmt.Fprintln(os.Stderr, "usage: pseudonym-spike --terms FILE --corpus DIR [--salt S] [--out FILE] [--emit-requests DIR]")
		os.Exit(2)
	}
	if err := run(*termsPath, *corpusDir, *saltFlag, *outPath, *emitDir); err != nil {
		fmt.Fprintln(os.Stderr, "pseudonym-spike:", err)
		os.Exit(1)
	}
}

func run(termsPath, corpusDir, saltFlag, outPath, emitDir string) error {
	table, salt, err := loadTerms(termsPath, saltFlag)
	if err != nil {
		return err
	}
	files, err := corpusFiles(corpusDir)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no .txt/.md documents under %s", corpusDir)
	}

	rep := report{Salt: string(salt), Terms: len(table.Terms())}
	for _, f := range files {
		st, err := measureDoc(table, salt, f)
		if err != nil {
			return err
		}
		rep.Docs++
		rep.Occurrences += st.Occurrences
		rep.Patched += st.Patched
		rep.Exact += st.Exact
		rep.CaseVariants += st.CaseVariants
		rep.Unresolved += st.Unresolved
		rep.MappingEntries += st.MappingEntries
		switch st.RoundTrip {
		case "byte_exact":
			rep.ByteExactDocs++
		case "case_loss":
			rep.CaseLossDocs++
		default:
			rep.MismatchDocs++
		}
		rep.PerDoc = append(rep.PerDoc, st)
	}
	rep.ApplyRate = rate(rep.Patched, rep.Occurrences)
	rep.UnresolvedRate = rate(rep.Unresolved, rep.Occurrences)

	if emitDir != "" {
		if err := emitRequests(emitDir, table, salt); err != nil {
			return err
		}
	}
	return writeReport(rep, outPath)
}

// loadTerms reads the declared-terms file and derives the salt (operator
// supplied, else a random default printed in the report so the run is
// reproducible).
func loadTerms(termsPath, saltFlag string) (*pseudonym.Table, []byte, error) {
	raw, err := os.ReadFile(termsPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read terms: %w", err)
	}
	var tf termsFile
	if err := json.Unmarshal(raw, &tf); err != nil {
		return nil, nil, fmt.Errorf("parse terms (want {\"terms\":[{\"value\":…,\"kind\":…}]}): %w", err)
	}
	if len(tf.Terms) == 0 {
		return nil, nil, fmt.Errorf("terms file declares no terms")
	}
	salt := []byte(saltFlag)
	if saltFlag == "" {
		salt = randomSalt()
	}
	return pseudonym.NewTable(tf.Terms), salt, nil
}

// corpusFiles lists the corpus documents deterministically.
func corpusFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read corpus: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".txt") || strings.HasSuffix(name, ".md") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	sort.Strings(files)
	return files, nil
}

// measureDoc runs one document through patch→restore and verifies the
// round trip: byte_exact when restore(patch(x)) == x, case_loss when the
// restored text equals the patcher's own canonical rendering of the
// accepted spans (case-variant patches restore to declared casing — the
// recorded loss), MISMATCH otherwise (a real fidelity bug — the gate
// fails closed on any of these).
func measureDoc(table *pseudonym.Table, salt []byte, path string) (docStat, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return docStat{}, fmt.Errorf("read %s: %w", path, err)
	}
	text := string(raw)
	p := pseudonym.NewPatcher(table, salt)
	patched := p.Patch(text)
	r := pseudonym.NewRestorer(p.Mapping())
	restored := r.Restore(patched.Text)

	st := docStat{
		File:           filepath.Base(path),
		Bytes:          len(raw),
		Occurrences:    patched.Stats.Occurrences,
		Patched:        patched.Stats.Patched,
		Exact:          patched.Stats.Exact,
		CaseVariants:   patched.Stats.CaseVariants,
		Unresolved:     patched.Stats.Unresolved,
		ApplyRate:      patched.Stats.ApplyRate(),
		UnresolvedRate: patched.Stats.UnresolvedRate(),
		Restored:       restored.Restored,
	}
	switch {
	case restored.Text == text:
		st.RoundTrip = "byte_exact"
	case restored.Text == p.CanonicalText(text):
		// The expected post-restore rendering comes from the patcher's
		// own accepted-span stream: honestly-handled documents (swallowed
		// interior aliases, unresolved inflections) classify as a recorded
		// case loss, never as a false MISMATCH.
		st.RoundTrip = "case_loss"
	default:
		st.RoundTrip = "MISMATCH"
	}
	st.MappingEntries = len(p.Mapping().Entries())
	return st, nil
}

// writeReport prints the human summary and writes the JSON artifact. The
// report carries the salt epoch — key material (salt + guessed term ->
// confirmed via the deterministic pseudonym), stated on the output so the
// artifact is handled like the mapping.
func writeReport(rep report, outPath string) error {
	fmt.Printf("pseudonym-spike: docs=%d terms=%d occurrences=%d patched=%d (%.2f%%) unresolved=%d (%.2f%%) round-trip byte_exact=%d case_loss=%d MISMATCH=%d mapping_entries=%d salt=%q\n",
		rep.Docs, rep.Terms, rep.Occurrences, rep.Patched, rep.ApplyRate*100,
		rep.Unresolved, rep.UnresolvedRate*100, rep.ByteExactDocs, rep.CaseLossDocs, rep.MismatchDocs, rep.MappingEntries, rep.Salt)
	fmt.Println("pseudonym-spike: NOTE the report carries the salt epoch — sensitive key material, handle like the mapping")
	encoded, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return err
	}
	if outPath == "" {
		fmt.Println(string(encoded))
		return nil
	}
	return os.WriteFile(outPath, append(encoded, '\n'), 0o600)
}
