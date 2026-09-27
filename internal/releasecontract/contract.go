package releasecontract

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Platforms supported by the release archive contract (CONTRACT.md §7.1).
const (
	PlatformLinux  = "linux"
	PlatformDarwin = "darwin"
)

// BinaryMember is the archive member consumers extract by exact name.
const BinaryMember = "nenya"

// Integrity artifact names published beside the archives (CONTRACT.md §7.2).
const (
	ChecksumsName = "checksums.txt"
	BundleName    = "checksums.txt.sigstore.json"
)

// membersByPlatform is the normative archive member set from CONTRACT.md §7.1.
var membersByPlatform = map[string][]string{
	PlatformLinux:  {BinaryMember, "deploy/nenya.service", "deploy/nenya.socket"},
	PlatformDarwin: {BinaryMember, "deploy/nenya.plist"},
}

// ArchiveMembers returns the contract archive members for platform, sorted.
func ArchiveMembers(platform string) ([]string, error) {
	members, ok := membersByPlatform[platform]
	if !ok {
		return nil, fmt.Errorf("releasecontract: unknown platform %q", platform)
	}
	out := append([]string(nil), members...)
	sort.Strings(out)
	return out, nil
}

// PlatformFromArchiveName derives the contract platform from a release archive
// filename such as nenya_0.16.0_linux_amd64.tar.gz.
func PlatformFromArchiveName(name string) (string, error) {
	base := filepath.Base(name)
	switch {
	case strings.Contains(base, "_"+PlatformLinux+"_"):
		return PlatformLinux, nil
	case strings.Contains(base, "_"+PlatformDarwin+"_"):
		return PlatformDarwin, nil
	default:
		return "", fmt.Errorf("releasecontract: cannot derive platform from %q", name)
	}
}

// InspectArchive returns the sorted regular-file members stored in a .tar.gz.
func InspectArchive(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("releasecontract: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("releasecontract: %s: %w", path, err)
	}
	defer func() { _ = gz.Close() }()

	var members []string
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("releasecontract: %s: %w", path, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			members = append(members, hdr.Name)
		}
	}
	sort.Strings(members)
	return members, nil
}

// VerifyArchive checks that the archive at path contains exactly the contract
// members for platform.
func VerifyArchive(platform, path string) error {
	want, err := ArchiveMembers(platform)
	if err != nil {
		return err
	}
	got, err := InspectArchive(path)
	if err != nil {
		return err
	}
	missing, extra := memberDiff(want, got)
	if len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("releasecontract: %s: archive members violate the contract (missing %v, unexpected %v)",
			filepath.Base(path), missing, extra)
	}
	return nil
}

// VerifyChecksums verifies every artifact listed in checksums.txt against its
// SHA-256 and requires a sibling, non-empty sigstore bundle (CONTRACT.md §7.2).
func VerifyChecksums(dir string) error {
	f, err := os.Open(filepath.Join(dir, ChecksumsName))
	if err != nil {
		return fmt.Errorf("releasecontract: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	entries := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return fmt.Errorf("releasecontract: malformed checksum line %q", line)
		}
		expected, name := fields[0], fields[1]
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("releasecontract: checksum entry %q: %w", name, err)
		}
		if !strings.EqualFold(expected, got) {
			return fmt.Errorf("releasecontract: checksum mismatch for %s (want %s, got %s)", name, expected, got)
		}
		entries++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("releasecontract: %w", err)
	}
	if entries == 0 {
		return fmt.Errorf("releasecontract: %s lists no artifacts", ChecksumsName)
	}
	return verifyBundle(dir)
}

// verifyBundle requires checksums.txt.sigstore.json to exist and decode to a
// non-empty JSON object. Signature verification itself needs the release
// identity and is performed by the consumer (install.sh / nenyactl).
func verifyBundle(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, BundleName))
	if err != nil {
		return fmt.Errorf("releasecontract: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("releasecontract: %s is not a sigstore JSON bundle: %w", BundleName, err)
	}
	if len(obj) == 0 {
		return fmt.Errorf("releasecontract: %s is empty", BundleName)
	}
	return nil
}

// fileSHA256 returns the lowercase hex SHA-256 of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// memberDiff returns members present in want but not got, and in got but not
// want.
func memberDiff(want, got []string) (missing, extra []string) {
	have := make(map[string]bool, len(got))
	for _, m := range got {
		have[m] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, m := range want {
		wantSet[m] = true
		if !have[m] {
			missing = append(missing, m)
		}
	}
	for _, m := range got {
		if !wantSet[m] {
			extra = append(extra, m)
		}
	}
	return missing, extra
}
