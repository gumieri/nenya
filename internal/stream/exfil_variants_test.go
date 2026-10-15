package stream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nenya/config"
)

// exfilVariantCorpus is the NENYA-147 adversarial variant corpus: the URL
// shapes an allowlist-style policy leaks on, each as a fixture with its
// expected verdict.
type exfilVariantCorpus struct {
	Cases []struct {
		Name           string   `json:"name"`
		URL            string   `json:"url"`
		Reason         string   `json:"reason"`
		AllowedHosts   []string `json:"allowed_hosts"`
		AllowIPLiteral bool     `json:"allow_ip_literals"`
	} `json:"cases"`
}

// TestExfilGuardVariantTaxonomy runs the committed adversarial corpus against
// the URL policy. Every upstream bypass shape (integer IP literals, IPv6 zone
// identifiers, userinfo tricks, percent-encoded hosts, scheme-relative and
// non-http markdown links, case and trailing-dot normalization) must produce
// the documented verdict.
func TestExfilGuardVariantTaxonomy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "exfil_url_variants.json"))
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	var corpus exfilVariantCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			cfg := config.ExfilGuardConfig{Action: config.ExfilActionLog, AllowedHosts: tc.AllowedHosts}
			if tc.AllowIPLiteral {
				cfg.AllowIPLiterals = config.PtrTo(true)
			}
			guard := NewExfilGuard(cfg, nil)
			_, _, reason := guard.InspectFull(tc.URL)
			if reason != tc.Reason {
				t.Fatalf("InspectFull(%q) reason = %q, want %q", tc.URL, reason, tc.Reason)
			}
		})
	}
}
