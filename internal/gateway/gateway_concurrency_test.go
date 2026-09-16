package gateway

import (
	"testing"

	"github.com/nenya/config"
)

func TestEffectiveConcurrencyLimit_Precedence(t *testing.T) {
	g := &NenyaGateway{
		Config: config.Config{Governance: config.GovernanceConfig{MaxConcurrentRequests: 9}},
		Providers: map[string]*config.Provider{
			"full": {
				Name:                  "full",
				MaxConcurrentRequests: 3,
				ModelConcurrency:      map[string]int{"glm-5.3": 5},
			},
			"provider-only": {
				Name:                  "provider-only",
				MaxConcurrentRequests: 3,
			},
			"explicit-unlimited": {
				Name:                  "explicit-unlimited",
				MaxConcurrentRequests: 3,
				ModelConcurrency:      map[string]int{"free-model": 0},
			},
			"unset": {Name: "unset"},
		},
	}

	tests := []struct {
		name          string
		provider      string
		model         string
		want          int
		wantUnlimited bool
	}{
		{"per-model override wins", "full", "glm-5.3", 5, false},
		{"provider cap when no override", "full", "other-model", 3, false},
		{"provider-only cap", "provider-only", "any", 3, false},
		{"explicit model 0 = unlimited (beats provider cap)", "explicit-unlimited", "free-model", 0, true},
		{"governance fallback", "unset", "any", 9, false},
		{"unknown provider falls back to governance", "missing", "any", 9, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := g.EffectiveConcurrencyLimit(tt.provider, tt.model)
			if tt.wantUnlimited && got != 0 {
				t.Fatalf("got %d, want unlimited (0)", got)
			}
			if !tt.wantUnlimited && got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestProviderConcurrencyLimit_ModelOverride(t *testing.T) {
	p := &config.Provider{
		MaxConcurrentRequests: 2,
		ModelConcurrency:      map[string]int{"a": 7, "b": 0},
	}
	if got := p.ConcurrencyLimit("a"); got != 7 {
		t.Fatalf("ConcurrencyLimit(a) = %d, want 7", got)
	}
	if got := p.ConcurrencyLimit("b"); got != 0 {
		t.Fatalf("ConcurrencyLimit(b) = %d, want 0 (explicit unlimited)", got)
	}
	if got := p.ConcurrencyLimit("other"); got != 2 {
		t.Fatalf("ConcurrencyLimit(other) = %d, want 2 (provider cap)", got)
	}
	if got := (*config.Provider)(nil).ConcurrencyLimit("a"); got != 0 {
		t.Fatalf("nil provider = %d, want 0", got)
	}
}
