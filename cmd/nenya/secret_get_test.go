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
)

// runSecretGet runs `secret get` and returns stdout.
func runSecretGet(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf, errBuf bytes.Buffer
	handled, err := handleSecret(&buf, &errBuf, append([]string{"secret", "get"}, args...))
	if !handled {
		t.Fatal("expected secret get to be handled")
	}
	return strings.TrimSpace(buf.String()), err
}

func writeSecrets(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
}

func TestSecretGet_ClientToken(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir, `{"client_token":"token-from-dir-123456","provider_keys":{"gemini":"AIza-x"}}`)
	t.Setenv("NENYA_SECRETS_DIR", dir)
	t.Setenv("NENYA_CONFIG_DIR", "")
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got, err := runSecretGet(t, "--client-token")
	if err != nil {
		t.Fatalf("secret get: %v", err)
	}
	if got != "token-from-dir-123456" {
		t.Errorf("token = %q, want the configured value", got)
	}
}

func TestSecretGet_ProviderKey(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir, `{"client_token":"token-123456789012","provider_keys":{"gemini":"AIza-x"}}`)
	t.Setenv("NENYA_SECRETS_DIR", dir)
	t.Setenv("NENYA_CONFIG_DIR", "")
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got, err := runSecretGet(t, "--provider", "gemini")
	if err != nil {
		t.Fatalf("secret get: %v", err)
	}
	if got != "AIza-x" {
		t.Errorf("provider key = %q, want AIza-x", got)
	}
}

func TestSecretGet_ConfigRootFallback(t *testing.T) {
	configDir := t.TempDir()
	writeSecrets(t, configDir, `{"client_token":"config-root-token-1234"}`)
	t.Setenv("NENYA_SECRETS_DIR", "")
	t.Setenv("NENYA_CONFIG_DIR", configDir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	got, err := runSecretGet(t, "--client-token")
	if err != nil {
		t.Fatalf("secret get: %v", err)
	}
	if got != "config-root-token-1234" {
		t.Errorf("token = %q, want the config-root value", got)
	}
}

func TestSecretGet_MissingProviderErrors(t *testing.T) {
	dir := t.TempDir()
	writeSecrets(t, dir, `{"client_token":"token-123456789012"}`)
	t.Setenv("NENYA_SECRETS_DIR", dir)
	t.Setenv("NENYA_CONFIG_DIR", "")
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	if _, err := runSecretGet(t, "--provider", "gemini"); err == nil {
		t.Fatal("expected an error for an unconfigured provider")
	}
}

func TestSecretGet_NoSecretsErrors(t *testing.T) {
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("NENYA_CONFIG_DIR", t.TempDir())
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	if _, err := runSecretGet(t, "--client-token"); err == nil {
		t.Fatal("expected an error when no secrets exist")
	}
}

func TestSecretGet_RequiresOneSelector(t *testing.T) {
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("CREDENTIALS_DIRECTORY", "")

	for _, args := range [][]string{{}, {"--client-token", "--provider", "gemini"}} {
		if _, err := runSecretGet(t, args...); !errors.Is(err, errUsage) {
			t.Errorf("args %v: err = %v, want errUsage", args, err)
		}
	}
}

func TestSecretGet_UnexpectedArgumentFailsClosed(t *testing.T) {
	t.Setenv("NENYA_SECRETS_DIR", t.TempDir())
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	if _, err := runSecretGet(t, "--client-token", "extra"); !errors.Is(err, errUsage) {
		t.Errorf("err = %v, want errUsage", err)
	}
}

func TestSecretGet_Help(t *testing.T) {
	handled, err := handleSecret(io.Discard, io.Discard, []string{"secret", "get", "-h"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got handled=%v err=%v", handled, err)
	}
}
