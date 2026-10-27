package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveSecretFilePath_RejectsSymlinkEscape pins AUD-71 (NENYA-297):
// the file: traversal guard must resolve SYMLINKS, not just lexical dots —
// a symlink inside the config root pointing outside it would pass the
// prefix check and read arbitrary files. Pre-fix only ../ was rejected.
func TestResolveSecretFilePath_RejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "real-secret.txt")
	if err := os.WriteFile(secret, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A symlink inside the root pointing OUTSIDE it.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := resolveSecretFilePath("link", root); err == nil {
		t.Fatal("a symlink escaping the config root must be rejected")
	} else if !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("the error must name the escape: %v", err)
	}

	// A symlink INSIDE the root (to another file inside the root) is fine.
	if err := os.Symlink(secret, filepath.Join(root, "inner-link")); err != nil {
		t.Fatal(err)
	}
	got, err := resolveSecretFilePath("inner-link", root)
	if err != nil {
		t.Fatalf("an internal symlink must resolve: %v", err)
	}
	if !strings.HasPrefix(got, CleanAbs(root)+string(filepath.Separator)) {
		t.Fatalf("resolved path %q escapes the root", got)
	}
}
