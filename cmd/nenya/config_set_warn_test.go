package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigSet_WarnsOnSecretReferenceUnderCredentialKey pins AUD-74
// (NENYA-300): writing an env:/file: value under a credential key must say
// on stderr that the literal will function as a secret REFERENCE resolved at
// boot — pre-fix it was written silently, so an operator pasting a
// placeholder got a live reference (or a misread) without a hint.
func TestConfigSet_WarnsOnSecretReferenceUnderCredentialKey(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_DIR", dir)
	t.Setenv("CONFIG_FILE", cfg)

	var errBuf strings.Builder
	// handleConfig needs a config dir context; drive through the same entry
	// the CLI uses with explicit paths.
	var out strings.Builder
	args := []string{"config", "set", "--config-dir", dir, "--config", cfg, "providers.p.api_key", "env:MY_SECRET"}
	handled, err := handleConfig(&out, &errBuf, args)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v stderr=%s", handled, err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "secret reference") {
		t.Fatalf("an env: literal under a credential key must warn that it acts as a reference, stderr:\n%s", errBuf.String())
	}
}

// TestConfigSet_WarnsOnNullCredential pins the null half of AUD-74: writing
// JSON null under a credential key clears the credential — the operator must
// be told.
func TestConfigSet_WarnsOnNullCredential(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"providers":{"p":{"api_key":"existing-key-123456"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_DIR", dir)
	t.Setenv("CONFIG_FILE", cfg)

	var out, errBuf strings.Builder
	args := []string{"config", "set", "--config-dir", dir, "--config", cfg, "providers.p.api_key", "null"}
	handled, err := handleConfig(&out, &errBuf, args)
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v stderr=%s", handled, err, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "clears the credential") {
		t.Fatalf("a null credential write must warn, stderr:\n%s", errBuf.String())
	}
}
