package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// runPathsJSON runs `paths --json` and decodes the result.
func runPathsJSON(t *testing.T, args ...string) pathsJSON {
	t.Helper()
	var buf, errBuf bytes.Buffer
	handled, err := handlePaths(&buf, &errBuf, append([]string{"paths", "--json"}, args...))
	if err != nil {
		t.Fatalf("handlePaths error: %v (stderr: %s)", err, errBuf.String())
	}
	if !handled {
		t.Fatal("expected handlePaths to handle `paths --json`")
	}
	var got pathsJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decode paths JSON %q: %v", buf.String(), err)
	}
	return got
}

func TestHandlePaths_DirectoryMode(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", dir)
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", "")

	got := runPathsJSON(t)

	if got.Mode != "directory" {
		t.Errorf("mode = %q, want directory", got.Mode)
	}
	if got.ConfigDir != dir {
		t.Errorf("config_dir = %q, want %q", got.ConfigDir, dir)
	}
	if want := filepath.Join(dir, "config.json"); got.ConfigFile != want {
		t.Errorf("config_file = %q, want %q", got.ConfigFile, want)
	}
	if want := filepath.Join(dir, "config.d"); got.ConfigD != want {
		t.Errorf("config_d = %q, want %q", got.ConfigD, want)
	}
	if got.SecretsDir != "/run/secrets/nenya" {
		t.Errorf("secrets_dir = %q, want /run/secrets/nenya", got.SecretsDir)
	}
	if got.SecretsFile == nil || *got.SecretsFile != filepath.Join(dir, "secrets.json") {
		t.Errorf("secrets_file = %v, want %q", got.SecretsFile, filepath.Join(dir, "secrets.json"))
	}
	if got.SocketPath != nil {
		t.Errorf("socket_path = %v, want null", *got.SocketPath)
	}
	if got.Platform != runtime.GOOS {
		t.Errorf("platform = %q, want %q", got.Platform, runtime.GOOS)
	}
}

func TestHandlePaths_FileMode(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "custom.json")
	t.Setenv("NENYA_CONFIG_FILE", file)
	t.Setenv("NENYA_CONFIG_DIR", "")

	got := runPathsJSON(t)

	if got.Mode != "file" {
		t.Errorf("mode = %q, want file", got.Mode)
	}
	if got.ConfigFile != file {
		t.Errorf("config_file = %q, want %q", got.ConfigFile, file)
	}
	if got.ConfigDir != dir {
		t.Errorf("config_dir = %q, want %q", got.ConfigDir, dir)
	}
	if want := filepath.Join(dir, "config.d"); got.ConfigD != want {
		t.Errorf("config_d = %q, want %q", got.ConfigD, want)
	}
	if got.SecretsFile != nil {
		t.Errorf("secrets_file = %v, want null in file mode", *got.SecretsFile)
	}
}

// NENYA_CONFIG_DIR overrides -config-dir (CONTRACT.md §3.3).
func TestHandlePaths_EnvOverridesFlag(t *testing.T) {
	envDir := t.TempDir()
	flagDir := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", envDir)
	t.Setenv("NENYA_CONFIG_FILE", "")

	got := runPathsJSON(t, "--config-dir", flagDir)

	if got.ConfigDir != envDir {
		t.Errorf("config_dir = %q, want env dir %q", got.ConfigDir, envDir)
	}
}

func TestHandlePaths_SecretsDirFromEnv(t *testing.T) {
	secrets := t.TempDir()
	t.Setenv("NENYA_CONFIG_DIR", t.TempDir())
	t.Setenv("NENYA_CONFIG_FILE", "")
	t.Setenv("NENYA_SECRETS_DIR", secrets)

	got := runPathsJSON(t)

	if got.SecretsDir != secrets {
		t.Errorf("secrets_dir = %q, want %q", got.SecretsDir, secrets)
	}
	if got.SecretsFile == nil || *got.SecretsFile != filepath.Join(secrets, "secrets.json") {
		t.Errorf("secrets_file = %v, want %q", got.SecretsFile, filepath.Join(secrets, "secrets.json"))
	}
}

func TestHandlePaths_FileModeWithSecretsDirEnv(t *testing.T) {
	secrets := t.TempDir()
	t.Setenv("NENYA_CONFIG_FILE", filepath.Join(t.TempDir(), "custom.json"))
	t.Setenv("NENYA_CONFIG_DIR", "")
	t.Setenv("NENYA_SECRETS_DIR", secrets)

	got := runPathsJSON(t)

	if got.Mode != "file" {
		t.Errorf("mode = %q, want file", got.Mode)
	}
	if got.SecretsFile == nil || *got.SecretsFile != filepath.Join(secrets, "secrets.json") {
		t.Errorf("secrets_file = %v, want %q", got.SecretsFile, filepath.Join(secrets, "secrets.json"))
	}
}

func TestHandlePaths_Passthrough(t *testing.T) {
	handled, err := handlePaths(io.Discard, io.Discard, []string{"serve"})
	if handled || err != nil {
		t.Errorf("expected passthrough for non-paths args, got handled=%v err=%v", handled, err)
	}
}

func TestHandlePaths_UnknownFlagFailsClosed(t *testing.T) {
	var errBuf bytes.Buffer
	handled, err := handlePaths(io.Discard, &errBuf, []string{"paths", "--nope"})
	if !handled {
		t.Fatal("expected handlePaths to handle the invocation")
	}
	if !errors.Is(err, errUsage) {
		t.Fatalf("expected errUsage, got %v", err)
	}
	if !strings.Contains(errBuf.String(), "flag provided but not defined") {
		t.Errorf("stderr should name the unknown flag, got %q", errBuf.String())
	}
}

func TestHandlePaths_HelpExitsClean(t *testing.T) {
	var errBuf bytes.Buffer
	handled, err := handlePaths(io.Discard, &errBuf, []string{"paths", "-h"})
	if !handled {
		t.Fatal("expected handlePaths to handle -h")
	}
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got %v", err)
	}
	if errBuf.Len() == 0 {
		t.Error("expected usage on stderr for -h")
	}
}

func TestHandlePaths_UnexpectedArgument(t *testing.T) {
	handled, err := handlePaths(io.Discard, io.Discard, []string{"paths", "extra"})
	if !handled || !errors.Is(err, errUsage) {
		t.Fatalf("expected errUsage for a stray argument, got handled=%v err=%v", handled, err)
	}
}

func TestHandlePaths_HumanReadable(t *testing.T) {
	t.Setenv("NENYA_CONFIG_DIR", "/etc/nenya/")
	t.Setenv("NENYA_CONFIG_FILE", "")

	var buf bytes.Buffer
	handled, err := handlePaths(&buf, io.Discard, []string{"paths"})
	if !handled || err != nil {
		t.Fatalf("handlePaths: handled=%v err=%v", handled, err)
	}
	for _, want := range []string{"mode=directory", "config_dir=/etc/nenya", "socket_path=null", "platform=" + runtime.GOOS} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("human output missing %q:\n%s", want, buf.String())
		}
	}
}

func TestInspectionExitCode(t *testing.T) {
	if got := inspectionExitCode(nil); got != 0 {
		t.Errorf("nil error: exit code = %d, want 0", got)
	}
	if got := inspectionExitCode(flag.ErrHelp); got != 0 {
		t.Errorf("flag.ErrHelp: exit code = %d, want 0", got)
	}
	if got := inspectionExitCode(errUsage); got != 2 {
		t.Errorf("errUsage: exit code = %d, want 2", got)
	}
}
