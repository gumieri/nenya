package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidatePromptPath_Security(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CONFIG_DIR", configDir)

	promptFile, err := os.CreateTemp(configDir, "prompt-*.txt")
	if err != nil {
		t.Fatalf("failed to create temp prompt file: %v", err)
	}
	_ = promptFile.Close()

	tests := []struct {
		name    string
		path    string
		wantErr bool
		reason  string
	}{
		{
			name:    "relative traversal blocked",
			path:    "../../../etc/passwd",
			wantErr: true,
			reason:  "relative path with .. escapes",
		},
		{
			name:    "absolute path outside config dir blocked",
			path:    "/etc/passwd",
			wantErr: true,
			reason:  "absolute path escape",
		},
		{
			name:    "valid file within config dir allowed",
			path:    promptFile.Name(),
			wantErr: false,
			reason:  "within config dir",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePromptPath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("validatePromptPath() error = %v, wantErr %v (%s)", err, tt.wantErr, tt.reason)
			}
		})
	}
}

// TestValidatePromptPath_RejectsAbsoluteOutsideWorkingDirWhenConfigDirUnset
// pins the NENYA-332 hardening: with CONFIG_DIR unset AND no config loaded,
// the boundary defaults to the working directory — an absolute path outside
// it must be rejected instead of silently allowed (the loader would read any
// file on the system as a "prompt").
func TestValidatePromptPath_RejectsAbsoluteOutsideWorkingDirWhenConfigDirUnset(t *testing.T) {
	t.Setenv("CONFIG_DIR", "")
	recordConfigRoot("") // no config loaded yet
	outside := filepath.Join(t.TempDir(), "prompt.md")

	err := validatePromptPath(outside)
	if err == nil {
		t.Fatalf("absolute path outside the working directory must be rejected when CONFIG_DIR is unset (got nil for %s)", outside)
	}
}

// TestValidatePromptPath_BoundsToLoadedConfigRoot pins the NENYA-332 review
// fix: production sets NENYA_CONFIG_DIR (or --config) — never CONFIG_DIR —
// so the boundary is the LOADED config file's directory. An absolute prompt
// under the real config root is accepted; one outside it is rejected.
func TestValidatePromptPath_BoundsToLoadedConfigRoot(t *testing.T) {
	t.Setenv("CONFIG_DIR", "")
	root := t.TempDir()
	recordConfigRoot(root)
	t.Cleanup(func() { recordConfigRoot("") })

	inside := filepath.Join(root, "prompt.md")
	if err := validatePromptPath(inside); err != nil {
		t.Fatalf("an absolute prompt under the loaded config root must pass: %v", err)
	}

	outside := filepath.Join(t.TempDir(), "evil.md")
	if err := validatePromptPath(outside); err == nil {
		t.Fatalf("an absolute prompt outside the loaded config root must be rejected (got nil for %s)", outside)
	}
}

// TestValidatePromptPath_AllowsWorkingDirRelativeWhenConfigDirUnset keeps the
// tightening honest: legitimate relative prompt paths keep working without
// CONFIG_DIR.
func TestValidatePromptPath_AllowsWorkingDirRelativeWhenConfigDirUnset(t *testing.T) {
	t.Setenv("CONFIG_DIR", "")
	dir := t.TempDir()
	t.Chdir(dir)
	if err := os.WriteFile(filepath.Join(dir, "prompt.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := validatePromptPath("prompt.md"); err != nil {
		t.Fatalf("a working-directory-relative prompt must pass without CONFIG_DIR: %v", err)
	}
}

// TestValidatePromptPath_SymlinkEscape pins the NENYA-332 symlink
// containment (round 3): the lexical boundary check passes a path that sits
// inside the boundary but SYMLINKS outside it, and os.ReadFile follows
// symlinks. Three cases, mirroring secrets_refs_symlink_test.go: an escape
// symlink is rejected, an internal symlink passes, a symlinked root still
// contains.
func TestValidatePromptPath_SymlinkEscape(t *testing.T) {
	t.Setenv("CONFIG_DIR", "")
	recordConfigRoot("") // no loaded config: boundary is the cwd
	t.Cleanup(func() { recordConfigRoot("") })
	root := t.TempDir()
	t.Chdir(root)

	if err := os.WriteFile(filepath.Join(root, "real.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "secret.md")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("escape symlink rejected", func(t *testing.T) {
		link := filepath.Join(root, "escape.md")
		if err := os.Symlink(outside, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := validatePromptPath("escape.md"); err == nil {
			t.Fatal("a relative symlink escaping the working directory must be rejected")
		}
	})

	t.Run("internal symlink passes", func(t *testing.T) {
		link := filepath.Join(root, "internal.md")
		if err := os.Symlink(filepath.Join(root, "real.md"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := validatePromptPath("internal.md"); err != nil {
			t.Fatalf("a symlink resolving inside the boundary must pass: %v", err)
		}
	})

	t.Run("symlinked root still contains", func(t *testing.T) {
		// The boundary ITSELF reached through a symlink: realRoot holds the
		// file, linkRoot symlinks to it, CONFIG_DIR points at linkRoot. An
		// absolute path inside the REAL root must resolve inside the
		// LINKED root (the resolved-vs-resolved compare), and an escape
		// link under the linked root must still be rejected.
		realRoot := t.TempDir()
		if err := os.WriteFile(filepath.Join(realRoot, "real.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		linkRoot := filepath.Join(t.TempDir(), "linked")
		if err := os.Symlink(realRoot, linkRoot); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Setenv("CONFIG_DIR", linkRoot)

		if err := validatePromptPath(filepath.Join(realRoot, "real.md")); err != nil {
			t.Fatalf("an in-root absolute prompt must pass when the root itself is a symlink: %v", err)
		}

		outside := filepath.Join(t.TempDir(), "secret.md")
		if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		escape := filepath.Join(linkRoot, "escape.md")
		if err := os.Symlink(outside, escape); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		if err := validatePromptPath(escape); err == nil {
			t.Fatal("an escape symlink under a symlinked config root must be rejected")
		}
	})
}
