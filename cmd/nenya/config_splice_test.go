package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nenya/config"
)

// assertValidJSONC fails when the document does not parse as JSONC.
func assertValidJSONC(t *testing.T, doc []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(config.StripComments(doc), &m); err != nil {
		t.Fatalf("produced invalid JSON: %v\n---\n%s\n---", err, doc)
	}
	return m
}

// TestSpliceConfigKeyPreservesComments pins the AUD-45 fix (NENYA-224):
// setting a key must leave every comment and the existing key order intact.
func TestSpliceConfigKeyPreservesComments(t *testing.T) {
	doc := []byte(`{
  // top-of-file note about this deployment
  "server": {
    "listen_addr": ":8080" // the port we expose
  },
  /* block comment
     spanning lines */
  "agents": {
    "a1": {"strategy": "fallback"}
  }
}
`)
	out, err := spliceConfigKey(doc, "server.max_body_bytes", float64(1024))
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	m := assertValidJSONC(t, out)

	if got := m["server"].(map[string]any)["max_body_bytes"]; got != float64(1024) {
		t.Fatalf("max_body_bytes = %v, want 1024", got)
	}
	if got := m["server"].(map[string]any)["listen_addr"]; got != ":8080" {
		t.Fatalf("unrelated key lost: listen_addr = %v", got)
	}

	s := string(out)
	for _, want := range []string{
		"// top-of-file note about this deployment",
		"// the port we expose",
		"/* block comment",
		"spanning lines */",
	} {
		if !contains(s, want) {
			t.Fatalf("comment lost: %q\n---\n%s", want, s)
		}
	}

	// Key order: server must still precede agents.
	if idxServer, idxAgents := indexOf(s, `"server"`), indexOf(s, `"agents"`); idxServer < 0 || idxAgents < 0 || idxServer > idxAgents {
		t.Fatalf("key order not preserved (server at %d, agents at %d):\n%s", idxServer, idxAgents, s)
	}
}

// TestSpliceConfigKeyReplacesExistingValue: an existing leaf is replaced in
// place, with its span only.
func TestSpliceConfigKeyReplacesExistingValue(t *testing.T) {
	doc := []byte(`{
  "a": 1, // keep me
  "b": {"c": "old", "d": true} // and me
}
`)
	out, err := spliceConfigKey(doc, "b.c", "new")
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	m := assertValidJSONC(t, out)
	b := m["b"].(map[string]any)
	if b["c"] != "new" || b["d"] != true {
		t.Fatalf("b = %v, want c=new d=true", b)
	}
	if m["a"] != float64(1) {
		t.Fatalf("a = %v, want 1", m["a"])
	}
	if !contains(string(out), "// keep me") || !contains(string(out), "// and me") {
		t.Fatalf("comments lost:\n%s", out)
	}
}

// TestSpliceConfigKeyCreatesNestedObjects: a missing path is created, and the
// document stays valid.
func TestSpliceConfigKeyCreatesNestedObjects(t *testing.T) {
	doc := []byte(`{
  // alpha note
  "alpha": true
}
`)
	out, err := spliceConfigKey(doc, "agents.a1.strategy", "fallback")
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	m := assertValidJSONC(t, out)
	agents, ok := m["agents"].(map[string]any)
	if !ok {
		t.Fatalf("agents not an object: %v", m["agents"])
	}
	a1, ok := agents["a1"].(map[string]any)
	if !ok {
		t.Fatalf("agents.a1 not an object: %v", agents["a1"])
	}
	if a1["strategy"] != "fallback" {
		t.Fatalf("strategy = %v", a1["strategy"])
	}
	if m["alpha"] != true {
		t.Fatalf("alpha lost: %v", m["alpha"])
	}
	if !contains(string(out), "// alpha note") {
		t.Fatalf("comment lost:\n%s", out)
	}
}

// TestSpliceConfigKeyEmptyDocument: an absent document is built from scratch.
func TestSpliceConfigKeyEmptyDocument(t *testing.T) {
	out, err := spliceConfigKey(nil, "server.listen_addr", ":9090")
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	m := assertValidJSONC(t, out)
	if m["server"].(map[string]any)["listen_addr"] != ":9090" {
		t.Fatalf("got %v", m)
	}
}

// TestSpliceConfigKeyStringWithCommasAndBraces: structural characters inside
// strings and comments must not confuse the scanner.
func TestSpliceConfigKeyStringWithCommasAndBraces(t *testing.T) {
	doc := []byte(`{
  "url": "http://x/{a,b}/c", // brace, comma, // in a string
  "note": "say \"hi\", ok"
}
`)
	out, err := spliceConfigKey(doc, "added", float64(2))
	if err != nil {
		t.Fatalf("splice: %v", err)
	}
	m := assertValidJSONC(t, out)
	if m["url"] != "http://x/{a,b}/c" {
		t.Fatalf("url mangled: %v", m["url"])
	}
	if m["note"] != `say "hi", ok` {
		t.Fatalf("note mangled: %v", m["note"])
	}
	if m["added"] != float64(2) {
		t.Fatalf("added = %v", m["added"])
	}
}

func contains(s, sub string) bool { return indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestSetConfigKeyPreservesCommentsOnDisk pins the WRITE PATH end-to-end
// (AUD-45): the config file keeps its comments and key order after a
// `config set`. This is the assertion a map round-trip cannot satisfy, so it
// is the real regression guard for the CLI behavior.
func TestSetConfigKeyPreservesCommentsOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	original := `{
  // production overrides live here
  "server": {
    "listen_addr": ":8080" // keep this port
  },
  /* do not reorder these */
  "governance": {"max_retry_attempts": 3}
}
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := setConfigKey(path, "server.max_body_bytes", float64(2048)); err != nil {
		t.Fatalf("setConfigKey: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	m := assertValidJSONC(t, data)
	if m["server"].(map[string]any)["max_body_bytes"] != float64(2048) {
		t.Fatalf("value not set: %v", m["server"])
	}

	for _, want := range []string{
		"// production overrides live here",
		"// keep this port",
		"/* do not reorder these */",
	} {
		if !contains(got, want) {
			t.Fatalf("comment lost on disk: %q\n---\n%s", want, got)
		}
	}
	if indexOf(got, `"server"`) > indexOf(got, `"governance"`) {
		t.Fatalf("key order changed on disk:\n%s", got)
	}
}

// TestSpliceConfigKeySharedStarGrammar pins the NENYA-339 invariant that
// the editor's comment scanner and the loader's StripComments agree
// byte-for-byte: a doc whose comment span only parses correctly under the
// STANDARD grammar (the shared star of an opening /* must not close it)
// splices cleanly, the trailing comment survives the splice verbatim, and
// the spliced output strips to a document carrying both the replaced value
// and the preserved comment's neighbors. If the two hand-synced scanners
// ever drift apart again, offsets diverge and this fails deterministically.
func TestSpliceConfigKeySharedStarGrammar(t *testing.T) {
	doc := []byte(`{"a": 1 /*/ note */}`)

	t.Run("insert after a shared-star comment value", func(t *testing.T) {
		out, err := spliceConfigKey(append([]byte(nil), doc...), "b", float64(42))
		if err != nil {
			t.Fatalf("splice: %v", err)
		}
		stripped := config.StripComments(out)
		var m map[string]any
		if err := json.Unmarshal(stripped, &m); err != nil {
			t.Fatalf("spliced doc does not strip to valid JSON: %v\n%s", err, out)
		}
		if m["a"] != float64(1) || m["b"] != float64(42) {
			t.Fatalf("a=%v b=%v, want 1 and 42\n%s", m["a"], m["b"], out)
		}
		if !contains(string(out), "/*/ note */") {
			t.Fatalf("the trailing comment must survive the splice verbatim:\n%s", out)
		}
	})

	t.Run("replace a value before a shared-star comment", func(t *testing.T) {
		out, err := spliceConfigKey(append([]byte(nil), doc...), "a", float64(7))
		if err != nil {
			t.Fatalf("splice: %v", err)
		}
		stripped := config.StripComments(out)
		var m map[string]any
		if err := json.Unmarshal(stripped, &m); err != nil {
			t.Fatalf("spliced doc does not strip to valid JSON: %v\n%s", err, out)
		}
		if m["a"] != float64(7) {
			t.Fatalf("a=%v, want 7 (the shared-star comment must not eat the replacement)\n%s", m["a"], out)
		}
		if _, ok := m["b"]; ok {
			t.Fatalf("a replace must not create b:\n%s", out)
		}
		if string(out) != `{"a": 7 /*/ note */}` {
			t.Fatalf("byte-exact output mismatch (the comment must survive the replacement verbatim):\n%s", out)
		}
	})
}

// TestSkipCommentMatchesLoaderGrammar is the direct drift detector: for a
// doc that is exactly one block comment, the editor scanner must consume
// precisely the bytes the loader's StripComments removes. Under the
// NENYA-339 quirk (comment closed at the shared star) the editor consumed
// 3 bytes where the loader now consumes 6.
func TestSkipCommentMatchesLoaderGrammar(t *testing.T) {
	for _, doc := range []string{
		"/*x*/",
		"/*/x*/", // shared star: NOT a close
		"/* multi\nline */",
		// "/*/" — boundary case: both grammars consume 3 (EOF coverage,
		// not a grammar discriminator).
		"/*/",
	} {
		// Bytes removed by the loader == bytes the editor must consume.
		want := len(doc) - len(stripCommentsForTest(doc))
		s := &jsonScanner{data: []byte(doc)}
		s.skipIgnorable()
		if s.i != want {
			t.Errorf("editor consumed %d bytes of %q, loader grammar removes %d — comment grammars diverged", s.i, doc, want)
		}
	}
}

// stripCommentsForTest exposes the loader grammar to the drift detector
// without importing the loader's internals beyond the public strip.
func stripCommentsForTest(doc string) string {
	return string(config.StripComments([]byte(doc)))
}
