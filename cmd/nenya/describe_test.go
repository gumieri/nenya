package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/version"
)

// setupDescribeEnv writes a config.json (configJSON, defaulting to "{}") and a
// valid secrets.json, then points the environment at them. It returns the config
// and secrets directories, which become the resolution targets.
func setupDescribeEnv(t *testing.T, configJSON string) (configDir, secretsDir string) {
	t.Helper()
	if configJSON == "" {
		configJSON = "{}"
	}
	configDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	secretsDir = t.TempDir()
	secrets := `{"client_token":"client-token-1234567890","provider_keys":{"gemini":"provider-key"}}`
	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(secrets), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	t.Setenv("NENYA_CONFIG_DIR", configDir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", secretsDir)
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	return configDir, secretsDir
}

// runDescribeJSON runs `describe --json` and decodes the result.
func runDescribeJSON(t *testing.T, args ...string) describeJSON {
	t.Helper()
	var buf, errBuf bytes.Buffer
	handled, err := handleDescribe(&buf, &errBuf, append([]string{"describe", "--json"}, args...))
	if err != nil {
		t.Fatalf("handleDescribe error: %v (stderr: %s)", err, errBuf.String())
	}
	if !handled {
		t.Fatal("expected describe to be handled")
	}
	var got describeJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode describe JSON: %v\n%s", err, buf.String())
	}
	return got
}

// findDiagnostic returns the first diagnostic with the given code.
func findDiagnostic(diags []config.Diagnostic, code string) (config.Diagnostic, bool) {
	for _, d := range diags {
		if d.Code == code {
			return d, true
		}
	}
	return config.Diagnostic{}, false
}

func TestHandleDescribe_JSON(t *testing.T) {
	configDir, secretsDir := setupDescribeEnv(t, "{}")

	got := runDescribeJSON(t)

	if got.ContractVersion != version.ContractVersion {
		t.Errorf("contract_version = %d, want %d", got.ContractVersion, version.ContractVersion)
	}
	if got.Paths.Mode != "directory" || got.Paths.ConfigDir != configDir {
		t.Errorf("paths = %+v, want directory mode at %s", got.Paths, configDir)
	}
	if got.Secrets.ActiveSource != secretsDir {
		t.Errorf("secrets.active_source = %q, want %q", got.Secrets.ActiveSource, secretsDir)
	}
	if len(got.Secrets.Searched) != 3 {
		t.Errorf("secrets.searched = %v, want 3 entries", got.Secrets.Searched)
	}
	if !slices.Contains(got.Providers.Configured, "gemini") {
		t.Errorf("providers.configured = %v, want it to include gemini", got.Providers.Configured)
	}
	if len(got.Providers.Catalog) == 0 {
		t.Error("providers.catalog is empty")
	}
	if got.Config == nil {
		t.Error("config is nil")
	}
	if _, found := findDiagnostic(got.Diagnostics, "secrets_not_found"); found {
		t.Error("unexpected secrets_not_found diagnostic when secrets are present")
	}
}

func TestHandleDescribe_WithoutSecrets(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("NENYA_CONFIG_DIR", configDir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got := runDescribeJSON(t)

	if got.Secrets.ActiveSource != "" {
		t.Errorf("active_source = %q, want empty when no secrets exist", got.Secrets.ActiveSource)
	}
	d, found := findDiagnostic(got.Diagnostics, "secrets_not_found")
	if !found {
		t.Fatalf("expected a secrets_not_found diagnostic, got %+v", got.Diagnostics)
	}
	if d.Level != "warn" {
		t.Errorf("secrets_not_found level = %q, want warn", d.Level)
	}
}

func TestHandleDescribe_InvalidSecrets(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(configDir, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	secretsDir := t.TempDir()
	// Missing client_token makes validation fail.
	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(`{"provider_keys":{"gemini":"x"}}`), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	t.Setenv("NENYA_CONFIG_DIR", configDir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", secretsDir)
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got := runDescribeJSON(t)

	d, found := findDiagnostic(got.Diagnostics, "secrets_invalid")
	if !found {
		t.Fatalf("expected a secrets_invalid diagnostic, got %+v", got.Diagnostics)
	}
	if d.Source != secretsDir {
		t.Errorf("secrets_invalid source = %q, want %q", d.Source, secretsDir)
	}
}

func TestHandleDescribe_UnknownFieldDiagnostic(t *testing.T) {
	configDir, _ := setupDescribeEnv(t, "{}")
	dropInDir := filepath.Join(configDir, "config.d")
	if err := os.MkdirAll(dropInDir, 0o755); err != nil {
		t.Fatalf("mkdir config.d: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dropInDir, "20-typo.json"), []byte(`{"server":{"listen_addr_bogus":":1"}}`), 0o644); err != nil {
		t.Fatalf("write drop-in: %v", err)
	}

	got := runDescribeJSON(t)

	d, found := findDiagnostic(got.Diagnostics, "unknown_field")
	if !found {
		t.Fatalf("expected an unknown_field diagnostic, got %+v", got.Diagnostics)
	}
	if !strings.Contains(d.Source, "20-typo.json") {
		t.Errorf("unknown_field source = %q, want it to name the drop-in", d.Source)
	}
	if !strings.Contains(d.Message, "listen_addr_bogus") {
		t.Errorf("unknown_field message = %q, want it to name the field", d.Message)
	}
}

func TestHandleDescribe_UnknownFlagFailsClosed(t *testing.T) {
	var errBuf bytes.Buffer
	handled, err := handleDescribe(io.Discard, &errBuf, []string{"describe", "--nope"})
	if !handled || !errors.Is(err, errUsage) {
		t.Fatalf("expected errUsage, got handled=%v err=%v", handled, err)
	}
}

func TestHandleDescribe_Help(t *testing.T) {
	handled, err := handleDescribe(io.Discard, io.Discard, []string{"describe", "-h"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got handled=%v err=%v", handled, err)
	}
}

func TestHandleDescribe_Passthrough(t *testing.T) {
	handled, err := handleDescribe(io.Discard, io.Discard, []string{"serve"})
	if handled || err != nil {
		t.Errorf("expected passthrough, got handled=%v err=%v", handled, err)
	}
}

func TestHandleDescribe_MissingConfigReportsDiagnostic(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", configDir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got := runDescribeJSON(t)

	if got.Config == nil {
		t.Fatal("config is nil; want defaults when no config file exists")
	}
	d, found := findDiagnostic(got.Diagnostics, "config_not_found")
	if !found {
		t.Fatalf("expected a config_not_found diagnostic, got %+v", got.Diagnostics)
	}
	if d.Level != "warn" {
		t.Errorf("config_not_found level = %q, want warn", d.Level)
	}
	if d.Source != configDir {
		t.Errorf("config_not_found source = %q, want %q", d.Source, configDir)
	}
}

func TestHandleDescribe_MissingConfigFileReportsDiagnostic(t *testing.T) {
	configDir := t.TempDir()
	configFile := filepath.Join(configDir, "config.json")
	t.Setenv("NENYA_CONFIG_DIR", "")
	t.Setenv("NENYA_CONFIG_FILE", configFile)
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got := runDescribeJSON(t)

	if got.Config == nil {
		t.Fatal("config is nil; want defaults when the config file is missing")
	}
	d, found := findDiagnostic(got.Diagnostics, "config_not_found")
	if !found {
		t.Fatalf("expected a config_not_found diagnostic, got %+v", got.Diagnostics)
	}
	if d.Source != configFile {
		t.Errorf("config_not_found source = %q, want %q", d.Source, configFile)
	}
}
