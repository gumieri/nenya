package releasecontract

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// dagModulePrefix is the module path prefix for repository packages.
const dagModulePrefix = "github.com/nenya/"

// dagInternalPrefix is the path segment prefix for internal packages below
// the module root.
const dagInternalPrefix = "internal/"

// dagExcluded lists internal packages documented as sitting outside the
// runtime dependency DAG (see the note under "Package Dependency DAG" in
// docs/ARCHITECTURE.md).
var dagExcluded = map[string]bool{
	"testutil":        true,
	"releasecontract": true,
}

// dagData is the shared, load-once input for the DAG drift guards.
type dagData struct {
	tokens []string
	note   string
	pkgs   map[string]dagPackage
}

var (
	dagOnce   sync.Once
	dagLoaded dagData
	dagErr    error
)

// loadDagData gathers the documented DAG (tokens + out-of-DAG note) and the
// real package graph (`go list`) exactly once per test binary.
func loadDagData(t *testing.T) dagData {
	t.Helper()
	dagOnce.Do(func() {
		arch := repoFile(t, "docs/ARCHITECTURE.md")
		fence := section(t, arch, "## Package Dependency DAG\n\n```", "```")
		note := section(t, arch, "Packages outside the runtime DAG by design:", "\n\n")
		tokens, err := parseDagTokens(fence)
		if err != nil {
			dagErr = err
			return
		}
		pkgs, err := goListRepoPackages()
		if err != nil {
			dagErr = err
			return
		}
		dagLoaded = dagData{tokens: tokens, note: note, pkgs: pkgs}
	})
	if dagErr != nil {
		t.Fatalf("DAG drift-guard setup: %v", dagErr)
	}
	return dagLoaded
}

// parseDagTokens splits the DAG fence line into ordered tokens, rejecting
// empty segments, duplicates, and packages documented as outside the DAG.
func parseDagTokens(fence string) ([]string, error) {
	line := strings.TrimSpace(fence)
	if line == "" {
		return nil, fmt.Errorf("docs/ARCHITECTURE.md package DAG line is empty (check the Package Dependency DAG section)")
	}
	if strings.Contains(line, "\n") {
		return nil, fmt.Errorf("DAG must stay on a single line, got %q", line)
	}
	var tokens []string
	seen := map[string]bool{}
	for _, layer := range strings.Split(line, "->") {
		for _, name := range strings.Split(layer, ",") {
			name = strings.TrimSpace(name)
			switch {
			case name == "":
				return nil, fmt.Errorf("DAG line has an empty layer segment: %q", line)
			case seen[name]:
				return nil, fmt.Errorf("DAG line lists %q more than once", name)
			case dagExcluded[name]:
				return nil, fmt.Errorf("DAG line lists %q, which is documented as outside the runtime DAG", name)
			}
			seen[name] = true
			tokens = append(tokens, name)
		}
	}
	return tokens, nil
}

// dagPackage is one repository package as reported by `go list`.
type dagPackage struct {
	name    string   // last path element ("config", "proxy", ...)
	path    string   // module-relative path ("internal/mcp_loop" style kept whole)
	imports []string // internal package names imported
}

// goListRepoPackages runs `go list` over the repository's packages and
// returns their internal-package import edges.
func goListRepoPackages() (map[string]dagPackage, error) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		return nil, fmt.Errorf("go not found on PATH (required by the DAG drift guard): %w", err)
	}
	cmd := exec.Command(goBin, "list", "-f", "{{.ImportPath}}|{{.Imports}}",
		dagModulePrefix+"internal/...", dagModulePrefix+"config")
	cmd.Dir = filepath.Join("..", "..")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("go list: %v: %s", err, strings.TrimSpace(string(out)))
	}

	pkgs := map[string]dagPackage{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 {
			continue
		}
		path := strings.TrimPrefix(parts[0], dagModulePrefix)
		rest := strings.TrimPrefix(path, dagInternalPrefix)
		pkg := dagPackage{
			name: strings.SplitN(rest, "/", 2)[0],
			path: path,
		}
		for _, imp := range strings.Fields(strings.Trim(parts[1], "[]")) {
			if strings.HasPrefix(imp, dagModulePrefix) {
				pkg.imports = append(pkg.imports, strings.TrimPrefix(strings.TrimPrefix(imp, dagModulePrefix), dagInternalPrefix))
			}
		}
		pkgs[pkg.path] = pkg
	}
	return pkgs, nil
}

// TestDocsDAGCoversInternalPackages keeps docs/ARCHITECTURE.md's package
// dependency DAG in sync with the actual package layout: every repository
// package must appear in the DAG line (or in the documented out-of-DAG
// exclusion), and every DAG token must name a real package — so neither a
// new package nor a stale token can slip through.
func TestDocsDAGCoversInternalPackages(t *testing.T) {
	data := loadDagData(t)
	tokenSet := make(map[string]bool, len(data.tokens))
	for _, name := range data.tokens {
		tokenSet[name] = true
	}

	for path, pkg := range data.pkgs {
		isRootConfig := path == "config"
		if !isRootConfig && path != dagInternalPrefix+pkg.name {
			t.Errorf("package %s is nested below internal/<name>/; the single-token DAG cannot represent it — flatten the package or extend the DAG guard", path)
			continue
		}
		if dagExcluded[pkg.name] {
			if !strings.Contains(data.note, "`"+pkg.name+"`") {
				t.Errorf("internal/%s is excluded from the DAG but not documented in the out-of-DAG note", pkg.name)
			}
			continue
		}
		if !tokenSet[pkg.name] {
			t.Errorf("internal/%s missing from the docs/ARCHITECTURE.md package DAG — update the layer map when adding packages", pkg.name)
		}
	}

	// Reverse direction: every token must name a real package.
	for _, name := range data.tokens {
		if _, ok := data.pkgs[dagInternalPrefix+name]; !ok && name != "config" {
			t.Errorf("DAG token %q does not name a real package (stale docs/ARCHITECTURE.md entry)", name)
		}
	}
}

// TestDocsDAGMatchesImportOrder validates the documented layer order against
// the real import graph: for every import edge A -> B where both sides are
// in the documented DAG, B must sit strictly left of A (same-layer imports
// are violations too, matching the "may only import from layers to its
// left" rule).
func TestDocsDAGMatchesImportOrder(t *testing.T) {
	data := loadDagData(t)
	position := make(map[string]int, len(data.tokens))
	for i, name := range data.tokens {
		position[name] = i
	}

	for _, pkg := range data.pkgs {
		posPkg, inDAG := position[pkg.name]
		if !inDAG {
			continue
		}
		for _, dep := range pkg.imports {
			posDep, inDAGDep := position[dep]
			if !inDAGDep {
				continue
			}
			if posDep >= posPkg {
				t.Errorf("DAG order violation: %s imports %s, but %s does not sit left of %s in docs/ARCHITECTURE.md", pkg.name, dep, dep, pkg.name)
			}
		}
	}
}
