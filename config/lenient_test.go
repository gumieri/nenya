package config

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureDefaultLog swaps slog's default logger for one writing into buf.
func captureDefaultLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestLoad_UnknownFieldsLenient pins the NENYA-19 contract: unknown fields
// at any level never fail load — the config parses with the known fields —
// and a warning names at least one of the unknown fields.
func TestLoad_UnknownFieldsLenient(t *testing.T) {
	buf := captureDefaultLog(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{
		"server": {"listen_addr": ":9999", "liseten_addr": ":typo"},
		"governance": {"ratelimit_max_rpm": 30, "future_field": true},
		"providers": {"zai": {"url": "https://api.z.ai/v1", "vendor_tier": "gold"}},
		"completely_new_section": {"x": 1}
	}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unknown fields must never fail load: %v", err)
	}
	if cfg.Server.ListenAddr != ":9999" {
		t.Errorf("known field lost: %q", cfg.Server.ListenAddr)
	}
	if cfg.Governance.RatelimitMaxRPM == nil || *cfg.Governance.RatelimitMaxRPM != 30 {
		t.Error("known nested field lost")
	}

	logged := buf.String()
	if !strings.Contains(logged, "unknown field") {
		t.Errorf("expected unknown-field warning, got: %s", logged)
	}
	if !strings.Contains(logged, "liseten_addr") {
		t.Errorf("expected typo field named in warning, got: %s", logged)
	}
}

// TestLoad_MalformedJSONFails verifies the caller contract that lets the
// SIGHUP path retain the last-known-good config: syntactically broken JSON
// is an error, never silently accepted.
func TestLoad_MalformedJSONFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{\"server\": {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

// TestLoadFromDir_ConfigDUnknownField warns per partial file but still
// merges and loads cleanly.
func TestLoadFromDir_ConfigDUnknownField(t *testing.T) {
	buf := captureDefaultLog(t)

	dir := t.TempDir()
	cfgd := filepath.Join(dir, "config.d")
	if err := os.MkdirAll(cfgd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgd, "10-base.json"),
		[]byte(`{"server": {"listen_addr": ":8080"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgd, "20-extra.json"),
		[]byte(`{"governance": {"unknown_thing": 1}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("unknown field in config.d must not fail load: %v", err)
	}
	if cfg.Server.ListenAddr != ":8080" {
		t.Errorf("merge lost known value: %q", cfg.Server.ListenAddr)
	}
	if !strings.Contains(buf.String(), "unknown field") {
		t.Errorf("expected unknown-field warning, got: %s", buf.String())
	}
}

func TestUnknownFieldName(t *testing.T) {
	_, ok := unknownFieldName(errors.New("some other error"))
	if ok {
		t.Fatal("non unknown-field error must not match")
	}
	name, ok := unknownFieldName(errors.New(`json: unknown field "ratelimit_max_rpmm"`))
	if !ok || name != "ratelimit_max_rpmm" {
		t.Fatalf("got %q ok=%v", name, ok)
	}
}
