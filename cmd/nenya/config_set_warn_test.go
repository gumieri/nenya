package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigSet_WarnsOnSecretReferenceUnderCredentialKey pins AUD-74
// (NENYA-300): writing an env:/file: value under a credential key must say
// on stderr that the literal is written VERBATIM — reference resolution
// runs only over secrets.json provider_keys, so a reference in the config
// file is a dead literal and belongs there. (The first wording of this note
// claimed the value "will be resolved at boot", and the second claimed
// config-file api_key is "not the runtime credential" — both false: only
// the resolution half is secrets-only, the literal itself serves via the
// provider's default account. Corrected after review of the shipped
// 0.19.0.)
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
	if !strings.Contains(errBuf.String(), "written verbatim") {
		t.Fatalf("an env: literal under a credential key must warn it is NOT reference-resolved, stderr:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "only in secrets.json provider_keys") {
		t.Fatalf("the note must point at the surface where references live, stderr:\n%s", errBuf.String())
	}
}

// TestConfigSet_WarnsOnNullCredential pins the null half of AUD-74: writing
// JSON null under a credential key empties the config-file leaf — the
// operator must be told, and told the serving credential is unaffected.
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
	if !strings.Contains(errBuf.String(), "removes this config-file credential") {
		t.Fatalf("a null credential write must warn, stderr:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "unaffected") {
		t.Fatalf("the note must say the provider_keys credential is unaffected, stderr:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "becomes credential-less") {
		t.Fatalf("the note must cover the no-provider_keys case, stderr:\n%s", errBuf.String())
	}
}
