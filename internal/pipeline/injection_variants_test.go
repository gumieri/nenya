package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// injectionVariantCorpus is the NENYA-147 adversarial variant corpus for the
// tier-1 injection patterns: every obfuscation shape from the upstream bypass
// taxonomy, with whether the deterministic pass must report it.
type injectionVariantCorpus struct {
	Cases []struct {
		Name     string `json:"name"`
		Text     string `json:"text"`
		Detected bool   `json:"detected"`
		// Absent, when set, must not survive in the sanitized copy: it makes a
		// case discriminating (a detection count alone can pass for the wrong
		// reason, e.g. an unrelated decoy hit).
		Absent string `json:"absent,omitempty"`
	} `json:"cases"`
}

// TestInjectionVariantTaxonomy runs the committed adversarial corpus. A
// detected=true case must produce at least one tier-1 hit; a detected=false case
// is either benign traffic that must stay clean or a documented limitation the
// corpus pins (so a future normalization change cannot silently claim coverage).
func TestInjectionVariantTaxonomy(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "injection_variants.json"))
	if err != nil {
		t.Fatalf("reading corpus: %v", err)
	}
	var corpus injectionVariantCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("parsing corpus: %v", err)
	}
	if len(corpus.Cases) == 0 {
		t.Fatal("corpus is empty")
	}

	interceptor := newTestInjectionInterceptor(t, &config.InjectionConfig{Enabled: config.PtrTo(true)}, nil, nil)
	for _, tc := range corpus.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			cleaned, hits, _ := interceptor.scan(tc.Text)
			if (hits > 0) != tc.Detected {
				t.Fatalf("scan(%q) hits = %d, want detected=%v", tc.Text, hits, tc.Detected)
			}
			if tc.Absent != "" && strings.Contains(cleaned, tc.Absent) {
				t.Fatalf("scan(%q) left the payload %q in the sanitized copy: %q", tc.Text, tc.Absent, cleaned)
			}
		})
	}
}
