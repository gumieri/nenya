package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/deploy"
)

// renderUnit runs `service-unit` with args and returns the unit.
func renderUnit(t *testing.T, args ...string) string {
	t.Helper()
	var buf, errBuf bytes.Buffer
	handled, err := handleServiceUnit(&buf, &errBuf, append([]string{"service-unit"}, args...))
	if err != nil {
		t.Fatalf("handleServiceUnit error: %v (stderr: %s)", err, errBuf.String())
	}
	if !handled {
		t.Fatal("expected service-unit to be handled")
	}
	return buf.String()
}

func TestServiceUnit_SystemdDefaultsMatchShipped(t *testing.T) {
	if got := renderUnit(t); got != deploy.SystemdService {
		t.Error("default systemd unit differs from the shipped deploy/nenya.service")
	}
}

func TestServiceUnit_LaunchdDefaultsMatchShipped(t *testing.T) {
	if got := renderUnit(t, "--init", "launchd"); got != deploy.LaunchdPlist {
		t.Error("default launchd unit differs from the shipped deploy/nenya.plist")
	}
}

func TestServiceUnit_SystemdReflectsPaths(t *testing.T) {
	execPath := filepath.Join(t.TempDir(), "nenya")
	configDir := "/srv/nenya"
	secrets := "/srv/nenya/secrets.json"

	got := renderUnit(t, "--exec-path", execPath, "--config-dir", configDir, "--secrets-file", secrets)

	for _, want := range []string{
		"ExecStart=" + execPath + " --config-dir " + configDir,
		"LoadCredential=secrets:" + secrets,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("systemd unit missing %q:\n%s", want, got)
		}
	}
}

func TestServiceUnit_LaunchdReflectsPaths(t *testing.T) {
	execPath := "/opt/nenya/bin/nenya"
	configDir := "/srv/nenya"
	secrets := "/srv/nenya/creds.json"

	got := renderUnit(t, "--init", "launchd", "--exec-path", execPath, "--config-dir", configDir, "--secrets-file", secrets)

	for key, want := range map[string]string{
		"Program":           execPath,
		"NENYA_CONFIG_DIR":  configDir,
		"NENYA_SECRETS_DIR": "/srv/nenya",
	} {
		valueRe := regexp.MustCompile(`<key>` + regexp.QuoteMeta(key) + `</key>\s*<string>([^<]*)</string>`)
		m := valueRe.FindStringSubmatch(got)
		if m == nil {
			t.Errorf("launchd unit has no %s key:\n%s", key, got)
			continue
		}
		if m[1] != want {
			t.Errorf("%s = %q, want %q", key, m[1], want)
		}
	}
}

func TestServiceUnit_InvalidInitFailsClosed(t *testing.T) {
	var errBuf bytes.Buffer
	handled, err := handleServiceUnit(io.Discard, &errBuf, []string{"service-unit", "--init", "upstart"})
	if !handled {
		t.Fatal("expected service-unit to handle the invocation")
	}
	if !errors.Is(err, errUsage) {
		t.Fatalf("expected errUsage, got %v", err)
	}
	if !strings.Contains(errBuf.String(), "unknown init system") {
		t.Errorf("stderr should name the invalid init system, got %q", errBuf.String())
	}
}

func TestServiceUnit_Help(t *testing.T) {
	handled, err := handleServiceUnit(io.Discard, io.Discard, []string{"service-unit", "-h"})
	if !handled || !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("expected flag.ErrHelp, got handled=%v err=%v", handled, err)
	}
}

func TestServiceUnit_Passthrough(t *testing.T) {
	handled, err := handleServiceUnit(io.Discard, io.Discard, []string{"serve"})
	if handled || err != nil {
		t.Errorf("expected passthrough, got handled=%v err=%v", handled, err)
	}
}

// TestServiceUnit_SystemdAnalyzeVerify validates the emitted unit with the
// systemd parser when systemd-analyze is available (CONTRACT.md §4.5). The
// shipped service requires nenya.socket, so the socket unit is verified
// alongside it.
func TestServiceUnit_SystemdAnalyzeVerify(t *testing.T) {
	systemdAnalyze, lookErr := exec.LookPath("systemd-analyze")
	if lookErr != nil {
		t.Skip("systemd-analyze not available")
	}

	dir := t.TempDir()
	execPath := filepath.Join(dir, "nenya")
	if writeErr := os.WriteFile(execPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); writeErr != nil {
		t.Fatalf("write fake binary: %v", writeErr)
	}

	socketBytes, readErr := os.ReadFile(filepath.Join("..", "..", "deploy", "nenya.socket"))
	if readErr != nil {
		t.Fatalf("read socket unit: %v", readErr)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, "nenya.service"), []byte(renderUnit(t, "--exec-path", execPath)), 0o644); writeErr != nil {
		t.Fatalf("write service unit: %v", writeErr)
	}
	if writeErr := os.WriteFile(filepath.Join(dir, "nenya.socket"), socketBytes, 0o644); writeErr != nil {
		t.Fatalf("write socket unit: %v", writeErr)
	}

	cmd := exec.Command(systemdAnalyze, "verify", "nenya.service", "nenya.socket")
	cmd.Dir = dir
	out, verifyErr := cmd.CombinedOutput()
	if verifyErr != nil {
		t.Fatalf("systemd-analyze verify failed: %v\n%s", verifyErr, out)
	}
}
