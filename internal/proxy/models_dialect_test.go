package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
)

// modelsGW builds a gateway with one agent pseudo-model and two catalog
// models, one of which matches the provider's models_hidden patterns. The
// provider is built through ResolveProviders so the patterns are compiled
// the way production wires them.
func modelsGW() *gateway.NenyaGateway {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "public-model", Provider: "prov", OwnedBy: "prov"})
	catalog.Add(discovery.DiscoveredModel{ID: "internal-alias", Provider: "prov", OwnedBy: "prov"})
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"my-agent": {Models: []config.AgentModel{{Model: "public-model"}}},
		},
		Providers: map[string]config.ProviderConfig{
			"prov": {
				URL:          "https://prov.test/v1/chat/completions",
				ModelsHidden: []string{"^internal-"},
			},
		},
	}
	providers := config.ResolveProviders(cfg, &config.SecretsConfig{ProviderKeys: map[string]string{"prov": "sk-test"}})
	return &gateway.NenyaGateway{
		Config:       *cfg,
		Providers:    providers,
		ModelCatalog: catalog,
		Logger:       infra.SetupLogger(false),
	}
}

// serveModels runs the models handler and returns the decoded body.
func serveModels(t *testing.T, gw *gateway.NenyaGateway, dialect string) map[string]interface{} {
	t.Helper()
	p := &Proxy{}
	p.StoreGateway(gw)
	url := "/v1/models"
	if dialect != "" {
		url += "?dialect=" + dialect
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	w := httptest.NewRecorder()
	p.handleModels(gw, w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

// listIDs extracts the id set from an OpenAI-shape listing.
func listIDs(t *testing.T, body map[string]interface{}) map[string]bool {
	t.Helper()
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("missing data array: %#v", body)
	}
	ids := make(map[string]bool)
	for _, raw := range data {
		entry := raw.(map[string]interface{})
		ids[entry["id"].(string)] = true
	}
	return ids
}

// TestModelsHiddenRoutableButUnlisted pins NENYA-200: a model matching the
// provider's models_hidden patterns is absent from /v1/models on both
// dialects, while the rest of the catalog (and agent pseudo-models) list
// normally. Routability is unaffected: the hidden ID still resolves through
// the same catalog every dispatch consults.
func TestModelsHiddenRoutableButUnlisted(t *testing.T) {
	gw := modelsGW()

	body := serveModels(t, gw, "")
	ids := listIDs(t, body)
	if ids["internal-alias"] {
		t.Fatalf("the hidden model must not be listed: %#v", ids)
	}
	if !ids["public-model"] || !ids["my-agent"] {
		t.Fatalf("public catalog entries and agent pseudo-models must list: %#v", ids)
	}

	// The default shape keeps its envelope.
	if body["object"] != "list" {
		t.Fatalf("the OpenAI shape must keep object=list: %#v", body)
	}

	// The Anthropic dialect renders the same visibility.
	anthro := serveModels(t, gw, "anthropic")
	if anthro["has_more"] != false {
		t.Fatalf("anthropic listing must set has_more=false: %#v", anthro)
	}
	data, ok := anthro["data"].([]interface{})
	if !ok || len(data) == 0 {
		t.Fatalf("anthropic listing must carry entries: %#v", anthro)
	}
	for _, raw := range data {
		entry := raw.(map[string]interface{})
		if entry["type"] != "model" {
			t.Fatalf("anthropic entries must be typed \"model\": %#v", entry)
		}
		if entry["display_name"] == "" || entry["created_at"] == "" {
			t.Fatalf("anthropic entries must carry display_name and created_at: %#v", entry)
		}
		if strings.HasPrefix(entry["id"].(string), "internal-") {
			t.Fatalf("the hidden model leaked into the anthropic dialect: %#v", entry)
		}
	}
	// Pagination bounds match the rendered slice.
	first := data[0].(map[string]interface{})["id"]
	last := data[len(data)-1].(map[string]interface{})["id"]
	if anthro["first_id"] != first || anthro["last_id"] != last {
		t.Fatalf("anthropic bounds must match the slice: first=%v last=%v vs %v/%v",
			anthro["first_id"], anthro["last_id"], first, last)
	}

	// The dialect match is case-insensitive.
	if listIDs(t, serveModels(t, gw, "Anthropic")) == nil {
		t.Fatal("case-variant dialect must render")
	}

	// An empty listing renders valid anthropic pagination (null bounds).
	empty := &gateway.NenyaGateway{
		Config:    config.Config{},
		Providers: map[string]*config.Provider{},
		Logger:    infra.SetupLogger(false),
	}
	emptyBody := serveModels(t, empty, "anthropic")
	if emptyBody["first_id"] != nil || emptyBody["last_id"] != nil || emptyBody["has_more"] != false {
		t.Fatalf("empty listing must render null bounds: %#v", emptyBody)
	}
}

// TestModelsHiddenInvalidPatternRejected pins the boot gate: an invalid
// models_hidden regex is a validation error naming the provider (the raw
// config is checked — a provider whose patterns fail to compile is skipped
// by ResolveProviders and would otherwise escape the resolved-map check).
func TestModelsHiddenInvalidPatternRejected(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"prov": {URL: "https://prov.test", ModelsHidden: []string{"^internal-["}},
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	err := config.ValidateConfigurationNoPing(context.Background(), cfg, &config.SecretsConfig{}, logger)
	if err == nil || !strings.Contains(err.Error(), `providers["prov"]`) || !strings.Contains(err.Error(), "models_hidden") {
		t.Fatalf("expected the invalid models_hidden pattern rejection, got: %v", err)
	}
}

// TestHiddenModelStillRoutable pins the other half of NENYA-200: the hidden
// ID remains in the catalog, so routing resolves it — hidden is a listing
// filter, not an availability change.
func TestHiddenModelStillRoutable(t *testing.T) {
	gw := modelsGW()
	entries := gw.ModelCatalog.LookupAll("internal-alias")
	if len(entries) != 1 {
		t.Fatalf("the hidden model must stay routable through the catalog: %d entries", len(entries))
	}
	if gw.Providers["prov"].IsModelHidden("internal-alias") != true {
		t.Fatal("the pattern must classify the hidden model")
	}
	if gw.Providers["prov"].IsModelHidden("public-model") {
		t.Fatal("the pattern must not classify the public model")
	}
}
