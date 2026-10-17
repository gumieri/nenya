package discovery

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestEnrichOllamaModels_RedactsURLCredentials pins NENYA-145 on the Ollama
// enrichment surface: when the configured base URL yields no /api/show
// endpoint, neither the returned error nor the debug log may echo the URL's
// userinfo credentials. The URL below is deliberately unparseable (a space in
// the host) so the redaction cannot lean on url.Parse succeeding.
func TestEnrichOllamaModels_RedactsURLCredentials(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	baseURL := "http://user:supersecret@ho st:11434"
	_, err := enrichOllamaModels(context.Background(), baseURL,
		[]DiscoveredModel{{ID: "llama3"}}, http.DefaultClient, logger)
	if err == nil {
		t.Fatal("expected an error for a base URL with no /api/show endpoint")
	}
	if strings.Contains(err.Error(), "supersecret") {
		t.Fatalf("the error leaked the URL credential: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatalf("the error must show the redacted URL: %v", err)
	}
	if strings.Contains(logBuf.String(), "supersecret") {
		t.Fatalf("the debug log leaked the URL credential: %s", logBuf.String())
	}
}
