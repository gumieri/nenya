package proxy

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/testutil"
)

// newTelemetryProxy builds a proxy with the primary client token plus a
// read-only key, a user key, and a disabled key, exercising the NENYA-131
// telemetry auth surface.
func newTelemetryProxy(t *testing.T, unauthenticated bool) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.TelemetryUnauthenticated = unauthenticated
	// Mirror newTestProxy: the engine reference keeps /healthz healthy in
	// the test environment (MinimalConfig alone leaves the engine unset).
	cfg.Bouncer.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
		ApiKeys: map[string]config.ApiKey{
			"monitoring": {Name: "monitoring", Token: "nk-monitoring-token-0123456789", Roles: []string{"read-only"}, Enabled: true},
			"app":        {Name: "app", Token: "nk-app-token-0123456789abcdef", Roles: []string{"user"}, Enabled: true},
			"retired":    {Name: "retired", Token: "nk-retired-token-0123456789ab", Roles: []string{"user"}, Enabled: false},
		},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// telemetryRequest builds a GET with a controlled Authorization header —
// unlike testutil.NewTestRequest, which always authenticates as the primary
// token.
func telemetryRequest(path, authHeader string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	return req
}

func TestTelemetry_Statsz_RequiresAuth(t *testing.T) {
	cases := []struct {
		name       string
		authHeader string
		want       int
	}{
		{name: "no token", authHeader: "", want: http.StatusUnauthorized},
		{name: "wrong token", authHeader: "Bearer wrong-token", want: http.StatusForbidden},
		{name: "primary token", authHeader: "Bearer test-token", want: http.StatusOK},
		{name: "read-only key GET", authHeader: "Bearer nk-monitoring-token-0123456789", want: http.StatusOK},
		{name: "disabled key", authHeader: "Bearer nk-retired-token-0123456789ab", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTelemetryProxy(t, false)
			rec := doTelemetryRequest(p, telemetryRequest("/statsz", tc.authHeader))
			if rec.Code != tc.want {
				t.Fatalf("/statsz %s: got %d, want %d (body: %s)", tc.name, rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestTelemetry_Metrics_RequiresAuth(t *testing.T) {
	cases := []struct {
		name       string
		authHeader string
		want       int
	}{
		{name: "no token", authHeader: "", want: http.StatusUnauthorized},
		{name: "wrong token", authHeader: "Bearer wrong-token", want: http.StatusForbidden},
		{name: "primary token", authHeader: "Bearer test-token", want: http.StatusOK},
		{name: "user-role key GET", authHeader: "Bearer nk-app-token-0123456789abcdef", want: http.StatusOK},
		{name: "read-only key GET", authHeader: "Bearer nk-monitoring-token-0123456789", want: http.StatusOK},
		{name: "disabled key", authHeader: "Bearer nk-retired-token-0123456789ab", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTelemetryProxy(t, false)
			rec := doTelemetryRequest(p, telemetryRequest("/metrics", tc.authHeader))
			if rec.Code != tc.want {
				t.Fatalf("/metrics %s: got %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

func TestTelemetry_Healthz_StaysUnauthenticated(t *testing.T) {
	p := newTelemetryProxy(t, false)
	rec := doTelemetryRequest(p, telemetryRequest("/healthz", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz without token: got %d, want 200", rec.Code)
	}
}

func TestTelemetry_Statsz_AuthenticatedIncludesKeyUsage(t *testing.T) {
	p := newTelemetryProxy(t, false)
	rec := doTelemetryRequest(p, telemetryRequest("/statsz", "Bearer test-token"))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	usage, ok := body["key_usage"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected key_usage object in authenticated /statsz, got: %s", rec.Body.String())
	}
	if _, ok := usage["keys"]; !ok {
		t.Error("authenticated /statsz should expose the per-key usage section")
	}
}

func TestTelemetry_UnauthenticatedMode_RedactsKeyNames(t *testing.T) {
	p := newTelemetryProxy(t, true)

	rec := doTelemetryRequest(p, telemetryRequest("/statsz", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /statsz in opt-out mode: got %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "monitoring") || strings.Contains(body, "retired") {
		t.Error("unauthenticated /statsz must never expose key names")
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	usage, ok := parsed["key_usage"].(map[string]interface{})
	if !ok {
		t.Fatal("expected key_usage object to remain present in redacted mode")
	}
	if usage["keys_redacted"] != true {
		t.Errorf("expected keys_redacted=true marker, got: %v", usage["keys_redacted"])
	}
	if _, ok := usage["keys"]; ok {
		t.Error("redacted key_usage must not contain the keys section")
	}

	rec = doTelemetryRequest(p, telemetryRequest("/metrics", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /metrics in opt-out mode: got %d, want 200", rec.Code)
	}
	// The opt-out mode must also redact API key names from the Prometheus
	// exposition: auth metrics carry a key_name label that would otherwise
	// enumerate every key (NENYA-131 review finding). Generate real auth
	// traffic first — an empty exposition would make this assertion vacuous.
	auth := doTelemetryRequest(p, telemetryRequest("/v1/models", "Bearer nk-monitoring-token-0123456789"))
	if auth.Code != http.StatusOK {
		t.Fatalf("authenticated traffic for label generation: got %d, want 200", auth.Code)
	}
	rec = doTelemetryRequest(p, telemetryRequest("/metrics", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("unauthenticated /metrics scrape: got %d, want 200", rec.Code)
	}
	metricsBody := rec.Body.String()
	if !strings.Contains(metricsBody, `key_name="`+infra.RedactedAuthLabel+`"`) {
		t.Error("unauthenticated /metrics must expose auth counters under the redacted label (traffic existed)")
	}
	for _, keyName := range []string{"monitoring", "retired", "nk-app-token"} {
		if strings.Contains(metricsBody, keyName) {
			t.Errorf("unauthenticated /metrics leaks key name %q via metric labels", keyName)
		}
	}
}

func doTelemetryRequest(p *Proxy, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}
