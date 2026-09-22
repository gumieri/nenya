package proxy

import (
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
)

// TestSelectExtensionProvider_DeterministicFallback pins NENYA-54: when the
// preferred provider has no usable key, the fallback must pick the FIRST
// usable provider in sorted-name order — never a map-iteration random one.
func TestSelectExtensionProvider_DeterministicFallback(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Providers: map[string]*config.Provider{
			"zeta":      {Name: "zeta"},                                       // no key: unusable
			"mid":       {Name: "mid", AuthStyle: config.AuthStyleNone},       // usable
			"alpha":     {Name: "alpha", AuthStyle: config.AuthStyleNone},     // usable, sorts first
			"preferred": {Name: "preferred", AuthStyle: "bearer", APIKey: ""}, // preferred but keyless
		},
	}
	p := &Proxy{}

	got := p.selectExtensionProvider(gw, "preferred")
	if got == nil {
		t.Fatal("expected a fallback provider")
	}
	if got.Name != "alpha" {
		t.Fatalf("expected deterministic fallback to 'alpha' (sorted first), got %q", got.Name)
	}
}
