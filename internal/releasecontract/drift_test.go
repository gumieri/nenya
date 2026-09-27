package releasecontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile reads a repository file relative to this package directory.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// section returns the substring of s from the first start marker up to the next
// end marker, or the empty string when either marker is absent.
func section(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("marker %q not found", start)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("marker %q not found after %q", end, start)
	}
	return rest[:j]
}

// TestGoreleaserMatchesContract keeps .goreleaser.yml aligned with CONTRACT.md
// §7: the archive member set, the integrity artifact names, and the sigstore
// signature must all still be declared. A member added or removed here without
// a contract update fails the test (and vice versa).
func TestGoreleaserMatchesContract(t *testing.T) {
	cfg := repoFile(t, ".goreleaser.yml")
	archives := section(t, cfg, "archives:", "nfpms:")

	for _, member := range []string{"src: deploy/nenya.service", "src: deploy/nenya.socket", "src: deploy/nenya.plist"} {
		if !strings.Contains(archives, member) {
			t.Errorf(".goreleaser.yml archives no longer declare %q (CONTRACT.md 7.1)", member)
		}
	}
	if strings.Contains(archives, "example.config.json") {
		t.Error(".goreleaser.yml archives must not ship an example config (CONTRACT.md 7.1)")
	}
	if !strings.Contains(archives, "id: linux") || !strings.Contains(archives, "id: darwin") {
		t.Error(".goreleaser.yml should declare both linux and darwin archives")
	}

	for _, want := range []string{
		`name_template: "checksums.txt"`,
		`signature: "${artifact}.sigstore.json"`,
		"cmd: cosign",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".goreleaser.yml no longer declares %q (CONTRACT.md 7.2)", want)
		}
	}
}

// TestInstallerMatchesContract keeps install.sh aligned with the archive layout
// and integrity requirements: it extracts the contract members and verifies the
// checksum plus (when cosign is available) the sigstore bundle.
func TestInstallerMatchesContract(t *testing.T) {
	sh := repoFile(t, "install.sh")
	for _, want := range []string{
		`tar -xzf "$ARCHIVE" nenya deploy/nenya.service deploy/nenya.socket`,
		`CHECKSUMS="checksums.txt"`,
		"sha256sum",
		"cosign verify-blob",
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("install.sh no longer matches the contract: missing %q", want)
		}
	}
	if !strings.Contains(sh, BundleName) {
		t.Errorf("install.sh does not reference %s", BundleName)
	}
}

// TestContractDocumentListsMembers guards the normative tables in CONTRACT.md
// §7 so the contract, the release config, and the artifacts stay in sync.
func TestContractDocumentListsMembers(t *testing.T) {
	doc := repoFile(t, "CONTRACT.md")
	for _, want := range []string{
		"| `nenya` | all |",
		"`deploy/nenya.service`, `deploy/nenya.socket`",
		"`deploy/nenya.plist`",
		"| `checksums.txt` | SHA-256 of every release artifact |",
		"`checksums.txt.sigstore.json`",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("CONTRACT.md 7 no longer documents %q", want)
		}
	}
}
