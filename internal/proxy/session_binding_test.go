package proxy

import (
	"bytes"
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

// newSessionProxy builds a single-target proxy with two enabled API keys, so
// the session binding can be driven end to end through the real request path.
func newSessionProxy(t *testing.T, upstreamURL string) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(0) // no shared limiter in this test
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(100000)
	cfg.Providers = map[string]config.ProviderConfig{
		"test-provider-0": {URL: upstreamURL + "/v1/chat/completions", AuthStyle: "none"},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models:   []config.AgentModel{{Provider: "test-provider-0", Model: "test-model"}},
		},
	}
	secrets := &config.SecretsConfig{
		ClientToken:  "primary-token-1234567890",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"key-a": {Name: "key-a", Token: "key-a-token-1234567890", Roles: []string{"user"}, Enabled: true},
			"key-b": {Name: "key-b", Token: "key-b-token-1234567890", Roles: []string{"user"}, Enabled: true},
		},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { _ = gw.Shutdown(context.Background()) })
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

func sessionTestPayload() map[string]any {
	return map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
}

func sessionTestRequest(gw *gateway.NenyaGateway, subject string) *chatRequest {
	agent := gw.Config.Agents["test-agent"]
	return &chatRequest{
		Payload:        sessionTestPayload(),
		ModelName:      "test-agent",
		Agent:          agent,
		SessionSubject: subject,
	}
}

// TestSessionKeyBoundToCallerSubject pins the NENYA-196 binding: the same
// conversation opener under two authenticated subjects derives two different
// session keys, and the derivation is stable per subject.
func TestSessionKeyBoundToCallerSubject(t *testing.T) {
	agent := config.AgentConfig{SystemPrompt: "sys"}
	keyFor := func(token string) (string, bool) {
		req := &chatRequest{
			Payload:        sessionTestPayload(),
			ModelName:      "agent-a",
			SessionSubject: sessionSubjectScope(&config.ApiKey{Name: token, Token: token}),
		}
		return sessionKeyFromRequest(req, agent)
	}

	keyA, okA := keyFor("token-a")
	keyB, okB := keyFor("token-b")
	if !okA || !okB {
		t.Fatal("expected derivable session keys")
	}
	if keyA == keyB {
		t.Fatal("two subjects must not share a session key")
	}
	if again, _ := keyFor("token-a"); again != keyA {
		t.Fatal("session key must be stable for the same subject")
	}

	// The unbound derivation (no subject) stays available for callers without
	// an authenticated identity, differs from any bound key, and keeps the
	// 64-char digest the session-header truncation depends on.
	unbound, ok := sessionKeyFromRequest(&chatRequest{Payload: sessionTestPayload(), ModelName: "agent-a"}, agent)
	if !ok {
		t.Fatal("unbound derivation must succeed")
	}
	if len(unbound) != 64 {
		t.Fatalf("session key must be a 64-char digest, got %d chars", len(unbound))
	}
	if unbound == keyA || unbound == keyB {
		t.Fatal("unbound derivation must not collide with a bound one")
	}
}

// TestSessionBindingEndToEnd drives the real request path with two API keys
// and asserts the wiring itself (validateChatRequest → sessionSubjectScope →
// the subject-bound key): the upstream sees a different synthesized
// X-Opencode-Session per key for the same conversation, and each key's sticky
// pin is invisible to the other.
func TestSessionBindingEndToEnd(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		writeCleanChatStream(w)
	}))
	defer up.Close()

	p := newSessionProxy(t, up.URL)
	gw := p.Gateway()

	// send returns the synthesized header Nenya set on the inbound request, so
	// the assertion is exact per key rather than a distribution.
	send := func(token string) string {
		t.Helper()
		req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
			`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
		return req.Header.Get(opencodeSessionHeader)
	}

	headerA1 := send("key-a-token-1234567890")

	// Seed key A's pin with a marker account, then let key B drive the same
	// conversation: B must read its own (absent) pin and never observe A's.
	subjectA := sessionSubjectScope(&config.ApiKey{Name: "key-a", Token: "key-a-token-1234567890"})
	subjectB := sessionSubjectScope(&config.ApiKey{Name: "key-b", Token: "key-b-token-1234567890"})
	keyA := routing.SessionKeyForSubject(subjectA, "test-agent", "", "hi")
	keyB := routing.SessionKeyForSubject(subjectB, "test-agent", "", "hi")
	if keyA == keyB {
		t.Fatal("session keys must differ per key")
	}
	if _, found := gw.AgentState.SessionRouter.Peek(keyA); !found {
		t.Fatal("key A's request must have pinned the subject-bound session key")
	}
	gw.AgentState.SessionRouter.Pin(keyA, "test-provider-0", "test-model", "acct-a", time.Hour)

	headerB := send("key-b-token-1234567890")
	headerA2 := send("key-a-token-1234567890")

	if headerA1 == "" || headerB == "" {
		t.Fatalf("expected synthesized session headers, got %q / %q", headerA1, headerB)
	}
	if headerA1 != headerA2 {
		t.Fatalf("key A's session header must be stable, got %q then %q", headerA1, headerA2)
	}
	if headerA1 == headerB {
		t.Fatalf("keys must get distinct session headers, both %q", headerA1)
	}

	if pinA, found := gw.AgentState.SessionRouter.Peek(keyA); !found || pinA.Account != "acct-a" {
		t.Fatalf("key A lost its own pin: %+v (found=%v)", pinA, found)
	}
	if pinB, found := gw.AgentState.SessionRouter.Peek(keyB); found && pinB.Account == "acct-a" {
		t.Fatal("key B inherited key A's pinned account")
	}
}

// TestResolveStickyPinIsSubjectScoped is the cross-key regression guard: a pin
// created while serving key A must be invisible to key B presenting the same
// conversation opener (and therefore cannot steer B's credential selection).
func TestResolveStickyPinIsSubjectScoped(t *testing.T) {
	p := newCachedStreamProxy(t, "http://127.0.0.1:1")
	gw := p.Gateway()

	reqA := sessionTestRequest(gw, sessionSubjectScope(&config.ApiKey{Name: "a", Token: "token-a"}))
	pinA := resolveStickyPin(reqA, gw)
	if pinA == nil {
		t.Fatal("expected a sticky-pin context for the owner request")
	}
	gw.AgentState.SessionRouter.Pin(pinA.key, "test-provider-0", "test-model", "acct-a", time.Hour)

	reqB := sessionTestRequest(gw, sessionSubjectScope(&config.ApiKey{Name: "b", Token: "token-b"}))
	pinB := resolveStickyPin(reqB, gw)
	if pinB == nil {
		t.Fatal("expected a sticky-pin context for the second key")
	}
	if pinB.key == pinA.key {
		t.Fatal("session keys must differ per authenticated subject")
	}
	if pinB.state.Account != "" {
		t.Fatalf("key B inherited key A's pinned account %q", pinB.state.Account)
	}

	// The owner still sees its own pin.
	againA := resolveStickyPin(reqA, gw)
	if againA.state.Account != "acct-a" {
		t.Fatalf("owner lost its pin, got account %q", againA.state.Account)
	}
}

// TestOpencodeSessionHeaderIsSubjectScoped pins that the synthesized upstream
// session identifier is derived from the subject-bound key, so two keys with
// the same opener are not conflated into one upstream session.
func TestOpencodeSessionHeaderIsSubjectScoped(t *testing.T) {
	p := newCachedStreamProxy(t, "http://127.0.0.1:1")
	gw := p.Gateway()

	headerFor := func(token string) string {
		req := sessionTestRequest(gw, sessionSubjectScope(&config.ApiKey{Name: token, Token: token}))
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		ensureOpencodeSessionHeader(gw, r, req)
		return r.Header.Get(opencodeSessionHeader)
	}

	headerA, headerB := headerFor("token-a"), headerFor("token-b")
	if headerA == "" || headerB == "" {
		t.Fatalf("expected synthesized session headers, got %q / %q", headerA, headerB)
	}
	if headerA == headerB {
		t.Fatalf("session headers must differ per key, both %q", headerA)
	}
}

// TestAuth_MalformedExpiryDenies pins the fail-closed runtime expiry: an
// unparseable expires_at (which load validation rejects) must refuse the key
// instead of being treated as never expiring.
func TestAuth_MalformedExpiryDenies(t *testing.T) {
	p, _ := newTestProxyWithSecrets(t, &config.SecretsConfig{
		ClientToken:  "",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"bad-expiry": {
				Name:      "bad-expiry",
				Token:     "secret-token-1234567890",
				Roles:     []string{"user"},
				Enabled:   true,
				ExpiresAt: "not-a-timestamp",
			},
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-token-1234567890")
	rec := httptest.NewRecorder()
	if _, ok := p.authenticateAndAuthorize(req, rec); ok {
		t.Fatal("a key with an unparseable expiry must be denied")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", rec.Code)
	}
}

// TestAuth_MissingModelDenies pins that a chat request without a model is
// rejected before routing instead of skipping per-key agent scoping.
func TestAuth_MissingModelDenies(t *testing.T) {
	p, _ := newTestProxyWithSecrets(t, &config.SecretsConfig{
		ClientToken:  "",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"scoped": {
				Name:          "scoped",
				Token:         "secret-token-1234567890",
				Roles:         []string{"user"},
				Enabled:       true,
				AllowedAgents: []string{"test-agent"},
			},
		},
	})

	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions",
		`{"messages":[{"role":"user","content":"hi"}]}`)
	req.Header.Set("Authorization", "Bearer secret-token-1234567890")
	rec := httptest.NewRecorder()
	if _, ok := p.authenticateAndAuthorize(req, rec); ok {
		t.Fatal("a chat request without a model must be denied")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestAuthorizeResponsesAgent_FailClosed covers the /v1/responses scoping
// rules: the create endpoint must name a scopeable model, while id-addressed
// operations (sub-paths, no model) pass through.
func TestAuthorizeResponsesAgent_FailClosed(t *testing.T) {
	p, _ := newTestProxyWithSecrets(t, nil)
	gw := p.Gateway()
	key := &config.ApiKey{Name: "scoped", Roles: []string{"user"}, AllowedAgents: []string{"allowed-agent"}}

	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		wantCode int
	}{
		{"create: authorized model", http.MethodPost, "/v1/responses", `{"model":"allowed-agent","input":"hi"}`, 0},
		{"create: unauthorized model", http.MethodPost, "/v1/responses", `{"model":"other-agent"}`, http.StatusForbidden},
		{"create: missing model", http.MethodPost, "/v1/responses", `{"input":"hi"}`, http.StatusBadRequest},
		{"create: empty model", http.MethodPost, "/v1/responses", `{"model":""}`, http.StatusBadRequest},
		{"create: non-string model", http.MethodPost, "/v1/responses", `{"model":123}`, http.StatusBadRequest},
		{"create: invalid json", http.MethodPost, "/v1/responses", `{"model":`, http.StatusBadRequest},
		{"create: empty body", http.MethodPost, "/v1/responses", ``, http.StatusBadRequest},
		{"create: non-canonical double slash", http.MethodPost, "/v1/responses//", `{"input":"hi"}`, http.StatusBadRequest},
		{"create: non-canonical dot segment", http.MethodPost, "/v1/responses/.", `{"input":"hi"}`, http.StatusBadRequest},
		{"create: encoded dot segment", http.MethodPost, "/v1/responses/%2e", `{"input":"hi"}`, http.StatusBadRequest},
		{"create: doubly encoded dot segment", http.MethodPost, "/v1/responses/%25252e", `{"input":"hi"}`, http.StatusBadRequest},
		{"create: doubly encoded traversal", http.MethodPost, "/v1/responses/%25252e%25252e", `{"input":"hi"}`, http.StatusBadRequest},
		{"id-addressed: no model", http.MethodPost, "/v1/responses/resp_1/cancel", `{}`, 0},
		{"id-addressed: retrieve", http.MethodGet, "/v1/responses/resp_1", ``, 0},
		{"id-addressed: delete", http.MethodDelete, "/v1/responses/resp_1", ``, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			_, herr := p.authorizeResponsesAgent(gw, req, key, []byte(tc.body))
			if tc.wantCode == 0 {
				if herr != nil {
					t.Fatalf("expected pass-through, got %v", herr)
				}
				return
			}
			if herr == nil {
				t.Fatalf("expected rejection with %d, got none", tc.wantCode)
			}
			if herr.Code != tc.wantCode {
				t.Fatalf("expected %d, got %d (%s)", tc.wantCode, herr.Code, herr.Message)
			}
		})
	}

	// A nil key cannot be scoped: deny rather than forward.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if _, herr := p.authorizeResponsesAgent(gw, req, nil, []byte(`{"model":"allowed-agent"}`)); herr == nil || herr.Code != http.StatusForbidden {
		t.Fatalf("nil key must be denied, got %v", herr)
	}

	// Every fail-closed denial is observable (metric + reason).
	var buf bytes.Buffer
	gw.Metrics.WritePrometheus(&buf)
	rendered := buf.String()
	for reason, keyName := range map[string]string{
		"missing_model": "scoped",
		"invalid_body":  "scoped",
		"invalid_key":   "unauthenticated",
	} {
		// The labeled series must exist (any count: the table drives several
		// denials of each shape).
		want := `nenya_auth_denials_total{key_name="` + keyName + `", reason="` + reason + `"} `
		if !strings.Contains(rendered, want) {
			t.Errorf("expected %q in the metrics exposition, got:\n%s", want, rendered)
		}
	}
}
