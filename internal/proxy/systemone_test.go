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
	"github.com/nenya/internal/testutil"
)

// newSystemOneProxy builds a fully-wired proxy whose `zen` provider points its
// System One format URL at upstreamURL. Real routing, RBAC, and the rate
// limiter are exercised via p.ServeHTTP.
func newSystemOneProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(60)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"zen": {
			URL:           "https://opencode.ai/zen/v1/chat/completions",
			AuthStyle:     "bearer",
			NonChatModels: []string{`^jev-`},
			FormatURLs:    map[string]string{"systemone": upstreamURL},
		},
		"noendpoint": {
			URL:           "https://example.invalid/v1/chat/completions",
			AuthStyle:     "bearer",
			NonChatModels: []string{`^other-`},
		},
	}
	cfg.Agents = map[string]config.AgentConfig{}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"zen": "zen-key", "noendpoint": "k"},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func TestParseSystemOneModel(t *testing.T) {
	gw := &gateway.NenyaGateway{Logger: testLog(t)}

	if _, herr := parseSystemOneModel(gw, []byte(`{"model":"jev-1.13-free"}`)); herr != nil {
		t.Fatalf("valid body rejected: %+v", herr)
	}
	_, herr := parseSystemOneModel(gw, []byte(`{"state":"x"}`))
	if herr == nil || herr.Kind != "invalid_request" {
		t.Fatalf("missing model: got %+v, want invalid_request", herr)
	}
	_, herr = parseSystemOneModel(gw, []byte(`not json`))
	if herr == nil || herr.Code != http.StatusBadRequest {
		t.Fatalf("bad json: got %+v", herr)
	}
}

func TestServeSystemOne_HappyPath(t *testing.T) {
	var gotAuth string
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"jev-1.13-free","answers":{"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":100,"output_tokens":20}}`))
	}))
	defer upstream.Close()

	p := newSystemOneProxy(t, upstream.URL+"/v1/systemone")
	body := `{"model":"jev-1.13-free","state":"hi","questions":{"q":{"type":"noul","instructions":"?"}}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer zen-key" {
		t.Fatalf("upstream Authorization = %q, want provider key (client token must not leak)", gotAuth)
	}
	if gotPath != "/v1/systemone" {
		t.Fatalf("upstream path = %q, want /v1/systemone", gotPath)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("response not JSON: %v", err)
	}
	if _, ok := got["answers"]; !ok {
		t.Fatalf("response missing answers: %s", rec.Body.String())
	}
}

func TestServeSystemOne_RequiresAuth(t *testing.T) {
	p := newSystemOneProxy(t, "https://example.invalid")
	body := `{"model":"jev-1.13-free","state":"hi","questions":{}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(body))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a client token", rec.Code)
	}
}

func TestServeSystemOne_ProviderWithoutEndpoint(t *testing.T) {
	p := newSystemOneProxy(t, "https://example.invalid")
	// `noendpoint` serves ^other- but declares no systemone format URL, so it
	// is not a System One provider and resolution fails closed.
	req := httptest.NewRequest(http.MethodPost, "/v1/systemone", strings.NewReader(`{"model":"other-1","state":"hi","questions":{}}`))
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when the provider has no systemone endpoint", rec.Code)
	}
}

func TestSystemOneTokenCounts(t *testing.T) {
	input, output, ok := systemOneTokenCounts([]byte(`{"usage":{"input_tokens":7,"output_tokens":3}}`))
	if !ok || input != 7 || output != 3 {
		t.Fatalf("counts = %d/%d ok=%v, want 7/3 true", input, output, ok)
	}
	if _, _, ok := systemOneTokenCounts([]byte(`nope`)); ok {
		t.Fatal("expected ok=false for malformed body")
	}
}
