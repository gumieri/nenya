package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// affinityGW builds a gateway with a header-convention provider and a
// body-convention provider (the default).
func affinityGW() *gateway.NenyaGateway {
	cfg := config.Config{}
	cfg.PrefixCache.Enabled = true
	return &gateway.NenyaGateway{
		Config: cfg,
		Providers: map[string]*config.Provider{
			"codexish": {
				Name: "codexish", URL: "https://codex.test/v1/chat/completions",
				ApiFormat: "openai", AuthStyle: "none",
				CacheAffinity: "header",
			},
			"plain": {
				Name: "plain", URL: "https://plain.test/v1/chat/completions",
				ApiFormat: "openai", AuthStyle: "none",
			},
		},
		Secrets: &config.SecretsConfig{ClientToken: "client-token"},
		Logger:  infra.SetupLogger(false),
	}
}

// TestCacheAffinityHeaderConvention pins the per-provider knob: a
// cache_affinity=header provider receives the per-conversation session
// identity on the Session-Id header (sanitation parity with the session
// machinery), while the default body convention does not inject the header.
func TestCacheAffinityHeaderConvention(t *testing.T) {
	gw := affinityGW()
	src := http.Header{}
	src.Set(opencodeSessionHeader, "nenya-abcd1234")
	prx := &Proxy{}

	req, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://codex.test/v1/chat/completions", []byte(`{}`), "codexish", "", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Session-Id"); got != "nenya-abcd1234" {
		t.Fatalf("the header convention must carry the session identity, got %q", got)
	}

	req, err = prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://plain.test/v1/chat/completions", []byte(`{}`), "plain", "", "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Session-Id"); got != "" {
		t.Fatalf("the body convention must not inject the header, got %q", got)
	}

	// Sanitation parity: an unusable identity is not forwarded.
	bad := http.Header{}
	bad.Set(opencodeSessionHeader, "bad\x01identity")
	req, err = prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://codex.test/v1/chat/completions", []byte(`{}`), "codexish", "", "", bad)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Header.Get("Session-Id"); got != "" {
		t.Fatalf("an unusable identity must not reach the header, got %q", got)
	}
}

// TestCacheAffinityConfigValidation pins the knob's validation: body,
// header, and empty are valid; anything else is rejected at boot.
func TestCacheAffinityConfigValidation(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for name, affinity := range map[string]string{"good": "header", "body": "body", "empty": ""} {
		cfg := &config.Config{Providers: map[string]config.ProviderConfig{
			name: {URL: "https://x.test", CacheAffinity: affinity},
		}}
		if err := config.ValidateConfiguration(context.Background(), cfg, &config.SecretsConfig{}, logger); err != nil {
			t.Fatalf("cache_affinity %q must be valid, got %v", affinity, err)
		}
	}
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"bogus": {URL: "https://x.test", CacheAffinity: "cookie"},
	}}
	err := config.ValidateConfiguration(context.Background(), cfg, &config.SecretsConfig{}, logger)
	if err == nil || !strings.Contains(err.Error(), "cache_affinity") {
		t.Fatalf("expected the bogus cache_affinity rejection, got: %v", err)
	}
}
