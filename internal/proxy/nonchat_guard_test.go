package proxy

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/testutil"
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

// TestNonChatModel_ProviderAgnosticDefault pins that an operator-defined
// provider inherits the NENYA-180 provider-agnostic classification: a
// decision-only model is non-chat there even though the operator declared no
// non_chat_models of its own.
func TestNonChatModel_ProviderAgnosticDefault(t *testing.T) {
	cfg := &config.Config{Providers: map[string]config.ProviderConfig{
		"mygateway": {URL: "https://gw.example/v1/chat/completions", AuthStyle: "bearer"},
	}}
	if err := config.ApplyDefaults(cfg); err != nil {
		t.Fatal(err)
	}
	provs := config.ResolveProviders(cfg, &config.SecretsConfig{ProviderKeys: map[string]string{"mygateway": "k"}})
	if !provs["mygateway"].IsNonChatModel("jev-1.13-free") {
		t.Fatal("an operator-defined provider must inherit the provider-agnostic non_chat_models default")
	}

	gw := &gateway.NenyaGateway{
		Logger:       testLog(t),
		Providers:    provs,
		ModelCatalog: discovery.NewModelCatalog(),
	}
	if !isNonChatModel(gw, "jev-1.13-free") {
		t.Fatal("isNonChatModel(jev-1.13-free) = false; want true (chat must be refused)")
	}
	if isNonChatModel(gw, "chat-model-1") {
		t.Fatal("isNonChatModel(chat-model-1) = true; want false")
	}
}

// TestNonChatModel_ProviderAgnosticDefaultEndToEnd drives the whole NENYA-180
// path against a generic provider that advertises a decision-only model
// alongside a chat model: discovery drops the decision-only model before it
// enters the catalog (counting the drop), /v1/models does not advertise it, and
// a chat request naming it fails fast with 400 invalid_request instead of a
// doomed upstream call.
func TestNonChatModel_ProviderAgnosticDefaultEndToEnd(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.Error(w, "unexpected upstream call: "+r.URL.Path, http.StatusTeapot)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"jev-1.13-free"},{"id":"chat-model-1"}]}`))
	}))
	defer upstream.Close()

	cfg := testutil.MinimalConfig()
	cfg.Discovery.Enabled = config.PtrTo(true)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"mygateway": {URL: upstream.URL + "/v1/chat/completions", AuthStyle: "bearer"},
	}
	cfg.Agents = map[string]config.AgentConfig{}
	if err := config.ApplyDefaults(cfg); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"mygateway": "k"},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	if len(gw.ModelCatalog.LookupAll("jev-1.13-free")) != 0 {
		t.Fatal("the discovered decision-only model must not enter the chat catalog")
	}
	if len(gw.ModelCatalog.LookupAll("chat-model-1")) == 0 {
		t.Fatal("the discovered chat model must enter the chat catalog")
	}

	// /v1/models does not advertise the decision-only model.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "jev-1.13-free") {
		t.Errorf("/v1/models advertised the decision-only model: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "chat-model-1") {
		t.Errorf("/v1/models dropped the chat model: %s", rec.Body.String())
	}

	// A chat request naming it fails fast with the structured 400 class.
	body := `{"model":"jev-1.13-free","messages":[{"role":"user","content":"hi"}]}`
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /v1/chat/completions (decision-only model) = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error_kind":"invalid_request"`) {
		t.Errorf("error_kind = %s, want invalid_request", rec.Body.String())
	}

	// The drop is counted, so an operator can see the classification working.
	metricsRec := httptest.NewRecorder()
	infra.HandleMetrics(gw.Metrics, metricsRec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(metricsRec.Body.String(), `nenya_non_chat_models_dropped_total{provider="mygateway"} 1`) {
		t.Errorf("the non-chat drop counter is missing:\n%s", metricsRec.Body.String())
	}
}
