package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// unknownFieldsFromLoad runs LoadFromDirWithDiagnostics over a directory with
// a single config.json and returns the unknown_field diagnostic messages.
func unknownFieldsFromLoad(t *testing.T, content string) []string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, diags, err := LoadFromDirWithDiagnostics(dir)
	if err != nil {
		t.Fatalf("unknown fields must never fail load: %v", err)
	}
	var msgs []string
	for _, d := range diags {
		if d.Code == "unknown_field" {
			msgs = append(msgs, d.Message)
		}
	}
	return msgs
}

// containsUnknownField reports whether one diagnostic message names name —
// messages carry the quoted dotted path, so the leaf matches as `.name"`
// inside the message (or the whole quoted path for top-level keys).
func containsUnknownField(msgs []string, name string) bool {
	for _, m := range msgs {
		if strings.Contains(m, `"`+name+`"`) || strings.Contains(m, `.`+name+`"`) {
			return true
		}
	}
	return false
}

// TestLoad_UnknownFieldsAllReported pins AUD-75 (NENYA-303): EVERY unknown
// field in a config file surfaces a diagnostic — the strict secondary decode
// used to stop at the first hit, so a file with several typos named only one.
func TestLoad_UnknownFieldsAllReported(t *testing.T) {
	msgs := unknownFieldsFromLoad(t, `{
		"server": {"listen_addr": ":9100", "liseten_addr": ":typo"},
		"governance": {"ratelimit_max_rpm": 30, "future_field": true},
		"providers": {"zai": {"url": "https://api.z.ai/v1", "vendor_tier": "gold"}},
		"completely_new_section": {"x": 1}
	}`)
	for _, name := range []string{"liseten_addr", "future_field", "vendor_tier", "completely_new_section"} {
		if !containsUnknownField(msgs, name) {
			t.Errorf("unknown field %q must be reported, got %v", name, msgs)
		}
	}
}

// TestLoad_UnknownFieldsAlignment pins the walker's alignment with the strict
// decoder's acceptance rules so the plural report cannot invent false
// positives: a case-insensitive known spelling is accepted by encoding/json
// (never reported), and an unknown key inside a type with a custom
// UnmarshalJSON (AgentConfig) is accepted by the loader (never reported).
func TestLoad_UnknownFieldsAlignment(t *testing.T) {
	msgs := unknownFieldsFromLoad(t, `{
		"server": {"LISTEN_ADDR": ":9100"},
		"agents": {"helper": {"models": ["gpt-4o"], "not_a_real_agent_field": true}}
	}`)
	if len(msgs) != 0 {
		t.Fatalf("accepted spellings must not be reported, got %v", msgs)
	}
}
