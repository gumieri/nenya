package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTryLoadCredFile_NonExistErrorSurfaces pins AUD-69 (NENYA-278): only a
// MISSING secrets file is a silent skip. Pre-fix tryLoadCredFile swallowed
// every read error — a mis-permissioned file (EACCES) or a directory where
// the file should be (EISDIR) was silently treated as "no credentials", so
// boot continued keyless instead of failing loudly with the real cause.
func TestTryLoadCredFile_NonExistErrorSurfaces(t *testing.T) {
	dir := t.TempDir()
	// A DIRECTORY where the secrets FILE should be: os.ReadFile fails with
	// EISDIR — deterministic regardless of the running user (a chmod-000
	// file would not fail for root).
	if err := os.Mkdir(filepath.Join(dir, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}

	secrets, err := tryLoadCredFile(dir)
	if err == nil {
		t.Fatal("a non-ENOENT read error must surface, not silently skip")
	}
	if secrets != nil {
		t.Fatalf("no secrets on error, got %+v", secrets)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "secrets")) {
		t.Fatalf("the error must reference the failing path: %v", err)
	}
}

// TestTryLoadCredFile_MissingFileStaysSilent guards the existing behavior:
// a genuinely absent secrets file is still a silent skip.
func TestTryLoadCredFile_MissingFileStaysSilent(t *testing.T) {
	secrets, err := tryLoadCredFile(t.TempDir())
	if err != nil {
		t.Fatalf("a missing secrets file is a silent skip: %v", err)
	}
	if secrets != nil {
		t.Fatalf("no secrets when the file is absent, got %+v", secrets)
	}
}
