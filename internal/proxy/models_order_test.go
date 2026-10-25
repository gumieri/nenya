package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/nenya/config"
)

// TestModelsListingDeterministic pins AUD-18 (NENYA-239): /v1/models must
// return the SAME order on every call. Agents are a map and the catalog
// iterates its own map, so the pre-fix listing order varied per request —
// the Anthropic dialect's first_id/last_id flapped and clients diffing the
// listing saw phantom churn. Post-fix the IDs are sorted, so repeated calls
// are identical and first_id/last_id are stable.
func TestModelsListingDeterministic(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{},
	}
	// Enough agents that Go's per-iteration map randomization virtually
	// guarantees different orders across calls pre-fix.
	names := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel"}
	for _, name := range names {
		cfg.Agents[name] = config.AgentConfig{
			Models: []config.AgentModel{{Provider: "p", Model: "m"}},
		}
	}
	gw := newTestGateway(cfg, map[string]*config.Provider{"p": {Name: "p"}})
	p := &Proxy{}

	orders := make([][]string, 0, 25)
	for i := 0; i < 25; i++ {
		rec := httptest.NewRecorder()
		p.handleModels(gw, rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("listing %d: status %d", i, rec.Code)
		}
		var resp struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("listing %d: decode: %v", i, err)
		}
		ids := make([]string, 0, len(resp.Data))
		for _, m := range resp.Data {
			ids = append(ids, m.ID)
		}
		orders = append(orders, ids)
	}

	// Every call identical.
	for i := 1; i < len(orders); i++ {
		if !slices.Equal(orders[0], orders[i]) {
			t.Fatalf("listing order varies between calls:\ncall 0: %v\ncall %d: %v", orders[0], i, orders[i])
		}
	}
	// And the shared order is sorted by ID.
	if !slices.IsSorted(orders[0]) {
		t.Fatalf("listing is not sorted by ID: %v", orders[0])
	}
}

// TestModelsAnthropicDialectStableIDs pins the dialect consequence: with a
// deterministic listing, first_id/last_id are the sorted extremes and do not
// flap between calls.
func TestModelsAnthropicDialectStableIDs(t *testing.T) {
	cfg := &config.Config{Agents: map[string]config.AgentConfig{}}
	for _, name := range []string{"alpha", "bravo", "charlie", "delta", "echo"} {
		cfg.Agents[name] = config.AgentConfig{
			Models: []config.AgentModel{{Provider: "p", Model: "m"}},
		}
	}
	gw := newTestGateway(cfg, map[string]*config.Provider{"p": {Name: "p"}})
	p := &Proxy{}

	seen := map[string]string{} // first_id -> count
	for i := 0; i < 25; i++ {
		rec := httptest.NewRecorder()
		p.handleModels(gw, rec, httptest.NewRequest(http.MethodGet, "/v1/models?dialect=anthropic", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("listing %d: status %d", i, rec.Code)
		}
		var resp struct {
			FirstID string `json:"first_id"`
			LastID  string `json:"last_id"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("listing %d: decode: %v", i, err)
		}
		if resp.FirstID != "alpha" || resp.LastID != "echo" {
			t.Fatalf("listing %d: first_id=%q last_id=%q, want alpha/echo", i, resp.FirstID, resp.LastID)
		}
		seen[resp.FirstID] = "ok"
	}
	if len(seen) != 1 {
		t.Fatalf("first_id flapped across calls: %v", seen)
	}
}
