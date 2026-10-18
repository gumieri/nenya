package config

import (
	"log/slog"
	"strings"
	"testing"
)

func byokBase() *Config {
	return &Config{
		Governance: GovernanceConfig{BYOK: &BYOKConfig{Enabled: true}},
		Providers:  map[string]ProviderConfig{"anthropic": {}},
	}
}

func TestBYOKHeaderDefault(t *testing.T) {
	if got := (*BYOKConfig)(nil).BYOKHeader(); got != DefaultBYOKHeader {
		t.Fatalf("nil config must fall back to the default header, got %q", got)
	}
	if got := (&BYOKConfig{}).BYOKHeader(); got != DefaultBYOKHeader {
		t.Fatalf("empty header must fall back to the default, got %q", got)
	}
	if got := (&BYOKConfig{Header: "X-Corp-Key"}).BYOKHeader(); got != "X-Corp-Key" {
		t.Fatalf("configured header must win, got %q", got)
	}
}

func TestValidateBYOKConfigHeaderName(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(&strings.Builder{}, nil))

	bad := byokBase()
	bad.Governance.BYOK.Header = "Not A Header"
	if errs := validateBYOKConfig(bad, logger); len(errs) == 0 {
		t.Fatal("a non-token header name must be rejected")
	}

	nonCanonical := byokBase()
	nonCanonical.Governance.BYOK.Header = "x-nenya-upstream-key"
	if errs := validateBYOKConfig(nonCanonical, logger); len(errs) == 0 {
		t.Fatal("a non-canonical header name must be rejected")
	}

	authorization := byokBase()
	authorization.Governance.BYOK.Header = "Authorization"
	if errs := validateBYOKConfig(authorization, logger); len(errs) == 0 {
		t.Fatal("Authorization must never carry an upstream credential")
	}

	ok := byokBase()
	ok.Governance.BYOK.Header = "X-Nenya-Upstream-Key"
	if errs := validateBYOKConfig(ok, logger); len(errs) != 0 {
		t.Fatalf("a canonical custom header must validate: %v", errs)
	}
}

// A global enable with no provider opted in is legal but warned about.
func TestValidateBYOKConfigWarnsWhenNoProviderOptsIn(t *testing.T) {
	var sb strings.Builder
	logger := slog.New(slog.NewTextHandler(&sb, nil))
	cfg := byokBase()
	if errs := validateBYOKConfig(cfg, logger); len(errs) != 0 {
		t.Fatalf("no provider opted in is a warning, not an error: %v", errs)
	}
	if !strings.Contains(sb.String(), "no provider declares allow_byok") {
		t.Fatalf("expected a warning, got %q", sb.String())
	}
}
