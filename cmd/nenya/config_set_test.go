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
