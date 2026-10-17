package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/testutil"
)

func TestSanitizeIdempotencyKey(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
		ok   bool
	}{
		{name: "plain", raw: "abc-123_XYZ", want: "abc-123_XYZ", ok: true},
		{name: "trimmed", raw: "  key-1  ", want: "key-1", ok: true},
		{name: "empty", raw: ""},
		{name: "whitespace only", raw: "   "},
		{name: "newline", raw: "key\ninjected: 1"},
		{name: "control character", raw: "key\x00"},
		{name: "non ascii", raw: "chave-é"},
		{name: "over the length cap", raw: strings.Repeat("k", maxIdempotencyKeyLen+1)},
		{name: "at the length cap", raw: strings.Repeat("k", maxIdempotencyKeyLen), want: strings.Repeat("k", maxIdempotencyKeyLen), ok: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := sanitizeIdempotencyKey(tt.raw)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("sanitizeIdempotencyKey(%q) = (%q, %v), want (%q, %v)", tt.raw, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestIdempotencyKeyForBody(t *testing.T) {
	body := []byte(`{"model":"m"}`)
	other := []byte(`{"model":"m","messages":[{"role":"tool","content":"x"}]}`)

	t.Run("the client key is bound to the first body", func(t *testing.T) {
		h := http.Header{}
		h.Set(idempotencyKeyHeader, "  client-key-1 ")
		key, ok := idempotencyKeyForBody(h, body)
		if !ok || key != "client-key-1" {
			t.Fatalf("key = %q (%v), want the trimmed client key", key, ok)
		}
		// The same body re-uses the same key (retry-in-place, account
		// failover).
		again, ok := idempotencyKeyForBody(h, body)
		if !ok || again != "client-key-1" {
			t.Fatalf("repeat key = %q (%v), want the bound key", again, ok)
		}
	})

	t.Run("a modified body is re-keyed", func(t *testing.T) {
		h := http.Header{}
		h.Set(idempotencyKeyHeader, "client-key-1")
		first, _ := idempotencyKeyForBody(h, body)
		second, ok := idempotencyKeyForBody(h, other)
		if !ok {
			t.Fatal("expected a key for the modified body")
		}
		if second == first {
			t.Fatalf("a modified body must not reuse the key %q (a provider rejects a repeated key with a different payload)", first)
		}
		// ...and the NEW body is then bound to its own key.
		again, _ := idempotencyKeyForBody(h, other)
		if again != second {
			t.Fatalf("the modified body's key changed between attempts: %q -> %q", second, again)
		}
	})

	t.Run("synthesizes when the client key is unusable", func(t *testing.T) {
		h := http.Header{}
		h.Set(idempotencyKeyHeader, "bad\nkey")
		key, ok := idempotencyKeyForBody(h, body)
		if !ok || !strings.HasPrefix(key, idempotencyKeyPrefix) {
			t.Fatalf("key = %q (%v), want a synthesized key", key, ok)
		}
		if _, ok := sanitizeIdempotencyKey(key); !ok {
			t.Fatalf("the synthesized key must be transport-safe: %q", key)
		}
	})

	t.Run("synthesizes when the client sent none", func(t *testing.T) {
		h := http.Header{}
		key, ok := idempotencyKeyForBody(h, body)
		if !ok || !strings.HasPrefix(key, idempotencyKeyPrefix) {
			t.Fatalf("key = %q (%v), want a synthesized key", key, ok)
		}
	})

	t.Run("two logical requests do not share a synthesized key", func(t *testing.T) {
		k1, _ := idempotencyKeyForBody(http.Header{}, body)
		k2, _ := idempotencyKeyForBody(http.Header{}, body)
		if k1 == k2 {
			t.Fatal("two logical requests must not share a synthesized key")
		}
	})

	t.Run("an empty header map yields no key", func(t *testing.T) {
		if _, ok := idempotencyKeyForBody(nil, body); ok {
			t.Fatal("expected no key without a header map")
		}
	})
}

// TestResetIdempotencyBinding pins the admission guard: a client cannot pin
// its key to a body of its choosing by sending the internal binding header.
func TestResetIdempotencyBinding(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(idempotencyBindingHeader, "forged")
	resetIdempotencyBinding(r)
	if got := r.Header.Get(idempotencyBindingHeader); got != "" {
		t.Fatalf("binding = %q, want it cleared", got)
	}
	// The client's key itself survives (it is sanitized, not dropped).
	r.Header.Set(idempotencyKeyHeader, "client-key-1")
	resetIdempotencyBinding(r)
	if got := r.Header.Get(idempotencyKeyHeader); got != "client-key-1" {
		t.Fatalf("key = %q, want the client key preserved", got)
	}
}

func TestInjectIdempotencyKey(t *testing.T) {
	newGW := func(header string) *gateway.NenyaGateway {
		return &gateway.NenyaGateway{
			Logger:    testLog(t),
			Providers: map[string]*config.Provider{"p": {IdempotencyHeader: header}},
		}
	}
	src := http.Header{}
	src.Set(idempotencyKeyHeader, "client-key-1")

	body := []byte(`{"model":"m"}`)

	t.Run("declared provider receives the key", func(t *testing.T) {
		dst := http.Header{}
		injectIdempotencyKey(newGW("Idempotency-Key"), "p", src, dst, body)
		if got := dst.Get("Idempotency-Key"); got != "client-key-1" {
			t.Fatalf("outbound key = %q, want the client key", got)
		}
	})

	t.Run("undeclared provider receives nothing", func(t *testing.T) {
		dst := http.Header{}
		injectIdempotencyKey(newGW(""), "p", src, dst, body)
		if got := dst.Get(idempotencyKeyHeader); got != "" {
			t.Fatalf("outbound key = %q, want none (provider opted out)", got)
		}
	})

	t.Run("an unusable client key is replaced, never mangled", func(t *testing.T) {
		dst := http.Header{}
		bad := http.Header{}
		bad.Set(idempotencyKeyHeader, "bad\x01key")
		injectIdempotencyKey(newGW("Idempotency-Key"), "p", bad, dst, body)
		got := dst.Get("Idempotency-Key")
		if got == "bad\x01key" || strings.ContainsRune(got, '\x01') {
			t.Fatalf("outbound key = %q, the malformed client value must never be forwarded", got)
		}
		if !strings.HasPrefix(got, idempotencyKeyPrefix) {
			t.Fatalf("outbound key = %q, want a synthesized replacement", got)
		}
	})
}

// TestBuildUpstreamRequest_ForwardsIdempotencyKey pins the dispatch behavior:
// the header reaches the outbound request under the provider's declared name,
// and only there.
func TestBuildUpstreamRequest_ForwardsIdempotencyKey(t *testing.T) {
	gw := &gateway.NenyaGateway{
		Logger: testLog(t),
		Config: config.Config{Server: config.ServerConfig{UserAgent: "nenya-test"}},
		Providers: map[string]*config.Provider{
			"declared": {AuthStyle: config.AuthStyleNone, IdempotencyHeader: "Idempotency-Key"},
			"plain":    {AuthStyle: config.AuthStyleNone},
		},
	}
	p := &Proxy{}
	src := http.Header{}
	src.Set(idempotencyKeyHeader, "client-key-1")

	body := []byte(`{"model":"m"}`)
	build := func(t *testing.T, provider string, body []byte) *http.Request {
		t.Helper()
		req, err := p.buildUpstreamRequest(gw, context.Background(), http.MethodPost,
			"https://up.test/v1/chat/completions", body, provider, "m", "", src)
		if err != nil {
			t.Fatal(err)
		}
		return req
	}

	first := build(t, "declared", body)
	if got := first.Header.Get("Idempotency-Key"); got != "client-key-1" {
		t.Fatalf("declared provider key = %q, want the client key", got)
	}
	// An identical re-dispatch (retry-in-place, account failover) keeps it.
	if got := build(t, "declared", body).Header.Get("Idempotency-Key"); got != "client-key-1" {
		t.Fatalf("repeated dispatch key = %q, want the bound client key", got)
	}
	// A modified body is re-keyed (MCP iteration, continuation, corrective
	// retry): a provider rejects a repeated key whose payload differs.
	modified := build(t, "declared", []byte(`{"model":"m","messages":[{"role":"tool","content":"x"}]}`))
	if got := modified.Header.Get("Idempotency-Key"); got == "client-key-1" || got == "" {
		t.Fatalf("modified body key = %q, want a fresh non-empty key", got)
	}

	plain := build(t, "plain", body)
	if got := plain.Header.Get(idempotencyKeyHeader); got != "" {
		t.Fatalf("plain provider key = %q, want none", got)
	}
}

// newIdempotencyProxy builds a chainSize-deep single-provider agent whose
// provider declares the idempotency header, so a retry dispatches again. It is
// deliberately self-contained rather than an options knob on newChatProxy: the
// shared helper exposes neither the provider options nor the chain depth this
// feature's tests need, and threading them through would change its call
// sites.
func newIdempotencyProxy(t *testing.T, upstreamURL string, chainSize int) *Proxy {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"idem-provider": {
			URL:               upstreamURL + "/v1/chat/completions",
			AuthStyle:         "none",
			IdempotencyHeader: config.PtrTo("Idempotency-Key"),
		},
	}
	models := make([]config.AgentModel, 0, chainSize)
	for i := 0; i < chainSize; i++ {
		models = append(models, config.AgentModel{Provider: "idem-provider", Model: "test-model"})
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {Strategy: "fallback", Models: models},
	}
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "test-token"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)
	return p
}

// TestRetryCarriesIdenticalIdempotencyKey pins NENYA-165's core promise: a
// gateway-initiated retry of the same logical request presents the SAME key, so
// a provider that dedups on it cannot bill the retried attempt twice.
func TestRetryCarriesIdenticalIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	p := newIdempotencyProxy(t, upstream.URL, 2)
	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the retry to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) < 2 {
		t.Fatalf("expected at least 2 upstream attempts, got %d (keys %v)", len(keys), keys)
	}
	for i, key := range keys {
		if key == "" {
			t.Fatalf("attempt %d carried no idempotency key", i)
		}
		if key != keys[0] {
			t.Fatalf("attempt %d carried key %q, want the first attempt's %q", i, key, keys[0])
		}
	}
}

// TestIdempotencyKeyAbsentIsNotForwarded pins the negative: with no
// idempotency-capable provider the request gains no header at all.
func TestIdempotencyKeyAbsentIsNotForwarded(t *testing.T) {
	var got atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		got.Store(r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	// newChatProxy's test-provider declares no idempotency header.
	p := newChatProxy(t, upstream.URL)
	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	raw := got.Load()
	if raw == nil {
		t.Fatal("the upstream handler never ran: the assertion would be vacuous")
	}
	if key, _ := raw.(string); key != "" {
		t.Fatalf("a provider without the capability received %q", key)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
}

// postChatJSONWithHeaders posts a chat body with extra client headers.
func postChatJSONWithHeaders(t *testing.T, p *Proxy, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer test-token")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestClientIdempotencyKeyReachesUpstream pins requirement (2): a client key
// is forwarded verbatim to a provider that declares the capability.
func TestClientIdempotencyKeyReachesUpstream(t *testing.T) {
	var got atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		got.Store(r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	p := newIdempotencyProxy(t, upstream.URL, 1)
	rec := postChatJSONWithHeaders(t, p,
		`{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
		map[string]string{"Idempotency-Key": "  client-key-42  "})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if key, _ := got.Load().(string); key != "client-key-42" {
		t.Fatalf("upstream saw key %q, want the trimmed client key", key)
	}
}

// TestCorrectiveRetryReKeysModifiedBody pins the body binding end to end: a
// corrective retry that strips a rejected parameter dispatches a DIFFERENT
// body, so it carries a different key (a provider rejects a key reused with a
// mismatched payload) — while a retry of an identical body keeps its key
// (TestRetryCarriesIdenticalIdempotencyKey).
func TestCorrectiveRetryReKeysModifiedBody(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"Unrecognized request argument supplied: temperature"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Governance.AutoRetryOnParamReject = config.PtrTo(true)
	cfg.Providers = map[string]config.ProviderConfig{
		"param-provider": {
			URL:               up.URL + "/v1/chat/completions",
			AuthStyle:         "none",
			IdempotencyHeader: config.PtrTo("Idempotency-Key"),
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			// The corrective retry consumes the next sweep slot (the same
			// shape as the context-limit retry), so the chain carries the
			// entry twice.
			Models: []config.AgentModel{
				{Provider: "param-provider", Model: "test-model"},
				{Provider: "param-provider", Model: "test-model"},
			},
		},
	}
	gw := gateway.New(context.Background(), *cfg,
		&config.SecretsConfig{ClientToken: "test-token"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the corrective retry to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("expected 2 attempts, got %d (%v)", len(keys), keys)
	}
	for i, key := range keys {
		if key == "" {
			t.Fatalf("attempt %d carried no idempotency key", i)
		}
	}
	if keys[0] == keys[1] {
		t.Fatalf("the corrective retry dispatched a modified body but reused key %q", keys[0])
	}
}

// TestAccountFailoverKeepsIdempotencyKey pins requirement (3): a failover
// between two accounts of the same provider re-sends the same body, so it
// presents the SAME key.
func TestAccountFailoverKeepsIdempotencyKey(t *testing.T) {
	var mu sync.Mutex
	var keys []string
	var auths []string
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer up.Close()

	cfg := testutil.MinimalConfig()
	cfg.Server.MaxBodyBytes = 10 << 20
	cfg.Governance.RatelimitMaxRPM = config.PtrTo(600)
	cfg.Governance.RatelimitMaxTPM = config.PtrTo(10000000)
	cfg.Bouncer.Enabled = config.PtrTo(false)
	cfg.Providers = map[string]config.ProviderConfig{
		"acct-provider": {
			URL:               up.URL + "/v1/chat/completions",
			AuthStyle:         "bearer",
			IdempotencyHeader: config.PtrTo("Idempotency-Key"),
			Accounts: []config.AccountConfig{
				{ID: "a1", Credential: "cred-one"},
				{ID: "a2", Credential: "cred-two"},
			},
		},
	}
	cfg.Agents = map[string]config.AgentConfig{
		"test-agent": {
			Strategy: "fallback",
			Models: []config.AgentModel{
				{Provider: "acct-provider", Model: "test-model"},
				{Provider: "acct-provider", Model: "test-model"},
			},
		},
	}
	// The account credentials ride the pool; the provider key keeps the
	// provider "available" to the target builder (the account pool supplies
	// the dispatch credential).
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"acct-provider": "placeholder"},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := &Proxy{}
	p.StoreGateway(gw)

	rec := postChatJSON(t, p, `{"model":"test-agent","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected the failover to succeed, got %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(keys) < 2 {
		t.Fatalf("expected 2 attempts, got %d (%v)", len(keys), keys)
	}
	if keys[0] == "" {
		t.Fatal("the first attempt carried no idempotency key")
	}
	for i, key := range keys {
		if key != keys[0] {
			t.Fatalf("attempt %d (account %s) carried key %q, want the first attempt's %q", i, auths[i], key, keys[0])
		}
	}
	if auths[0] == auths[1] {
		t.Fatalf("expected the credential to rotate, both attempts used %q", auths[0])
	}
}
