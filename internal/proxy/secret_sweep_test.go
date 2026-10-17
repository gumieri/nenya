package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nenya/internal/routing"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// sweepSecrets are the distinctive credential strings a populated config
// carries: every first-party output surface must keep them out.
var sweepSecrets = []string{
	"sk-live-PROVIDER-SECRET-9f8e7d",
	"tok-CLIENT-SECRET-1a2b3c4d",
	"acct-ACCOUNT-SECRET-5e6f7a",
	"hdr-MCP-SECRET-0a1b2c",
	"proxyuser-PROXYPASS-3c4d5e",
}

// sweepGW builds a gateway whose config carries every secret class.
func sweepGW(t *testing.T, logBuf *bytes.Buffer) *gateway.NenyaGateway {
	t.Helper()
	cfg := config.Config{
		Server: config.ServerConfig{UserAgent: "nenya-test"},
		Providers: map[string]config.ProviderConfig{
			"up": {
				URL:       "https://upstream.test/v1/chat/completions",
				AuthStyle: "bearer",
				ProxyURL:  "http://user:" + sweepSecrets[4] + "@proxy.test:3128",
				Accounts: []config.AccountConfig{
					{ID: "acct-1", Credential: sweepSecrets[2]},
				},
			},
		},
		MCPServers: map[string]config.MCPServerConfig{
			"tools": {
				URL:     "https://mcp.test/sse",
				Headers: map[string]string{"Authorization": "Bearer " + sweepSecrets[3]},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  sweepSecrets[1],
		ProviderKeys: map[string]string{"up": sweepSecrets[0]},
		ApiKeys: map[string]config.ApiKey{
			"key-1": {Name: "key-1", Token: sweepSecrets[1], Enabled: true, Roles: []string{"user"}},
		},
	}
	providers := config.ResolveProviders(&cfg, secrets)
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &gateway.NenyaGateway{
		Config:    cfg,
		Providers: providers,
		Secrets:   secrets,
		Logger:    logger,
		Metrics:   infra.NewMetrics(),
		Stats:     infra.NewUsageTracker(),
		KeyUsage:  auth.NewKeyUsageTracker(time.Now),
		AgentState: routing.NewAgentStateWithConfig(logger, infra.NewMetrics(), &config.GovernanceConfig{},
			map[string]config.AgentConfig{"agent-a": {Models: []config.AgentModel{{Model: "m"}}}}),
	}
}

// TestNoSecretInDebugLogsOrStatsz pins NENYA-145: a fully populated config
// (provider key, client token, account credential, MCP header, proxy
// userinfo) leaves no secret substring in the debug log stream or the
// /statsz snapshot — while the redaction still preserves the information
// an operator needs (provider names, hosts).
func TestNoSecretInDebugLogsOrStatsz(t *testing.T) {
	var logBuf bytes.Buffer
	gw := sweepGW(t, &logBuf)

	// Exercise the dispatch-adjacent surfaces a request touches, including
	// the debug log line that echoes a dispatch URL (the retry loop's
	// logger path, invoked directly so the assertion cannot go vacuous —
	// buildUpstreamRequest itself does not log).
	src := http.Header{}
	src.Set("Authorization", "Bearer "+sweepSecrets[1])
	prx := &Proxy{}
	if _, err := prx.buildUpstreamRequest(gw, context.Background(), "POST",
		"https://upstream.test/v1/chat/completions", []byte(`{"model":"m"}`), "up", "m", "", src); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"https://upstream.test/v1/chat/completions", nil)
	logRequestIfDebug(context.Background(), gw.Logger, req,
		"https://user:"+sweepSecrets[4]+"@upstream.test/v1/chat/completions", []byte(`{"model":"m"}`))

	// Non-vacuity control: the log stream must be non-empty and must still
	// carry the host (redaction removes credentials, not information).
	if logBuf.Len() == 0 {
		t.Fatal("the sweep is vacuous: no debug output was produced")
	}
	if !strings.Contains(logBuf.String(), "upstream.test") {
		t.Fatalf("the redacted URL lost its host: %s", logBuf.String())
	}

	// Exercise the key/usage tracker so the snapshot carries provider names
	// (proving redaction removes credentials, not all information).
	gw.KeyUsage.AllowRequest("key-1", 0)
	gw.KeyUsage.AllowProviderTokens("up", "always", 1000, 10)

	// The /statsz snapshot (unauthenticated mode included).
	p2 := &Proxy{}
	p2.StoreGateway(gw)
	w := httptest.NewRecorder()
	p2.handleStats(w, true)
	statsBody := w.Body.String()

	for name, out := range map[string]string{"debug logs": logBuf.String(), "statsz": statsBody} {
		for _, secret := range sweepSecrets {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaked %q:\n%s", name, secret, out)
			}
		}
	}

	// The snapshot must still be useful: it reports the provider by NAME
	// (checked structurally, not by string shape).
	var parsed struct {
		KeyUsage struct {
			Providers map[string]any `json:"providers"`
		} `json:"key_usage"`
	}
	if err := json.Unmarshal([]byte(statsBody), &parsed); err != nil {
		t.Fatalf("statsz is not valid JSON: %v", err)
	}
	if _, ok := parsed.KeyUsage.Providers["up"]; !ok {
		t.Fatalf("statsz must still name the provider: %s", statsBody)
	}
}

// TestRedactedURLLogging pins the log-site redaction directly: the dispatch
// debug line redacts userinfo while leaving the host visible.
func TestRedactedURLLogging(t *testing.T) {
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://up.test/x", nil)
	logRequestIfDebug(context.Background(), logger, req, "https://user:supersecret@up.test/v1/chat", []byte(`{"a":1}`))

	out := logBuf.String()
	if strings.Contains(out, "supersecret") {
		t.Fatalf("userinfo credentials leaked into the debug log: %s", out)
	}
	if !strings.Contains(out, "up.test") {
		t.Fatalf("the redacted URL must keep the host visible: %s", out)
	}
}
