package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// toolArgVariantCorpus is the NENYA-147 adversarial variant corpus for the MCP
// tool-argument URL policy.
type toolArgVariantCorpus struct {
	Cases []struct {
		Name         string   `json:"name"`
		Value        string   `json:"value"`
		Rejected     bool     `json:"rejected"`
		AllowedHosts []string `json:"allowed_hosts"`
	} `json:"cases"`
}

// TestToolArgURLVariantTaxonomy runs the committed adversarial corpus against
// the argument guard's URL policy: every bypass shape from the upstream
// taxonomy (integer IP literals, IPv6 zones, userinfo, percent-encoded hosts,
// non-http and scheme-relative forms, URLs embedded in command lines, bare
// denied hosts) must produce the documented decision.
func TestToolArgURLVariantTaxonomy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "tool_arg_url_variants.json"))
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	var corpus toolArgVariantCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			cfg := guard(true)
			cfg.AllowedHosts = tc.AllowedHosts
			violations, rejected := ValidateArgs(map[string]any{"url": tc.Value}, nil, cfg)
			if rejected != tc.Rejected {
				t.Fatalf("ValidateArgs(%q) rejected = %v (violations %v), want %v", tc.Value, rejected, violations, tc.Rejected)
			}
			if tc.Rejected && len(violations) == 0 {
				t.Errorf("a rejected case must carry a violation reason: %q", tc.Value)
			}
			if !tc.Rejected && len(violations) != 0 {
				t.Errorf("a clean case must carry no violation: %q -> %v", tc.Value, violations)
			}
		})
	}
}
