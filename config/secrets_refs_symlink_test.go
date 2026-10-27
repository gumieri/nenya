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
	if !strings.HasPrefix(got, containmentPrefix(CleanAbs(root))) {
		t.Fatalf("resolved path %q escapes the root", got)
	}
}

// TestResolveSecretFilePath_RootBehindSymlink pins the resolved-vs-resolved
// containment compare: when the config root ITSELF sits behind a symlink,
// EvalSymlinks(joined) resolves the root component too, so comparing against
// the unresolved root would falsely reject every legitimate internal
// reference. Pre-fix this test failed exactly that way.
func TestResolveSecretFilePath_RootBehindSymlink(t *testing.T) {
	realRoot := t.TempDir()
	secret := filepath.Join(realRoot, "cred.txt")
	if err := os.WriteFile(secret, []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The configured root is a symlink to the real directory.
	linkRoot := filepath.Join(t.TempDir(), "nenya-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	got, err := resolveSecretFilePath("cred.txt", linkRoot)
	if err != nil {
		t.Fatalf("an internal reference under a symlinked config root must resolve: %v", err)
	}
	if !strings.HasPrefix(got, containmentPrefix(CleanAbs(realRoot))) {
		t.Fatalf("resolved path %q escapes the real root", got)
	}

	// The escape guard still works through the symlinked root.
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(linkRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveSecretFilePath("escape", linkRoot); err == nil {
		t.Fatal("an escape through a symlinked config root must still be rejected")
	}
}

// TestResolveSecretFilePath_FilesystemRoot pins the pathological configRoot
// of "/": the containment prefix must not collapse to "//" (which rejected
// every reference with a misleading escape message).
func TestResolveSecretFilePath_FilesystemRoot(t *testing.T) {
	dir := t.TempDir() // contained under "/"
	secret := filepath.Join(dir, "c.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(string(filepath.Separator), secret)
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveSecretFilePath(rel, string(filepath.Separator))
	if err != nil {
		t.Fatalf("a reference under a / config root must resolve: %v", err)
	}
	// Compare against the RESOLVED target: TempDir may traverse symlinks on
	// some hosts (macOS /var -> /private/var).
	want, err := filepath.EvalSymlinks(secret)
	if err != nil {
		want = secret
	}
	if got != CleanAbs(want) {
		t.Fatalf("got %q, want %q", got, CleanAbs(want))
	}
}
