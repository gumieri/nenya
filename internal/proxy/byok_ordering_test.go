package proxy

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

// newBYOKOrderingProxy builds a proxy with TWO allow_byok providers behind a
// sticky-strategy agent, BYOK enabled, so the relative order of intent
// resolution and sticky routing is observable through the real
// validateChatRequest sequence (NENYA-208).
func newBYOKOrderingProxy(t *testing.T) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(0) // no shared limiter in this test
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Governance.BYOK = &config.BYOKConfig{Enabled: true, Header: config.DefaultBYOKHeader}
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: "http://127.0.0.1:1/v1/chat/completions", AuthStyle: "none", AllowBYOK: config.PtrTo(true)},
		"test-provider-1": {URL: "http://127.0.0.1:2/v1/chat/completions", AuthStyle: "none", AllowBYOK: config.PtrTo(true)},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "sticky",
			Models: []config.AgentModel{
				{Provider: "test-provider-0", Model: "test-model"},
				{Provider: "test-provider-1", Model: "test-model"},
			},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "primary-token-1234567890",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"key-a": {Name: "key-a", Token: "key-a-token-1234567890", Roles: []string{"user"}, Enabled: true,
				Permissions: map[string]any{"byok": true}},
		},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// TestBYOKIntentPrecedesStickyReorder pins the PRODUCTION ORDER inside
// validateChatRequest (NENYA-208): resolveBYOKIntent runs BEFORE resolveRouting,
// whose tail applies sticky routing. A session pin seeded to the SECOND built
// target must reorder a shared-account request, but must leave a BYOK
// request's built order untouched — because the intent was already resolved
// when sticky routing ran. If the intent resolution moved after routing, the
// BYOK request below would be reordered and the first assertion would fail.
func TestBYOKIntentPrecedesStickyReorder(t *testing.T) {
	p := newBYOKOrderingProxy(t)
	gw := p.Gateway()
	apiKey := &config.ApiKey{Name: "key-a", Token: "key-a-token-1234567890", Roles: []string{"user"}, Enabled: true,
		Permissions: map[string]any{"byok": true}}

	// Seed the session pin to the SECOND built target.
	pinKey := routing.SessionKeyForSubject(sessionSubjectScope(apiKey), "test-agent", "", "hi")
	gw.AgentState.SessionRouter.Pin(pinKey, "test-provider-1", "test-model", "", time.Hour)

	send := func(byok bool) *chatRequest {
		t.Helper()
		body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if byok {
			r.Header.Set(config.DefaultBYOKHeader, "nk-caller-key-0123456789abcdef")
		}
		w := httptest.NewRecorder()
		req, herr := p.validateChatRequest(w, r, gw, apiKey)
		if herr != nil {
			t.Fatalf("validateChatRequest: %d %s", herr.Code, herr.Message)
		}
		return req
	}

	// BYOK request: the intent precedes routing, so sticky routing neither
	// reads nor applies the pin — the built order stands.
	req := send(true)
	if !req.BYOKRequested {
		t.Fatal("the presented BYOK header must resolve the intent")
	}
	if len(req.Targets) != 2 {
		t.Fatalf("both allow_byok targets must survive the narrowing: %+v", req.Targets)
	}
	if req.Targets[0].Provider != "test-provider-0" {
		t.Fatalf("a BYOK request must keep the built order (the provider-1 pin must not reorder): %+v", req.Targets)
	}

	// Mutation sensitivity: the same fixture WITHOUT the BYOK header must
	// apply the pin and reorder — proving the assertion above watches the
	// ordering, not an accident of the fixture.
	req2 := send(false)
	if req2.BYOKRequested {
		t.Fatal("no header presented: the intent must stay unresolved")
	}
	if req2.Targets[0].Provider != "test-provider-1" {
		t.Fatalf("without BYOK the sticky pin must reorder the session to its pinned target: %+v", req2.Targets)
	}
}
