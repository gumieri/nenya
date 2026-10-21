package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// runConfigSet runs `config set` and returns the path it printed.
func runConfigSet(t *testing.T, args ...string) string {
	t.Helper()
	var buf, errBuf bytes.Buffer
	handled, err := handleConfig(&buf, &errBuf, append([]string{"config", "set"}, args...))
	if err != nil {
		t.Fatalf("handleConfig error: %v (stderr: %s)", err, errBuf.String())
	}
	if !handled {
		t.Fatal("expected config set to be handled")
	}
	return strings.TrimSpace(buf.String())
}

// readJSONFile decodes a JSON object from path.
func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return doc
}

// nested returns the value at a dotted path in a decoded JSON object.
func nested(t *testing.T, doc map[string]any, dotted string) any {
	t.Helper()
	parts := strings.Split(dotted, ".")
	var cur any = doc
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("key %q is not an object while walking %q", p, dotted)
		}
		cur = m[p]
	}
	return cur
}

func TestConfigSet_DirectoryModeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	path := runConfigSet(t, "server.listen_addr", `":9090"`)

	want := filepath.Join(dir, "config.d", managedConfigDropIn)
	if path != want {
		t.Fatalf("printed path = %q, want %q", path, want)
	}
	if got := nested(t, readJSONFile(t, path), "server.listen_addr"); got != ":9090" {
		t.Errorf("written listen_addr = %v, want :9090", got)
	}

	cfg, err := config.LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("loaded ListenAddr = %q, want :9090", cfg.Server.ListenAddr)
	}

	// NENYA-183: the rewrite preserves the file's existing permissions (the
	// shipped systemd unit runs a dynamic user, so forcing 0600 would make a
	// root-owned drop-in unreadable to the service).
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Errorf("config file mode = %o, want the seeded 644 preserved", fi.Mode().Perm())
	}
}

// TestConfigSet_PreservesPrivateMode pins the other half: an operator who
// makes a credential-bearing config file private keeps that mode across
// rewrites.
func TestConfigSet_PreservesPrivateMode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "private.json")
	if err := os.WriteFile(file, []byte(`{"server":{"listen_addr":":8080"}}`), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("NENYA_CONFIG_FILE", file)
	t.Setenv("NENYA_CONFIG_DIR", "")

	runConfigSet(t, "server.listen_addr", `":9090"`)

	fi, err := os.Stat(file)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %o, want the seeded 600 preserved", fi.Mode().Perm())
	}
}

func TestConfigSet_ValueParsing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	runConfigSet(t, "governance.auto_retry_on_context_limit", "true")
	path := runConfigSet(t, "server.log_level", "debug")

	doc := readJSONFile(t, path)
	if got := nested(t, doc, "governance.auto_retry_on_context_limit"); got != true {
		t.Errorf("auto_retry_on_context_limit = %#v, want bool true", got)
	}
	if got := nested(t, doc, "server.log_level"); got != "debug" {
		t.Errorf("log_level = %#v, want string \"debug\"", got)
	}

	cfg, err := config.LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if cfg.Governance.AutoRetryOnContextLimit == nil || !*cfg.Governance.AutoRetryOnContextLimit {
		t.Errorf("loaded auto_retry_on_context_limit not true")
	}
	if cfg.Server.LogLevel != "debug" {
		t.Errorf("loaded log_level = %q, want debug", cfg.Server.LogLevel)
	}
}

func TestConfigSet_FileMode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "custom.json")
	if err := os.WriteFile(file, []byte(`{"server":{"listen_addr":":8080"}}`), 0o644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("NENYA_CONFIG_FILE", file)
	t.Setenv("NENYA_CONFIG_DIR", "")

	path := runConfigSet(t, "server.listen_addr", `":9090"`)

	if path != file {
		t.Fatalf("printed path = %q, want %q", path, file)
	}
	cfg, err := config.Load(file)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("loaded ListenAddr = %q, want :9090", cfg.Server.ListenAddr)
	}
}

func TestConfigSet_CreatesNestedObjectsAndPreservesKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	runConfigSet(t, "alpha.beta.gamma", "7")
	path := runConfigSet(t, "alpha.beta.delta", "kept")

	doc := readJSONFile(t, path)
	if got := nested(t, doc, "alpha.beta.gamma"); got != float64(7) {
		t.Errorf("gamma = %#v, want 7", got)
	}
	if got := nested(t, doc, "alpha.beta.delta"); got != "kept" {
		t.Errorf("delta = %#v, want kept", got)
	}
}

func TestConfigSet_RejectsNonObjectIntermediate(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	runConfigSet(t, "server.listen_addr", `":9090"`)

	// server is an object, but server.listen_addr is a string: cannot descend.
	_, err := handleConfig(io.Discard, io.Discard, []string{"config", "set", "server.listen_addr.x", "1"})
	if err == nil {
		t.Fatal("expected an error setting a value under a scalar")
	}
}

func TestConfigSet_UsageErrors(t *testing.T) {
	cases := map[string][]string{
		"missing_set":   {"config"},
		"missing_args":  {"config", "set"},
		"too_many_args": {"config", "set", "a.b", "1", "2"},
		"unknown_flag":  {"config", "set", "--nope", "a.b", "1"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			handled, err := handleConfig(io.Discard, io.Discard, args)
			if !handled || !errors.Is(err, errUsage) {
				t.Fatalf("expected errUsage, got handled=%v err=%v", handled, err)
			}
		})
	}
}

func TestConfigSet_Help(t *testing.T) {
	handled, err := handleConfig(io.Discard, io.Discard, []string{"config", "set", "-h"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got handled=%v err=%v", handled, err)
	}
}

func TestConfigSet_Passthrough(t *testing.T) {
	handled, err := handleConfig(io.Discard, io.Discard, []string{"serve"})
	if handled || err != nil {
		t.Errorf("expected passthrough, got handled=%v err=%v", handled, err)
	}
}

func TestConfigSet_CredentialKeyStrengthGuard(t *testing.T) {
	// NENYA-192: credential-typed config keys are strength-checked at write
	// time so `config set` cannot produce a boot-refused document.
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NENYA_CONFIG_DIR", configDir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	var out, errBuf bytes.Buffer
	// Placeholder under providers.*.api_key is rejected with exit 2.
	handled, err := handleConfig(&out, &errBuf, []string{"config", "set", "providers.openai.api_key", "your-api-key-here"})
	if !handled || !errors.Is(err, errUsage) {
		t.Fatalf("placeholder api_key: handled=%v err=%v, want errUsage", handled, err)
	}
	if !strings.Contains(errBuf.String(), "rejected") {
		t.Errorf("stderr = %q, want a rejection message", errBuf.String())
	}

	// Case-insensitive key match is also guarded.
	errBuf.Reset()
	handled, err = handleConfig(&out, &errBuf, []string{"config", "set", "providers.openai.API_KEY", "changeme-0000000000"})
	if !handled || !errors.Is(err, errUsage) {
		t.Fatalf("uppercase API_KEY: handled=%v err=%v, want errUsage", handled, err)
	}

	// A strong value writes fine.
	if _, err := handleConfig(&out, &errBuf, []string{"config", "set", "providers.openai.api_key", "sk-real-key-0123456789abcdef"}); err != nil {
		t.Fatalf("strong api_key rejected: %v", err)
	}

	// Non-credential leaves are untouched.
	if _, err := handleConfig(&out, &errBuf, []string{"config", "set", "mcp_servers.local.headers.api_key", "changeme-0000000000"}); err != nil {
		t.Fatalf("mcp header value must not be strength-checked: %v", err)
	}
}

// NENYA-175: `config set` warns (never blocks) when the target key does not
// exist in the config schema — typo protection mirroring the loader's
// NENYA-19 unknown-field warning.
func TestConfigSet_WarnsOnUnknownKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	cases := []struct {
		name     string
		key      string
		value    string
		wantWarn string // substring of the warning; "" asserts no warning
	}{
		{"typo in a section name", "goverance.max_tokens", `"x"`, `"goverance" is not a known config key`}, //nolint:misspell // the typo is the test input
		{"typo in a leaf name", "server.listen_addr_typo", `"x"`, `"listen_addr_typo" is not a known config key`},
		{"known key stays silent", "server.listen_addr", `":8080"`, ""},
		// Provider/agent/MCP-server NAMES are map keys: any spelling is valid.
		// (The api_key leaf needs a strong value: the credential strength gate
		// runs before the write and rejects weak values outright, NENYA-192.)
		{"provider name is a map key", "providers.myprov.api_key", `"nk-provider-test-0123456789abcdef"`, ""},
		{"agent name is a map key", "agents.myagent.strategy", `"sticky"`, ""},
		{"mcp header is a map value", "mcp_servers.srv.headers.X-Key", `"x"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			handled, err := handleConfig(&out, &errOut, []string{"config", "set", tc.key, tc.value})
			if !handled || err != nil {
				t.Fatalf("handleConfig: handled=%v err=%v stderr=%s", handled, err, errOut.String())
			}
			if tc.wantWarn == "" {
				if strings.Contains(errOut.String(), "not a known config key") {
					t.Fatalf("a valid key must not warn: %s", errOut.String())
				}
			} else if !strings.Contains(errOut.String(), tc.wantWarn) {
				t.Fatalf("expected a typo warning naming the field:\n%s", errOut.String())
			}
			// The value lands either way (the warning never blocks; a silent
			// key must have been written too).
			path := filepath.Join(dir, "config.d", managedConfigDropIn)
			doc := readJSONFile(t, path)
			var written any
			if err := json.Unmarshal([]byte(tc.value), &written); err != nil {
				t.Fatal(err)
			}
			if got := nested(t, doc, tc.key); got != written {
				t.Fatalf("the value must be written: %v, want %v", got, written)
			}
		})
	}
}

// NENYA-175: the write-preservation round trip — foreign top-level keys and
// foreign keys inside known sections survive BOTH write directions (a nested
// write and a top-level write).
func TestConfigSet_ForeignKeysSurviveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	original := `{
	  "top_level_foreign": {"nested": [1, 2, {"deep": true}]},
	  "another_foreign": "keep-me",
	  "server": {"listen_addr": ":9000", "foreign_in_known_section": {"a": 1}},
	  "providers": {"existing": {"url": "http://x"}}
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// A nested-path write must not disturb the foreign keys.
	runConfigSet(t, "providers.new.api_key", "nk-roundtrip-0123456789")
	// The drop-in only carries the written key; foreign keys live in the base.
	base := readJSONFile(t, filepath.Join(dir, "config.json"))
	for _, key := range []string{"top_level_foreign", "another_foreign"} {
		if _, ok := base[key]; !ok {
			t.Fatalf("the base file must still carry %q after a nested write", key)
		}
	}

	// And the merged load sees foreign + written keys together.
	cfg, err := config.LoadFromDir(dir)
	if err != nil {
		t.Fatalf("LoadFromDir: %v", err)
	}
	if cfg.Server.ListenAddr != ":9000" {
		t.Fatalf("the base listen_addr must survive: %q", cfg.Server.ListenAddr)
	}

	// A top-level foreign-key write must not disturb nested content: write
	// into the BASE file directly (file mode) and re-check everything.
	baseFile := filepath.Join(dir, "config.json")
	runConfigSet(t, "--config", baseFile, "brand_new_top_level", `"v"`)
	after := readJSONFile(t, baseFile)
	if got := nested(t, after, "brand_new_top_level"); got != "v" {
		t.Fatalf("the top-level write must land: %v", got)
	}
	if _, ok := after["top_level_foreign"]; !ok {
		t.Fatal("the foreign top-level object must survive the top-level write")
	}
	// The foreign object's nested array content survives intact (the helper
	// walks maps only, so the array is navigated by hand).
	foreign, ok := after["top_level_foreign"].(map[string]any)
	if !ok {
		t.Fatalf("the foreign object must keep its shape: %v", after["top_level_foreign"])
	}
	arr, ok := foreign["nested"].([]any)
	if !ok || len(arr) != 3 {
		t.Fatalf("the foreign array must survive intact: %v", foreign["nested"])
	}
	deep, ok := arr[2].(map[string]any)
	if !ok || deep["deep"] != true {
		t.Fatalf("the foreign array's payload must survive intact: %v", arr[2])
	}
	if nested(t, after, "server.listen_addr") != ":9000" {
		t.Fatalf("nested content must survive the top-level write: %v", after["server"])
	}
	if nested(t, after, "server.foreign_in_known_section.a") != float64(1) {
		t.Fatalf("a foreign key inside a known section must survive: %v", after["server"])
	}
	if nested(t, after, "providers.existing.url") != "http://x" {
		t.Fatalf("existing provider config must survive: %v", after["providers"])
	}
}
