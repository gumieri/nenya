package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// runSecretSet runs `secret set` and returns the path it printed.
func runSecretSet(t *testing.T, args ...string) string {
	t.Helper()
	var buf, errBuf bytes.Buffer
	handled, err := handleSecret(&buf, &errBuf, append([]string{"secret", "set"}, args...))
	if err != nil {
		t.Fatalf("handleSecret error: %v (stderr: %s)", err, errBuf.String())
	}
	if !handled {
		t.Fatal("expected secret set to be handled")
	}
	return strings.TrimSpace(buf.String())
}

func TestSecretSet_ProviderKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_SECRETS_DIR", dir)
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	path := runSecretSet(t, "--provider", "gemini", "AIza-test")

	want := filepath.Join(dir, "secrets.json")
	if path != want {
		t.Fatalf("printed path = %q, want %q", path, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("secrets file mode = %o, want 0600", fi.Mode().Perm())
	}
	doc := readJSONFile(t, path)
	keys, _ := doc["provider_keys"].(map[string]any)
	if keys == nil || keys["gemini"] != "AIza-test" {
		t.Errorf("provider_keys = %v, want gemini=AIza-test", doc["provider_keys"])
	}
}

func TestSecretSet_DirectoryModeDefaultsToConfigRoot(t *testing.T) {
	stubDefaultSecretsDirPopulated(t, false)
	dir := t.TempDir()
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	path := runSecretSet(t, "--provider", "gemini", "AIza-test")

	want := filepath.Join(dir, "secrets.json")
	if path != want {
		t.Fatalf("printed path = %q, want the config-root file %q", path, want)
	}
	doc := readJSONFile(t, path)
	keys, _ := doc["provider_keys"].(map[string]any)
	if keys == nil || keys["gemini"] != "AIza-test" {
		t.Errorf("provider_keys = %v, want gemini=AIza-test", doc["provider_keys"])
	}
}

func TestSecretTargetPath_FileModeFallsBackToRunSecrets(t *testing.T) {
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got, err := secretTargetPath(configPaths{file: "/etc/nenya/custom.json"})
	if err != nil {
		t.Fatalf("secretTargetPath: %v", err)
	}
	if want := "/run/secrets/nenya/secrets.json"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

// stubDefaultSecretsDirPopulated overrides the /run/secrets/nenya probe for the
// duration of a test, so the shadow path is exercised without host control.
func stubDefaultSecretsDirPopulated(t *testing.T, populated bool) {
	t.Helper()
	saved := defaultSecretsDirPopulated
	defaultSecretsDirPopulated = func() bool { return populated }
	t.Cleanup(func() { defaultSecretsDirPopulated = saved })
}

func TestSecretTargetPath_DirectoryModeDefaultsToConfigRoot(t *testing.T) {
	stubDefaultSecretsDirPopulated(t, false)
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	dir := t.TempDir()

	got, err := secretTargetPath(configPaths{dir: dir})
	if err != nil {
		t.Fatalf("secretTargetPath: %v", err)
	}
	if want := filepath.Join(dir, "secrets.json"); got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

func TestSecretTargetPath_ShadowedByDefaultDirFailsClosed(t *testing.T) {
	stubDefaultSecretsDirPopulated(t, true)
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	if _, err := secretTargetPath(configPaths{dir: t.TempDir()}); err == nil {
		t.Fatal("expected fail-closed when /run/secrets/nenya holds secrets that would shadow the write")
	}
}

func TestSecretTargetPath_CredentialsDirWithoutFilesDoesNotFailClosed(t *testing.T) {
	stubDefaultSecretsDirPopulated(t, false)
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir()) // empty: no credential files
	dir := t.TempDir()

	got, err := secretTargetPath(configPaths{dir: dir})
	if err != nil {
		t.Fatalf("secretTargetPath: %v", err)
	}
	if want := filepath.Join(dir, "secrets.json"); got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

func TestSecretTargetPath_ConfigRootIsDefaultDirNotShadowed(t *testing.T) {
	stubDefaultSecretsDirPopulated(t, true)
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got, err := secretTargetPath(configPaths{dir: "/run/secrets/nenya"})
	if err != nil {
		t.Fatalf("secretTargetPath: %v", err)
	}
	if want := "/run/secrets/nenya/secrets.json"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
}

func TestSecretTargetPath_SecretsDirAsFileWritesItDirectly(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "vault.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NENYA_SECRETS_DIR", file)
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got, err := secretTargetPath(configPaths{})
	if err != nil {
		t.Fatalf("secretTargetPath: %v", err)
	}
	if got != file {
		t.Errorf("target = %q, want the file target %q", got, file)
	}
}

func TestSecretSet_FlagErrorDoesNotEchoValue(t *testing.T) {
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	var out, errBuf bytes.Buffer
	handled, err := handleSecret(&out, &errBuf, []string{"secret", "set", "--client-token=SUPERSECRETVALUE"})
	if !handled || !errors.Is(err, errUsage) {
		t.Fatalf("handled=%v err=%v, want errUsage", handled, err)
	}
	if strings.Contains(errBuf.String(), "SUPERSECRETVALUE") {
		t.Errorf("stderr echoed the flag value: %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "nenya secret set") {
		t.Errorf("expected usage on stderr, got %q", errBuf.String())
	}
}

func TestSecretSet_ClientTokenGeneratedAndPreserves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_SECRETS_DIR", dir)
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	runSecretSet(t, "--provider", "gemini", "AIza-test")
	path := runSecretSet(t, "--client-token")

	doc := readJSONFile(t, path)
	token, _ := doc["client_token"].(string)
	if !strings.HasPrefix(token, "nk-") || len(token) <= len("nk-") {
		t.Errorf("generated client_token = %q, want a non-empty nk- token", token)
	}
	keys, _ := doc["provider_keys"].(map[string]any)
	if keys == nil || keys["gemini"] != "AIza-test" {
		t.Errorf("provider_keys not preserved: %v", doc["provider_keys"])
	}

	// An explicit token replaces the generated one and keeps other keys.
	runSecretSet(t, "--client-token", "explicit-token-1234567890")
	doc = readJSONFile(t, path)
	if doc["client_token"] != "explicit-token-1234567890" {
		t.Errorf("client_token = %v, want the explicit value", doc["client_token"])
	}
	if keys, _ := doc["provider_keys"].(map[string]any); keys == nil || keys["gemini"] != "AIza-test" {
		t.Errorf("provider_keys not preserved after client-token set: %v", doc["provider_keys"])
	}
}

func TestSecretSet_RefusesSystemdCredentialSource(t *testing.T) {
	credDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(credDir, "secrets"), []byte(`{"client_token":"x-1234567890"}`), 0o600); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", credDir)
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())

	handled, err := handleSecret(io.Discard, io.Discard, []string{"secret", "set", "--client-token"})
	if !handled {
		t.Fatal("expected secret set to handle the invocation")
	}
	if err == nil {
		t.Fatal("expected an error when the systemd credential source is active")
	}
	if !strings.Contains(err.Error(), "systemd credential") {
		t.Errorf("error = %q, want it to mention the systemd credential source", err)
	}
}

func TestSecretSet_UsageErrors(t *testing.T) {
	cases := map[string][]string{
		"missing_set":       {"secret"},
		"no_flags":          {"secret", "set"},
		"both_flags":        {"secret", "set", "--provider", "gemini", "--client-token"},
		"provider_no_value": {"secret", "set", "--provider", "gemini"},
		"unknown_flag":      {"secret", "set", "--nope"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			handled, err := handleSecret(io.Discard, io.Discard, args)
			if !handled || !errors.Is(err, errUsage) {
				t.Fatalf("expected errUsage, got handled=%v err=%v", handled, err)
			}
		})
	}
}

func TestSecretSet_Help(t *testing.T) {
	handled, err := handleSecret(io.Discard, io.Discard, []string{"secret", "set", "-h"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got handled=%v err=%v", handled, err)
	}
}

func TestSecretSet_Passthrough(t *testing.T) {
	handled, err := handleSecret(io.Discard, io.Discard, []string{"serve"})
	if handled || err != nil {
		t.Errorf("expected passthrough, got handled=%v err=%v", handled, err)
	}
}

// TestSecretSet_ResultLoads proves the written file is a valid secrets document.
func TestSecretSet_ResultLoads(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_SECRETS_DIR", dir)
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	runSecretSet(t, "--provider", "gemini", "AIza-test")
	runSecretSet(t, "--client-token", "explicit-token-1234567890")

	res, err := config.ResolveSecrets("")
	if err != nil {
		t.Fatalf("ResolveSecrets: %v", err)
	}
	if res.Secrets == nil {
		t.Fatal("expected the written secrets to resolve")
	}
	if res.Secrets.ProviderKeys["gemini"] != "AIza-test" {
		t.Errorf("provider key = %q, want AIza-test", res.Secrets.ProviderKeys["gemini"])
	}
	if res.Secrets.ClientToken != "explicit-token-1234567890" {
		t.Errorf("client token = %q, want the explicit value", res.Secrets.ClientToken)
	}
}
