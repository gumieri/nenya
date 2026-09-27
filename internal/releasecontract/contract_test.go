package releasecontract

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeArchive creates a .tar.gz at path whose regular-file members are names.
func writeArchive(t *testing.T, path string, names ...string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		body := []byte(name)
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %s: %v", name, err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("write body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write archive: %v", err)
	}
}

func TestArchiveMembers(t *testing.T) {
	linux, err := ArchiveMembers(PlatformLinux)
	if err != nil {
		t.Fatalf("linux: %v", err)
	}
	wantLinux := []string{"deploy/nenya.service", "deploy/nenya.socket", "nenya"}
	if !equalStrings(linux, wantLinux) {
		t.Errorf("linux members = %v, want %v", linux, wantLinux)
	}
	darwin, err := ArchiveMembers(PlatformDarwin)
	if err != nil {
		t.Fatalf("darwin: %v", err)
	}
	wantDarwin := []string{"deploy/nenya.plist", "nenya"}
	if !equalStrings(darwin, wantDarwin) {
		t.Errorf("darwin members = %v, want %v", darwin, wantDarwin)
	}
	if _, err := ArchiveMembers("windows"); err == nil {
		t.Error("expected an error for an unsupported platform")
	}
}

func TestPlatformFromArchiveName(t *testing.T) {
	cases := map[string]string{
		"nenya_0.16.0_linux_amd64.tar.gz":       PlatformLinux,
		"nenya_0.16.0_linux_arm64.tar.gz":       PlatformLinux,
		"nenya_0.16.0_darwin_amd64.tar.gz":      PlatformDarwin,
		"dist/nenya_0.16.0_darwin_arm64.tar.gz": PlatformDarwin,
	}
	for name, want := range cases {
		got, err := PlatformFromArchiveName(name)
		if err != nil {
			t.Errorf("%s: unexpected error: %v", name, err)
			continue
		}
		if got != want {
			t.Errorf("%s: platform = %q, want %q", name, got, want)
		}
	}
	if _, err := PlatformFromArchiveName("nenya_0.16.0_windows_amd64.zip"); err == nil {
		t.Error("expected an error for an unknown platform filename")
	}
}

func TestVerifyArchiveAcceptsContract(t *testing.T) {
	dir := t.TempDir()
	linux := filepath.Join(dir, "nenya_0.16.0_linux_amd64.tar.gz")
	writeArchive(t, linux, "nenya", "deploy/nenya.service", "deploy/nenya.socket")
	if err := VerifyArchive(PlatformLinux, linux); err != nil {
		t.Errorf("linux archive should satisfy the contract: %v", err)
	}
	darwin := filepath.Join(dir, "nenya_0.16.0_darwin_arm64.tar.gz")
	writeArchive(t, darwin, "nenya", "deploy/nenya.plist")
	if err := VerifyArchive(PlatformDarwin, darwin); err != nil {
		t.Errorf("darwin archive should satisfy the contract: %v", err)
	}
}

func TestVerifyArchiveRejectsDrift(t *testing.T) {
	cases := map[string][]string{
		"missing_binary":        {"deploy/nenya.service", "deploy/nenya.socket"},
		"missing_unit":          {"nenya", "deploy/nenya.service"},
		"extra_member":          {"nenya", "deploy/nenya.service", "deploy/nenya.socket", "LICENSE"},
		"wrong_platform_member": {"nenya", "deploy/nenya.plist"},
	}
	for name, members := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name+".tar.gz")
			writeArchive(t, path, members...)
			if err := VerifyArchive(PlatformLinux, path); err == nil {
				t.Errorf("expected a contract violation for %s, got nil", name)
			}
		})
	}
}

func TestVerifyChecksumsAcceptsMatchingArtifacts(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "nenya_0.16.0_linux_amd64.tar.gz")
	writeArchive(t, archive, "nenya", "deploy/nenya.service", "deploy/nenya.socket")

	sum, err := fileSHA256(archive)
	if err != nil {
		t.Fatalf("hash archive: %v", err)
	}
	writeFile(t, filepath.Join(dir, ChecksumsName), sum+"  "+filepath.Base(archive)+"\n")
	writeFile(t, filepath.Join(dir, BundleName), `{"mediaType":"application/vnd.dev.sigstore.bundle+json;version=0.3"}`)

	if err := VerifyChecksums(dir); err != nil {
		t.Fatalf("matching artifacts should verify: %v", err)
	}
}

func TestVerifyChecksumsRejectsTampering(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "nenya_0.16.0_linux_amd64.tar.gz")
	writeArchive(t, archive, "nenya")
	sum, err := fileSHA256(archive)
	if err != nil {
		t.Fatalf("hash archive: %v", err)
	}
	writeFile(t, filepath.Join(dir, ChecksumsName), sum+"  "+filepath.Base(archive)+"\n")
	writeFile(t, filepath.Join(dir, BundleName), `{"mediaType":"bundle"}`)

	// Tamper with the archive after checksums.txt was produced.
	if err := os.WriteFile(archive, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := VerifyChecksums(dir); err == nil {
		t.Error("expected a checksum mismatch after tampering")
	}
}

func TestVerifyChecksumsRequiresBundle(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "nenya_0.16.0_linux_amd64.tar.gz")
	writeArchive(t, archive, "nenya")
	sum, err := fileSHA256(archive)
	if err != nil {
		t.Fatalf("hash archive: %v", err)
	}
	writeFile(t, filepath.Join(dir, ChecksumsName), sum+"  "+filepath.Base(archive)+"\n")

	if err := VerifyChecksums(dir); err == nil {
		t.Error("expected an error when the sigstore bundle is absent")
	}
}

// TestDistArtifacts verifies real release artifacts when NENYA_DIST_DIR points
// at a goreleaser dist/ directory (release pipeline or a local snapshot). It is
// skipped when the variable is unset so ordinary unit runs need no artifacts.
func TestDistArtifacts(t *testing.T) {
	dir := os.Getenv("NENYA_DIST_DIR")
	if dir == "" {
		t.Skip("set NENYA_DIST_DIR to a goreleaser dist/ directory to verify built artifacts")
	}
	archives, err := filepath.Glob(filepath.Join(dir, "*.tar.gz"))
	if err != nil {
		t.Fatalf("glob archives: %v", err)
	}
	if len(archives) == 0 {
		t.Fatalf("no release archives found in %s", dir)
	}
	for _, archive := range archives {
		platform, err := PlatformFromArchiveName(archive)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		if err := VerifyArchive(platform, archive); err != nil {
			t.Error(err)
		}
	}
	if err := VerifyChecksums(dir); err != nil {
		t.Fatal(err)
	}
}

// writeFile writes content to path, failing the test on error.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// equalStrings reports whether two string slices are equal, order-sensitive.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
