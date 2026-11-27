package gateway

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/testutil"
)

// TestGatewayNewCompilesSecretValues pins the compile→serve composition
// (round-1 LOW): a gateway built with a secret_values entry serves the
// QuoteMeta pattern inside SecretPatterns, and the literal actually
// rewrites through the same redaction entry point the interceptors use.
func TestGatewayNewCompilesSecretValues(t *testing.T) {
	cfg := testutil.MinimalConfig()
	cfg.Bouncer.Enabled = config.PtrTo(true)
	cfg.Bouncer.RedactionLabel = "REDACTED"
	secrets := &config.SecretsConfig{
		ClientToken:  "0123456789abcdef",
		SecretValues: []string{"hunter2secret-value"},
	}
	gw := New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if gw == nil {
		t.Fatal("gateway nil")
	}
	found := false
	for _, p := range gw.SecretPatterns {
		if strings.Contains(p.String(), regexp.QuoteMeta("hunter2secret-value")) {
			found = true
		}
	}
	if !found {
		t.Fatalf("declared literal not compiled into SecretPatterns (%d patterns)", len(gw.SecretPatterns))
	}
	out := pipeline.RedactSecrets("rotate hunter2secret-value now", true, gw.SecretPatterns, cfg.Bouncer.RedactionLabel)
	if strings.Contains(out, "hunter2secret-value") || !strings.Contains(out, "REDACTED") {
		t.Fatalf("declared literal not redacted: %q", out)
	}
}
