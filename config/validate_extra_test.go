package config

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
)

// TestValidateHeaderFilters pins the NENYA-201 boot validation: empty
// header names and empty patterns are rejected (an empty pattern would
// match every value); a well-formed filter is silent.
func TestValidateHeaderFilters(t *testing.T) {
	providers := map[string]*Provider{
		"good": {HeaderFilters: map[string]HeaderFilter{
			"Anthropic-Beta": {Allow: []string{"extended-cache-ttl"}, Deny: []string{"context-management"}},
		}},
		"empty-name": {HeaderFilters: map[string]HeaderFilter{
			"": {Deny: []string{"x"}},
		}},
		"empty-pattern": {HeaderFilters: map[string]HeaderFilter{
			"Anthropic-Beta": {Allow: []string{""}},
		}},
		"non-canonical": {HeaderFilters: map[string]HeaderFilter{
			"anthropic-beta": {Deny: []string{"x"}},
		}},
		"case-variant": {HeaderFilters: map[string]HeaderFilter{
			"ANTHROPIC-BETA": {Deny: []string{"b"}},
		}},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	errs := validateProviders(context.Background(), providers, &NetworkConfig{}, logger)
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, `providers["empty-name"]`) || !strings.Contains(joined, "must not be empty") {
		t.Fatalf("expected the empty header name rejection, got: %s", joined)
	}
	if !strings.Contains(joined, `header_filters["Anthropic-Beta"]`) {
		t.Fatalf("expected the empty pattern rejection, got: %s", joined)
	}
	if !strings.Contains(joined, `use the canonical header name "Anthropic-Beta"`) {
		t.Fatalf("expected the non-canonical name rejection, got: %s", joined)
	}
	if !strings.Contains(joined, `providers["case-variant"]`) || !strings.Contains(joined, `use the canonical header name`) {
		t.Fatalf("expected the case-variant rejection, got: %s", joined)
	}
	if strings.Contains(joined, `providers["good"]`) {
		t.Fatalf("a well-formed filter must be silent, got: %s", joined)
	}
}
