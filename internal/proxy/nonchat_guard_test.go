package proxy

import (
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
)

func TestIsNonChatModel_LiveConfig(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{}}
	if err := config.ApplyDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	provs := config.ResolveProviders(cfg, &config.SecretsConfig{ProviderKeys: map[string]string{"zen": "k"}})
	if provs["zen"].IsNonChatModel("jev-1.13-free") != true {
		t.Fatal("builtin zen must classify jev-1.13-free as non-chat")
	}

	gw := &gateway.NenyaGateway{
		Logger:       testLog(t),
		Providers:    provs,
		ModelCatalog: discovery.NewModelCatalog(),
	}
	// Unknown to the catalog and no provider opines as chat: refuse.
	if !isNonChatModel(gw, "jev-1.13-free") {
		t.Fatal("isNonChatModel(jev-1.13-free) = false; want true (chat must be refused)")
	}
	// Unknown model with no non-chat classification anywhere: allow.
	if isNonChatModel(gw, "some-unknown-model") {
		t.Fatal("isNonChatModel(some-unknown-model) = true; want false")
	}
}
