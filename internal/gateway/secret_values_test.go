package gateway

import (
	"encoding/json"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
)

// NENYA-350 (literal-value list via secret refs) gateway pins: each
// secret_values entry compiles into an exact-literal pattern (QuoteMeta
// — metacharacters inert), short entries are skipped, and preset
// expansion is unaffected (a custom redact_patterns list disables it;
// secret_values must not).

func TestCompileSecretValuePatternsQuoteMeta(t *testing.T) {
	secrets := &config.SecretsConfig{SecretValues: []string{
		"hunter2secret-value",
		"p@ss.w*r$d[1](x)", // regex metacharacters must be inert
	}}
	patterns := compileSecretValuePatterns(secrets, slog.Default())
	if len(patterns) != 2 {
		t.Fatalf("compiled %d patterns, want 2", len(patterns))
	}
	// The literal matches verbatim.
	if !patterns[0].MatchString("rotate hunter2secret-value today") {
		t.Fatal("literal not matched verbatim")
	}
	// The metacharacter literal matches ONLY itself: its regex-reading
	// (p@ss.w*rd[1](x) as a regex) would match different strings.
	if !patterns[1].MatchString("the key is p@ss.w*r$d[1](x) ok") {
		t.Fatal("metachar literal not matched verbatim")
	}
	if patterns[1].MatchString("p@ssXYZr$d1(x)") {
		t.Fatal("metachar literal behaved as a regex, not a literal")
	}
	for _, p := range patterns {
		// QuoteMeta's source differs from the literal only by escaping —
		// the VERBATIM match assertions above are the real contract.
		if !strings.Contains(p.String(), "\\") && !strings.Contains(p.String(), "hunter2secret-value") {
			t.Fatalf("unexpected unescaped pattern source: %q", p.String())
		}
	}
}

func TestCompileSecretValuePatternsSkipsShort(t *testing.T) {
	secrets := &config.SecretsConfig{SecretValues: []string{"abc", "hunter2secret-value"}}
	patterns := compileSecretValuePatterns(secrets, slog.Default())
	if len(patterns) != 1 {
		t.Fatalf("short entry not skipped: %d patterns", len(patterns))
	}
}

func TestCompileSecretValuePatternsEmpty(t *testing.T) {
	if patterns := compileSecretValuePatterns(nil, slog.Default()); patterns != nil {
		t.Fatalf("nil secrets compiled patterns: %v", patterns)
	}
	if patterns := compileSecretValuePatterns(&config.SecretsConfig{}, slog.Default()); patterns != nil {
		t.Fatalf("empty list compiled patterns: %v", patterns)
	}
}

func TestSecretValuesDoNotDisablePresetExpansion(t *testing.T) {
	// The corpus (NENYA-343) contract: a custom redact_patterns list
	// disables preset expansion; secret_values must not. With
	// redact_patterns unset, the credentials preset still expands AND
	// the literal is compiled alongside.
	var cfg config.Config
	raw := `{"bouncer": {"enabled": true, "redact_preset": "credentials"}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := config.ApplyDefaults(&cfg); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if len(cfg.Bouncer.RedactPatterns) == 0 {
		t.Fatal("preset did not expand")
	}
	secrets := &config.SecretsConfig{SecretValues: []string{"hunter2secret-value"}}
	patterns := compileSecretValuePatterns(secrets, slog.Default())
	all := append(append([]string{}, cfg.Bouncer.RedactPatterns...), patternSources(patterns)...)
	joined := strings.Join(all, "\n")
	if !strings.Contains(joined, regexp.QuoteMeta("hunter2secret-value")) {
		t.Fatal("literal pattern missing from the combined set")
	}
	if len(all) < 10 {
		t.Fatalf("preset patterns lost: %d", len(all))
	}
}

func patternSources(patterns []*regexp.Regexp) []string {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		out = append(out, p.String())
	}
	return out
}
