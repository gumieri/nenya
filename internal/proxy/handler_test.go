package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/testutil"
)

func newTestProxyWithSecrets(t *testing.T, secrets *config.SecretsConfig) (*Proxy, *httptest.Server) {
	t.Helper()
	cfg := testutil.MinimalConfig()
	cfg.Bouncer.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	if secrets == nil {
		secrets = &config.SecretsConfig{
			ClientToken:  "test-token",
			ProviderKeys: map[string]string{"gemini": "test-key"},
		}
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)
	return p, nil
}

func newTestProxy(t *testing.T) (*Proxy, *httptest.Server) {
	return newTestProxyWithSecrets(t, nil)
}

func TestServeHTTP_Healthz_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status ok, got %v", body["status"])
	}
}

func TestServeHTTP_Statsz_PublicAccess(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/statsz", nil)
	req.Header.Set("Authorization", "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
}

func TestServeHTTP_Statsz_WithAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/statsz", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
}

func TestServeHTTP_ChatCompletions_WrongMethod(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusMethodNotAllowed)
}

func TestServeHTTP_Embeddings_WrongMethod(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/embeddings", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusMethodNotAllowed)
}

func TestServeHTTP_Metrics_WrongMethod(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodPost, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusMethodNotAllowed)
}

func TestServeHTTP_Models_ValidAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	if body["object"] != "list" {
		t.Errorf("expected object=list, got %v", body["object"])
	}
}

func TestServeHTTP_Models_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusUnauthorized)
}

func TestServeHTTP_Models_WrongToken(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusForbidden)
}

func TestServeHTTP_ChatCompletions_NoAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusUnauthorized)
}

func TestServeHTTP_ChatCompletions_WrongToken(t *testing.T) {
	p, _ := newTestProxy(t)
	body := `{"model":"gemini-2.5-flash","messages":[{"role":"user","content":"hi"}]}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusForbidden)
}

func TestServeHTTP_UnknownPath(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/unknown", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusNotFound)
}

func TestServeHTTP_Models_WrongMethod(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusMethodNotAllowed)
}

func TestServeHTTP_Metrics_ValidAuth(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
}

// Test authenticateRequest: missing Authorization header
func TestAuthenticateRequest_MissingHeader(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	// No Authorization header
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail, got token=%q", token)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: malformed Authorization header (wrong prefix)
func TestAuthenticateRequest_WrongPrefix(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Basic wrong-token")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail, got token=%q", token)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: empty Bearer token
func TestAuthenticateRequest_EmptyBearer(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer ")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail, got token=%q", token)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: whitespace-only Bearer token
func TestAuthenticateRequest_WhitespaceBearer(t *testing.T) {
	p, _ := newTestProxy(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer   ")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail, got token=%q", token)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: client token mismatch via secure memory
func TestAuthenticateRequest_ClientTokenMismatch(t *testing.T) {
	p, _ := newTestProxyWithSecrets(t, &config.SecretsConfig{
		ClientToken:  "correct-token",
		ProviderKeys: map[string]string{},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail, got token=%q", token)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: expired API key
func TestAuthenticateRequest_ExpiredKey(t *testing.T) {
	expired := time.Now().Add(-time.Hour).Format(time.RFC3339)
	p, _ := newTestProxyWithSecrets(t, &config.SecretsConfig{
		ClientToken:  "",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"expired-key": {
				Name:      "expired-key",
				Token:     "secret-token",
				ExpiresAt: expired,
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail (expired), got token=%q", token)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: disabled API key
func TestAuthenticateRequest_DisabledKey(t *testing.T) {
	p, _ := newTestProxyWithSecrets(t, &config.SecretsConfig{
		ClientToken:  "",
		ProviderKeys: map[string]string{},
		ApiKeys: map[string]config.ApiKey{
			"disabled-key": {
				Name:    "disabled-key",
				Token:   "secret-token",
				Enabled: false,
			},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if ok {
		t.Fatalf("expected authentication to fail (disabled), got token=%q", token)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("expected status 403, got %d", rec.Code)
	}
	if token != "" {
		t.Fatalf("expected empty token, got %q", token)
	}
}

// Test authenticateRequest: valid token succeeds
func TestAuthenticateRequest_ValidToken(t *testing.T) {
	p, _ := newTestProxyWithSecrets(t, &config.SecretsConfig{
		ClientToken:  "valid-token",
		ProviderKeys: map[string]string{},
	})
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	token, ok := p.authenticateRequest(req, rec)
	if !ok {
		t.Fatalf("expected authentication to succeed, got status %d", rec.Code)
	}
	if token != "primary" {
		t.Fatalf("expected token reference 'primary', got %q", token)
	}
}

// TestAuthenticateRequest_MessagesAgentScoping pins NENYA-52: per-key agent
// scoping must be enforced on /v1/messages exactly like on
// /v1/chat/completions — a key scoped to agent-a cannot reach agent-b by
// switching wire formats.
func TestAuthenticateRequest_MessagesAgentScoping(t *testing.T) {
	secrets := &config.SecretsConfig{
		ClientToken: "valid-token",
		ApiKeys: map[string]config.ApiKey{
			"scoped-a": {Name: "scoped-a", Token: "scoped-token-agent-a", Roles: []string{"user"}, AllowedAgents: []string{"agent-a"}, Enabled: true},
			"unscoped": {Name: "unscoped", Token: "unscoped-token-1234", Roles: []string{"user"}, Enabled: true},
			"admin":    {Name: "admin", Token: "admin-token-123456", Roles: []string{"admin"}, AllowedAgents: []string{"agent-a"}, Enabled: true},
		},
		ProviderKeys: map[string]string{},
	}
	p, _ := newTestProxyWithSecrets(t, secrets)

	newMessagesReq := func(model string) *http.Request {
		body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, model)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return req
	}

	t.Run("scoped key denied on other agent via /v1/messages", func(t *testing.T) {
		req := newMessagesReq("agent-b")
		req.Header.Set("Authorization", "Bearer scoped-token-agent-a")
		rec := httptest.NewRecorder()
		_, ok := p.authenticateRequest(req, rec)
		if ok {
			t.Fatalf("expected authz rejection for agent-b via /v1/messages, got status %d", rec.Code)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", rec.Code)
		}
	})

	t.Run("scoped key allowed on own agent via /v1/messages", func(t *testing.T) {
		req := newMessagesReq("agent-a")
		req.Header.Set("Authorization", "Bearer scoped-token-agent-a")
		rec := httptest.NewRecorder()
		key, ok := p.authenticateRequest(req, rec)
		if !ok {
			t.Fatalf("expected authz pass for agent-a via /v1/messages, got status %d", rec.Code)
		}
		if key != "scoped-a" {
			t.Fatalf("expected authenticated key 'scoped-a', got %q", key)
		}
	})

	t.Run("unscoped key allowed on any agent via /v1/messages", func(t *testing.T) {
		req := newMessagesReq("agent-b")
		req.Header.Set("Authorization", "Bearer unscoped-token-1234")
		rec := httptest.NewRecorder()
		if _, ok := p.authenticateRequest(req, rec); !ok {
			t.Fatalf("expected authz pass for unscoped key, got status %d", rec.Code)
		}
	})

	t.Run("admin key bypasses scoping on /v1/messages", func(t *testing.T) {
		req := newMessagesReq("agent-b")
		req.Header.Set("Authorization", "Bearer admin-token-123456")
		rec := httptest.NewRecorder()
		if _, ok := p.authenticateRequest(req, rec); !ok {
			t.Fatalf("expected authz pass for admin key, got status %d", rec.Code)
		}
	})

	t.Run("invalid JSON body on chat route fails closed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("{invalid"))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer scoped-token-agent-a")
		rec := httptest.NewRecorder()
		if _, ok := p.authenticateRequest(req, rec); ok {
			t.Fatalf("expected rejection for invalid JSON body, got status %d", rec.Code)
		}
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d", rec.Code)
		}
		assertErrorKind(t, rec, "invalid_request")
	})

	t.Run("oversized body on chat route returns 413", func(t *testing.T) {
		cfg := testutil.MinimalConfig()
		cfg.Bouncer.Engine = config.EngineRef{Provider: "ollama", Model: "qwen2.5-coder"}
		cfg.Server.MaxBodyBytes = 64
		gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
		p2 := &Proxy{}
		p2.StoreGateway(gw)

		oversized := `{"model":"agent-b","messages":[{"role":"user","content":"` + strings.Repeat("x", 200) + `"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer scoped-token-agent-a")
		rec := httptest.NewRecorder()
		if _, ok := p2.authenticateRequest(req, rec); ok {
			t.Fatalf("expected rejection for oversized body, got status %d", rec.Code)
		}
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %d", rec.Code)
		}
		assertErrorKind(t, rec, "payload_too_large")
	})
}

// assertErrorKind checks the error_kind field of a structured error response.
// Handles both envelope shapes: top-level error_kind (writeStructuredError)
// and nested error.error_kind (writeGatewayError's OpenAI envelope).
func assertErrorKind(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var body struct {
		ErrorKind string `json:"error_kind"`
		Error     struct {
			ErrorKind string `json:"error_kind"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON error body, got error: %v, body: %s", err, rec.Body.String())
	}
	kind := body.ErrorKind
	if kind == "" {
		kind = body.Error.ErrorKind
	}
	if kind != want {
		t.Fatalf("expected error_kind %q, got %q (body: %s)", want, kind, rec.Body.String())
	}
}

func TestServeHTTP_Models_NoDeepSeekWithoutAPIKey(t *testing.T) {
	p, _ := newTestProxy(t)
	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v, body: %s", err, rec.Body.String())
	}
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("expected data array, got body: %+v", body)
	}
	for _, m := range data {
		entry, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		ownedBy, _ := entry["owned_by"].(string)
		if strings.Contains(strings.ToLower(id), "deepseek") || strings.Contains(strings.ToLower(ownedBy), "deepseek") {
			t.Errorf("deepseek model should not appear without API key: id=%q ownedBy=%q", id, ownedBy)
		}
	}
}

func TestCheckOllamaProviderHealth_RetryOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	defer server.Close()

	cfg := testutil.MinimalConfig()
	cfg.Bouncer.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	p := &Proxy{}
	p.StoreGateway(gw)

	ctx := context.Background()
	result := p.checkOllamaProviderHealth(ctx, gw, server.URL)
	if !result {
		t.Errorf("expected true after retry, got false")
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestCheckOllamaProviderHealth_AllFailed(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := testutil.MinimalConfig()
	cfg.Bouncer.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	p := &Proxy{}
	p.StoreGateway(gw)

	ctx := context.Background()
	result := p.checkOllamaProviderHealth(ctx, gw, server.URL)
	if result {
		t.Errorf("expected false when all attempts fail, got true")
	}
	if attempts.Load() != 2 {
		t.Errorf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestCheckOllamaProviderHealth_ContextDeadline(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		time.Sleep(500 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"models": []interface{}{}})
	}))
	defer server.Close()

	cfg := testutil.MinimalConfig()
	cfg.Bouncer.Engine = config.EngineRef{
		Provider: "ollama",
		Model:    "qwen2.5-coder",
	}
	secrets := &config.SecretsConfig{
		ClientToken: "test-token",
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())

	p := &Proxy{}
	p.StoreGateway(gw)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	result := p.checkOllamaProviderHealth(ctx, gw, server.URL)
	if result {
		t.Errorf("expected false due to context timeout, got true")
	}
	if attempts.Load() > 2 {
		t.Errorf("expected at most 2 attempts due to timeout, got %d", attempts.Load())
	}
}

func TestServeHTTP_Models_AgentMetadata(t *testing.T) {
	cfg := testutil.TestConfig(
		testutil.WithAgent("coder", config.AgentConfig{
			Models: []config.AgentModel{
				{Provider: "deepseek", Model: "deepseek-v4-flash"},
				{Provider: "gemini", Model: "gemini-2.5-flash"},
			},
			Strategy: "fallback",
		}),
	)
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"gemini": "test-key", "deepseek": "test-key"},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("expected data array, got %T", body["data"])
	}

	var agentEntry map[string]interface{}
	for _, item := range data {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if entry["id"] == "coder" {
			agentEntry = entry
			break
		}
	}
	if agentEntry == nil {
		t.Fatal("agent 'coder' not found in models list")
	}

	if agentEntry["owned_by"] != "nenya" {
		t.Errorf("expected owned_by=nenya, got %v", agentEntry["owned_by"])
	}
	if ctx, ok := agentEntry["context_window"].(float64); !ok || int(ctx) != 1048576 {
		t.Errorf("expected context_window=1048576 (max of 1000000 and 1048576), got %v", agentEntry["context_window"])
	}
	if maxTok, ok := agentEntry["max_tokens"].(float64); !ok || int(maxTok) != 384000 {
		t.Errorf("expected max_tokens=384000 (max of 384000 and 65536), got %v", agentEntry["max_tokens"])
	}
	if strategy, ok := agentEntry["routing_strategy"].(string); !ok || strategy != "fallback" {
		t.Errorf("expected routing_strategy=fallback, got %v", agentEntry["routing_strategy"])
	}

	for _, item := range data {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if entry["id"] == "coder" {
			continue
		}
		if _, hasID := entry["id"]; !hasID {
			continue
		}
		if _, ok := entry["routing_strategy"]; ok {
			t.Errorf("catalog model %v should not have routing_strategy", entry["id"])
		}
	}
}

func TestServeHTTP_Models_AgentDescription(t *testing.T) {
	cfg := testutil.TestConfig(
		testutil.WithAgent("with-desc", config.AgentConfig{
			Models:      []config.AgentModel{{Provider: "deepseek", Model: "deepseek-v4-flash"}},
			Strategy:    "fallback",
			Description: "Test agent with description",
		}),
	)
	secrets := &config.SecretsConfig{
		ClientToken:  "test-token",
		ProviderKeys: map[string]string{"deepseek": "test-key"},
	}
	gw := gateway.New(context.Background(), *cfg, secrets, slog.Default())
	p := &Proxy{}
	p.StoreGateway(gw)

	req := testutil.NewTestRequest(t, http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)

	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("expected JSON body, got error: %v", err)
	}
	data, ok := body["data"].([]interface{})
	if !ok {
		t.Fatalf("expected data array, got %T", body["data"])
	}

	var agentEntry map[string]interface{}
	found := false
	for _, item := range data {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if entry["id"] == "with-desc" {
			agentEntry = entry
			found = true
			break
		}
	}
	if !found {
		t.Fatal("agent 'with-desc' not found in models list")
	}

	if desc, ok := agentEntry["description"].(string); !ok || desc != "Test agent with description" {
		t.Errorf("expected description='Test agent with description', got %v", agentEntry["description"])
	}
}

func TestServeHTTP_DrainRejectsNewRequests(t *testing.T) {
	p := &Proxy{ShutdownCtx: context.Background()}
	p.Shutdown.Store(true)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 during drain, got %d", rec.Code)
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Error("expected Retry-After header during drain")
	}
	var body struct {
		Kind string `json:"error_kind"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not structured JSON: %v", err)
	}
	if body.Kind != string(infra.ErrorKindInternal) {
		t.Errorf("expected error_kind internal_error, got %q", body.Kind)
	}
}

func TestProxy_WaitAutoSave_ReturnsImmediately(t *testing.T) {
	p := &Proxy{ShutdownCtx: context.Background()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p.WaitAutoSave(ctx) // no goroutines tracked: must not block
}
