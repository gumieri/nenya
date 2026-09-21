package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/nenya/config"
)

// TestEffectiveIdleConnTimeout pins the NENYA-47 resolution: explicit
// config wins, zero falls back to the 90s conservative default that sits
// below every known provider keep-alive horizon (Google Frontend ~240s).
func TestEffectiveIdleConnTimeout(t *testing.T) {
	if got := (&config.Provider{}).EffectiveIdleConnTimeout(); got != 90*time.Second {
		t.Errorf("zero config: expected 90s default, got %v", got)
	}
	if got := (*config.Provider)(nil).EffectiveIdleConnTimeout(); got != 90*time.Second {
		t.Errorf("nil provider: expected 90s default, got %v", got)
	}
	p := &config.Provider{IdleConnTimeoutSeconds: 210}
	if got := p.EffectiveIdleConnTimeout(); got != 210*time.Second {
		t.Errorf("explicit config: expected 210s (Google case), got %v", got)
	}
}

// TestBuildProviderClients_IdleTimeoutClone pins that a provider with a
// non-default idle timeout gets its own cloned transport (and keeps the
// base pool untouched), while default providers share the base client.
func TestBuildProviderClients_IdleTimeoutClone(t *testing.T) {
	base := &http.Transport{IdleConnTimeout: 90 * time.Second}
	providers := map[string]*config.Provider{
		"default-provider": {URL: "http://a"},
		"google-provider":  {URL: "http://b", IdleConnTimeoutSeconds: 210},
	}

	clients := buildProviderClients(base, providers)

	if _, ok := clients["default-provider"]; ok {
		t.Error("default provider must share the base transport")
	}
	google, ok := clients["google-provider"]
	if !ok {
		t.Fatal("google provider must get a dedicated client for its 210s idle timeout")
	}
	tp, ok := google.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected cloned *http.Transport, got %T", google.Transport)
	}
	if tp.IdleConnTimeout != 210*time.Second {
		t.Errorf("expected cloned transport IdleConnTimeout=210s, got %v", tp.IdleConnTimeout)
	}
	if tp == base {
		t.Error("clone must not alias the base transport")
	}
}

// TestEvictIdleConnections_NoPanicOnEmptyGateway checks the eviction hook
// is safe when the gateway has no providers/clients wired (partial
// construction in tests / early startup).
func TestEvictIdleConnections_NoPanicOnEmptyGateway(t *testing.T) {
	g := &NenyaGateway{}
	g.EvictIdleConnections("any-provider") // must not panic
	g.EvictIdleConnections("")
}
