package main

// FuzzSpliceConfigKey pins NENYA-332 (audit leftover): config_splice.go is a
// hand-rolled JSON/JSONC scanner splicing values into the LIVE config file —
// the only security-critical parser without a Fuzz* target (AGENTS §12). A
// scanner bug here corrupts operator configs: offsets computed on the
// original bytes must yield, on success, a document that (a) parses as JSON
// after comment stripping (the production gate is json.Valid — numbers that
// overflow float64 stay valid, so the oracle decodes with UseNumber), (b)
// still carries any sentinel sibling, and (c) resolves dottedKey to exactly
// the spliced value. On failure the editor must produce nothing (nil, err) —
// never a partial document.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/nenya/config"
)

const fuzzSeedDoc = `{
  "server": {
    // the port we expose
    "listen_addr": ":8080",
    "read_timeout": 30
  },
  "__sentinel__": "keep-me",
  "agents": { /* inline */ "a1": {"strategy": "fallback"} }
}
`

func decodeUseNumber(t *testing.T, data []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("document does not decode: %v\n---\n%s\n---", err, data)
	}
	return m
}

func spliceDocIsSane(t *testing.T, doc, out []byte, dottedKey string) {
	t.Helper()
	stripped := config.StripComments(out)
	if !json.Valid(stripped) {
		t.Fatalf("spliced document is not valid JSON (the production gate)\n---\n%s\n---", out)
	}
	m := decodeUseNumber(t, stripped)

	// The sentinel assert applies only when the PARSED INPUT carried the
	// sentinel AND the splice did not target it exactly (replacing the
	// sentinel is legal). Presence is the property — the fuzzer may have
	// mutated its value.
	var in map[string]any
	if json.Valid(config.StripComments(doc)) {
		ind := json.NewDecoder(bytes.NewReader(config.StripComments(doc)))
		ind.UseNumber()
		_ = ind.Decode(&in)
	}
	if _, had := in["__sentinel__"]; had && dottedKey != "__sentinel__" {
		if _, ok := m["__sentinel__"]; !ok {
			t.Fatalf("sentinel sibling lost by the splice (doc: %s)", out)
		}
	}

	// The targeted key must resolve to exactly the spliced value. Parts are
	// normalized through json.Marshal round-trip FIRST — the editor writes
	// keys via json.Marshal, which normalizes invalid UTF-8 to U+FFFD, so
	// the oracle must look up the same normalized key (raw "\xe2" never
	// round-trips).
	cur := any(m)
	for _, part := range strings.Split(dottedKey, ".") {
		keyJSON, err := json.Marshal(part)
		if err != nil {
			t.Fatalf("key part does not marshal: %v", err)
		}
		var normalized string
		if err := json.Unmarshal(keyJSON, &normalized); err != nil {
			t.Fatalf("marshaled key does not decode: %v", err)
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("spliced path %q breaks the object chain at %q (doc: %s)", dottedKey, part, out)
		}
		cur, ok = obj[normalized]
		if !ok {
			t.Fatalf("spliced key %q missing from output (doc: %s)", dottedKey, out)
		}
	}
	n, ok := cur.(json.Number)
	if !ok || n.String() != "42" {
		t.Fatalf("spliced value = %v (%T), want 42", cur, cur)
	}
}

func FuzzSpliceConfigKey(f *testing.F) {
	f.Add([]byte(fuzzSeedDoc), "server.read_timeout")
	f.Add([]byte(fuzzSeedDoc), "server.read_timeout.extra.nested")
	f.Add([]byte(fuzzSeedDoc), "__sentinel__")
	f.Add([]byte(fuzzSeedDoc), "agents.a1.strategy")
	f.Add([]byte(fuzzSeedDoc), "brand.new.key")
	f.Add([]byte("{}"), "a")
	f.Add([]byte(""), "a.b")
	f.Add([]byte("// only a comment\n"), "a")
	f.Add([]byte(`{"k": "val with } brace and \" quote"}`), "k")
	f.Add([]byte(`{"k": /* comment between */ 1}`), "m")
	f.Add([]byte(`{"k": [1, 2, {"deep": true}]}`), "k")
	f.Add([]byte(`{"k": {"a": "\\"}, "j": "// not a comment"}`), "j")
	f.Add([]byte(`{"k": "unterminated`), "k")

	f.Fuzz(func(t *testing.T, doc []byte, dottedKey string) {
		out, err := spliceConfigKey(doc, dottedKey, float64(42))
		if err != nil {
			if out != nil {
				t.Fatalf("failure path must return a nil document, got %d bytes (err: %v)", len(out), err)
			}
			return
		}
		spliceDocIsSane(t, doc, out, dottedKey)
	})
}
