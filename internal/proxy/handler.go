package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/util"
)

// MaxModelNameLength is the maximum allowed length for model names.
const MaxModelNameLength = 256

// fallbackAuthBodyLimit bounds the auth-time body read on chat routes when
// the server configuration carries no MaxBodyBytes.
const fallbackAuthBodyLimit = 10 << 20

// Proxy handles HTTP requests and routes them to upstream AI providers.
type Proxy struct {
	gw                 atomic.Pointer[gateway.NenyaGateway]
	ShutdownCtx        context.Context
	Shutdown           atomic.Bool
	lastQuotaExhausted atomic.Bool
	// autoSaveWG tracks fire-and-forget MCP auto-save goroutines so the
	// drain sequence can wait (bounded) for a mid-CallTool save to finish
	// instead of killing it at process exit.
	autoSaveWG sync.WaitGroup
}

// StoreGateway sets the gateway instance for the proxy.
func (p *Proxy) StoreGateway(gw *gateway.NenyaGateway) {
	p.gw.Store(gw)
}

// Gateway returns the current gateway instance.
func (p *Proxy) Gateway() *gateway.NenyaGateway {
	return p.gw.Load()
}

// WaitAutoSave blocks until all in-flight MCP auto-save goroutines have
// finished or the context is done, whichever comes first. Called during
// drain so shutdown cannot kill an auto-save mid-CallTool.
func (p *Proxy) WaitAutoSave(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		p.autoSaveWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-ctx.Done():
	}
}

// ServeHTTP handles incoming HTTP requests and routes them to appropriate handlers.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer p.recoverPanic(w)

	if p.Shutdown.Load() {
		// Drain window: the signal arrived and the listener is closing,
		// but in-flight requests are still completing. Reject anything
		// new instead of accepting work the process cannot finish.
		w.Header().Set("Retry-After", "5")
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Server is shutting down")
		return
	}

	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return
	}

	// DNS-rebinding hardening (NENYA-34): browser-context requests are
	// refused unless explicitly allowlisted, on every route including the
	// no-auth GET surfaces.
	if p.enforceBrowserGuard(gw, w, r) {
		return
	}

	if handler := p.resolveRoute(r.URL.Path); handler != nil {
		handler(gw, w, r)
		return
	}
	writeStructuredError(w, http.StatusNotFound, infra.ErrorKindInvalidRequest, "Not Found")
}

type routeHandler func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request)

func (p *Proxy) recoverPanic(w http.ResponseWriter) {
	if rec := recover(); rec != nil {
		if gw := p.Gateway(); gw != nil {
			gw.Logger.Error("panic recovered", "err", rec)
			gw.Metrics.RecordPanic()
		}
		writeStructuredError(w, http.StatusInternalServerError, infra.ErrorKindInternal, "Internal Server Error")
	}
}

func (p *Proxy) resolveRoute(path string) routeHandler {
	type entry struct {
		prefix  bool
		pattern string
		handler func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request)
	}

	handlers := []entry{
		{false, "/healthz", p.chainHealthz},
		{false, "/statsz", p.chainStats},
		{false, "/metrics", p.chainMetrics},
		{true, "/debug/pprof", p.chainAuthPprof},
		{false, "/v1/models", p.chainModels},
		{false, "/v1/chat/completions", p.chainChat},
		{false, "/v1/messages", p.chainChat},
		{false, "/v1/systemone", p.chainSystemOne},
		{false, "/v1/embeddings", p.chainEmbeddings},
		{true, "/v1/responses", p.chainResponses},
		{true, "/proxy/", p.chainProxy},
		{true, "/v1/files", p.chainFiles},
		{true, "/v1/batches", p.chainBatches},
		{true, "/v1/images/generations", p.chainImages},
		{true, "/v1/audio/transcriptions", p.chainAudioTranscriptions},
		{true, "/v1/audio/speech", p.chainAudioSpeech},
		{true, "/v1/moderations", p.chainModerations},
		{true, "/v1/rerank", p.chainRerank},
		{true, "/v1/a2a", p.chainA2A},
	}

	for _, e := range handlers {
		if e.prefix {
			if strings.HasPrefix(path, e.pattern) {
				return e.handler
			}
		} else if e.pattern == path {
			return e.handler
		}
	}
	return nil
}

// chainEndpoint wraps a handler with optional method validation, authentication,
// and metrics observation. This eliminates repetition across all chain methods.
func (p *Proxy) chainEndpoint(method, path string, requireAuth bool, handler func(*gateway.NenyaGateway, http.ResponseWriter, *http.Request, *config.ApiKey)) func(*gateway.NenyaGateway, http.ResponseWriter, *http.Request) {
	return func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
		if method != "" && r.Method != method {
			writeStructuredError(w, http.StatusMethodNotAllowed, infra.ErrorKindInvalidRequest, "Method Not Allowed")
			return
		}
		var apiKey *config.ApiKey
		var ok bool
		if requireAuth {
			apiKey, ok = p.authenticateAndAuthorize(r, w)
			if !ok {
				return
			}
		}
		infra.ObserveHTTPFunc(gw.Metrics, func(w http.ResponseWriter, r *http.Request) {
			handler(gw, w, r, apiKey)
		})(w, r)
	}
}

func (p *Proxy) chainHealthz(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodGet, "/healthz", false, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleHealthz(w, r)
	})(gw, w, r)
}

func (p *Proxy) chainStats(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodGet, "/statsz", false, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		infra.ObserveHTTP(gw.Metrics, p.handleStats)(w, r)
	})(gw, w, r)
}

func (p *Proxy) chainMetrics(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodGet, "/metrics", false, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleMetrics(w, r)
	})(gw, w, r)
}

func (p *Proxy) chainAuthPprof(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/debug/pprof", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handlePprof(w, r)
	})(gw, w, r)
}

func (p *Proxy) chainModels(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodGet, "/v1/models", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		infra.ObserveHTTP(gw.Metrics, p.handleModels)(w, r)
	})(gw, w, r)
}

func (p *Proxy) chainChat(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/chat/completions", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleChatCompletions(gw, w, r, apiKey)
	})(gw, w, r)
}

func (p *Proxy) chainEmbeddings(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/embeddings", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleEmbeddings(gw, w, r, apiKey)
	})(gw, w, r)
}

func (p *Proxy) chainSystemOne(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/systemone", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleSystemOne(gw, w, r, apiKey)
	})(gw, w, r)
}

func (p *Proxy) chainResponses(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/v1/responses", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleResponses(gw, w, r, apiKey)
	})(gw, w, r)
}

func (p *Proxy) chainProxy(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/proxy/", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handlePassthrough(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainFiles(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/v1/files", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleFiles(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainBatches(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/v1/batches", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleBatches(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainImages(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/images/generations", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleImages(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainAudioTranscriptions(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/audio/transcriptions", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleAudioTranscriptions(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainAudioSpeech(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/audio/speech", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleAudioSpeech(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainModerations(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/moderations", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleModerations(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainRerank(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/rerank", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleRerank(gw, w, r, keyRef)
	})(gw, w, r)
}

func (p *Proxy) chainA2A(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodPost, "/v1/a2a", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		keyRef := ""
		if apiKey != nil {
			keyRef = apiKey.Name
		}
		p.handleA2A(gw, w, r, keyRef)
	})(gw, w, r)
}

// authenticateAndAuthorize validates the token and enforces RBAC permissions.
// Returns the matched ApiKey and a boolean indicating success.
func (p *Proxy) authenticateAndAuthorize(r *http.Request, w http.ResponseWriter) (*config.ApiKey, bool) {
	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return nil, false
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		p.logAuthWarning(gw, "missing or malformed Authorization header", r)
		gw.Metrics.RecordAuthFailure("missing_header")
		writeStructuredError(w, http.StatusUnauthorized, infra.ErrorKindAuthFailed, "Unauthorized")
		return nil, false
	}
	clientToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))

	apiKey, ok := p.resolveAuthenticatedKey(gw, clientToken)
	if !ok {
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "Forbidden")
		return nil, false
	}

	// Primary token has full access — skip RBAC.
	// Record success metric for auth telemetry.
	if apiKey == nil {
		gw.Metrics.RecordAuthSuccess("client_token", "primary")
		return &config.ApiKey{Name: "primary", Roles: []string{"admin"}, Enabled: true}, true
	}

	if !apiKey.Enabled {
		gw.Metrics.IncAuthDenials(apiKey.Name, "disabled")
		p.logAuthDenial(gw, apiKey, "disabled key", r)
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "Forbidden")
		return nil, false
	}

	if apiKey.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, apiKey.ExpiresAt); err == nil && time.Now().After(t) {
			gw.Metrics.IncAuthDenials(apiKey.Name, "expired")
			p.logAuthDenial(gw, apiKey, "expired key", r)
			writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "Forbidden")
			return nil, false
		}
	}

	// Enforce endpoint-level RBAC.
	if !auth.AuthorizeEndpoint(apiKey, r.Method, r.URL.Path) {
		gw.Metrics.IncAuthDenials(apiKey.Name, "endpoint")
		p.logAuthDenial(gw, apiKey, fmt.Sprintf("endpoint %s %s", r.Method, r.URL.Path), r)
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "Forbidden")
		return nil, false
	}

	// Agent-level authorization for the chat wire routes: both carry a
	// top-level "model" field, and resolveRoute maps them to the same
	// handler (see isChatRoute). /v1/responses is handled by its handler
	// (handleResponses) which reads the body for the model field and
	// performs RBAC checks there.
	if isChatRoute(r.URL.Path) && !p.enforceChatAgentScoping(gw, w, r, apiKey) {
		return nil, false
	}

	// Per-key request rate limit (NENYA-20): enforced after authz so
	// authorization denials take precedence over throttling.
	if apiKey.RatelimitMaxRPM > 0 && !gw.KeyUsage.AllowRequest(apiKey.Name, apiKey.RatelimitMaxRPM) {
		gw.Metrics.IncAuthDenials(apiKey.Name, "rate_limited")
		p.logAuthDenial(gw, apiKey, "key rpm limit exceeded", r)
		writeStructuredError(w, http.StatusTooManyRequests, infra.ErrorKindRateLimited, "API key rate limit exceeded")
		return nil, false
	}

	gw.Metrics.RecordAuthSuccess("api_key", apiKey.Name)
	return apiKey, true
}

// enforceChatAgentScoping enforces per-key agent scoping on the chat wire
// routes. The body read fails closed: unreadable, oversized, or unparseable
// bodies are rejected (413/400) rather than allowed to bypass scoping.
// Returns ok=false when the request is rejected; the response is written.
func (p *Proxy) enforceChatAgentScoping(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) bool {
	agentName, err := extractAgentName(r, w, gw.Config.Server.MaxBodyBytes)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			gw.Metrics.IncAuthDenials(apiKey.Name, "payload_too_large")
			p.logAuthDenial(gw, apiKey, "body exceeds max_body_bytes on chat route", r)
			writeStructuredError(w, http.StatusRequestEntityTooLarge, infra.ErrorKindPayloadTooLarge, "Payload Too Large")
			return false
		}
		gw.Metrics.IncAuthDenials(apiKey.Name, "invalid_body")
		p.logAuthDenial(gw, apiKey, "invalid body on chat route", r)
		writeStructuredError(w, http.StatusBadRequest, infra.ErrorKindInvalidRequest, "Invalid JSON payload")
		return false
	}
	if agentName != "" && !auth.AuthorizeAgent(apiKey, agentName) {
		gw.Metrics.IncAuthDenials(apiKey.Name, "agent")
		p.logAuthDenial(gw, apiKey, "agent "+agentName, r)
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindAuthFailed, "Forbidden")
		return false
	}
	return true
}

// resolveAuthenticatedKey checks the Bearer token against configured credentials.
// Returns (*config.ApiKey, true) for a matched API key, (nil, true) for a
// matching primary token, or (nil, false) on failure.
func (p *Proxy) resolveAuthenticatedKey(gw *gateway.NenyaGateway, clientToken string) (*config.ApiKey, bool) {
	// Primary token check
	if gw.Secrets.ClientToken != "" {
		var tokenOK bool
		if gw.SecureMem != nil {
			tokenOK = gw.SecureMem.CompareToken(gw.ClientTokenRef, clientToken)
		} else {
			tokenOK = hmac.Equal([]byte(clientToken), []byte(gw.Secrets.ClientToken))
		}
		if tokenOK {
			return nil, true
		}
	}

	// Per-key check
	if gw.Secrets == nil {
		return nil, false
	}
	for _, key := range gw.Secrets.ApiKeys {
		if hmac.Equal([]byte(clientToken), []byte(key.Token)) {
			matched := key
			return &matched, true
		}
	}

	return nil, false
}

// isChatRoute reports whether path is one of the two chat wire routes served
// by chainChat. Both carry a top-level model field and share agent-level RBAC;
// resolveRoute and authenticateAndAuthorize must agree on this set.
func isChatRoute(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/messages"
}

// extractAgentName reads the model name from the request body for the chat
// wire routes. The body is fully buffered — bounded by maxBodyBytes (falling
// back to fallbackAuthBodyLimit when the server config carries no limit) —
// and restored for downstream handlers, so large payloads are not truncated
// by the auth pass. Returns the model name and a nil error for a parseable
// JSON body (possibly with an empty model). Returns *http.MaxBytesError when
// the body exceeds the limit, and a generic error for missing/unparseable
// bodies: callers must treat every error as a scoping failure and deny the
// request (fail closed) rather than skip agent scoping.
func extractAgentName(r *http.Request, w http.ResponseWriter, maxBodyBytes int64) (string, error) {
	if r.Body == nil {
		return "", errors.New("request has no body")
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = fallbackAuthBodyLimit
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	// Restore whatever was read so downstream handlers can re-read the body.
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", errors.New("request body is not valid JSON")
	}
	return req.Model, nil
}

func (p *Proxy) logAuthWarning(gw *gateway.NenyaGateway, msg string, r *http.Request) {
	if gw.Logger == nil {
		return
	}
	gw.Logger.Warn(msg,
		"remote_addr", r.RemoteAddr,
		"method", r.Method,
		"path", r.URL.Path,
		"user_agent", r.Header.Get("User-Agent"),
	)
}

func (p *Proxy) logAuthDenial(gw *gateway.NenyaGateway, key *config.ApiKey, reason string, r *http.Request) {
	if gw.Logger == nil || key == nil {
		return
	}
	gw.Logger.Warn("auth denied",
		"key_name", key.Name,
		"reason", reason,
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
	)
}

// authenticateRequest is a thin wrapper around authenticateAndAuthorize
// that returns the key name (string) for backward compatibility in tests.
func (p *Proxy) authenticateRequest(r *http.Request, w http.ResponseWriter) (string, bool) {
	key, ok := p.authenticateAndAuthorize(r, w)
	if !ok || key == nil {
		return "", ok
	}
	return key.Name, ok
}

// handleMetrics serves the Prometheus-compatible metrics endpoint.
func (p *Proxy) handleMetrics(w http.ResponseWriter, r *http.Request) {
	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return
	}
	infra.HandleMetrics(gw.Metrics, w, r)
}

// modelsEntry represents a single model in the /v1/models response.
type modelEntry struct {
	ID                string  `json:"id"`
	Object            string  `json:"object"`
	OwnedBy           string  `json:"owned_by"`
	ContextWindow     int     `json:"context_window,omitempty"`
	MaxTokens         int     `json:"max_tokens,omitempty"`
	SupportsVision    bool    `json:"supports_vision,omitempty"`
	SupportsTools     bool    `json:"supports_tool_calls,omitempty"`
	SupportsReasoning bool    `json:"supports_reasoning,omitempty"`
	InputCostPer1M    float64 `json:"input_cost_per_1m,omitempty"`
	OutputCostPer1M   float64 `json:"output_cost_per_1m,omitempty"`
	RoutingStrategy   string  `json:"routing_strategy,omitempty"`
	Description       string  `json:"description,omitempty"`
}

// buildAgentModelEntry creates a modelEntry for an agent pseudo-model,
// aggregating metadata (context window, capabilities, pricing) from the
// agent's model chain using the routing resolution helpers.
// Returns a default entry (zero metadata) if gw is nil.
func buildAgentModelEntry(agentName string, agent config.AgentConfig, gw *gateway.NenyaGateway) modelEntry {
	entry := modelEntry{
		ID:      agentName,
		Object:  "model",
		OwnedBy: "nenya",
	}
	if gw == nil {
		return entry
	}

	if maxCtx := routing.ResolveWindowMaxContext(agentName, gw.Config.Agents, gw.ModelCatalog); maxCtx > 0 {
		entry.ContextWindow = maxCtx
	}
	if maxOut := routing.ResolveWindowMaxOutput(agentName, gw.Config.Agents, gw.ModelCatalog); maxOut > 0 {
		entry.MaxTokens = maxOut
	}

	caps := routing.ResolveAgentCapabilities(agentName, gw.Config.Agents, gw.ModelCatalog)
	entry.SupportsVision = caps.SupportsVision
	entry.SupportsTools = caps.SupportsToolCalls
	entry.SupportsReasoning = caps.SupportsReasoning

	if pricing := routing.ResolveAgentPricing(agentName, gw.Config.Agents, gw.ModelCatalog); pricing.HasPricing {
		entry.InputCostPer1M = pricing.InputCostPer1M
		entry.OutputCostPer1M = pricing.OutputCostPer1M
	}

	if agent.Strategy != "" {
		entry.RoutingStrategy = agent.Strategy
	}
	if agent.Description != "" {
		entry.Description = agent.Description
	}
	return entry
}

// applyModelFields sets optional metadata fields on a modelEntry from the
// given discovery metadata and pricing, applying the same omitempty logic
// as the /v1/models response.
func applyModelFields(entry *modelEntry, maxCtx, maxOut int, meta *discovery.ModelMetadata, pricing *discovery.PricingEntry) {
	if maxCtx > 0 {
		entry.ContextWindow = maxCtx
	}
	if maxOut > 0 {
		entry.MaxTokens = maxOut
	}
	if meta != nil {
		entry.SupportsVision = meta.SupportsVision
		entry.SupportsTools = meta.SupportsToolCalls
		entry.SupportsReasoning = meta.SupportsReasoning
	}
	if pricing != nil && !pricing.IsZero() {
		entry.InputCostPer1M = pricing.InputCostPer1M
		entry.OutputCostPer1M = pricing.OutputCostPer1M
	}
}

// catalogModelEntries converts catalog models into /v1/models entries,
// skipping providers without credentials, non-chat models, and IDs already
// seen (from agent pseudo-models). seen is mutated to record emitted IDs.
func catalogModelEntries(gw *gateway.NenyaGateway, seen map[string]bool) []modelEntry {
	if gw.ModelCatalog == nil {
		return nil
	}
	var models []modelEntry
	for _, m := range gw.ModelCatalog.AllModels() {
		if !catalogModelEligible(gw, m, seen) {
			continue
		}
		seen[m.ID] = true
		entry := modelEntry{ID: m.ID, Object: "model", OwnedBy: m.OwnedBy}
		applyModelFields(&entry, m.MaxContext, m.MaxOutput, m.Metadata, m.Pricing)
		models = append(models, entry)
	}
	return models
}

// isNonChatModel reports whether a model ID is a chat model from the gateway's
// perspective: at least one provider that actually serves it treats it as a
// chat model. Providers with no catalog entry for the ID have no opinion and
// cannot veto the classification.
func isNonChatModel(gw *gateway.NenyaGateway, model string) bool {
	if gw.ModelCatalog == nil {
		return !config.AnyProviderServesAsChat(gw.Providers, model, nil)
	}
	var servable []config.ServedModel
	for _, m := range gw.ModelCatalog.LookupAll(model) {
		servable = append(servable, config.ServedModel{Provider: m.Provider, Model: m.ID})
	}
	return !config.AnyProviderServesAsChat(gw.Providers, model, servable)
}

// catalogModelEligible reports whether a catalog model should appear in
// /v1/models: its provider is configured, the model is a chat model, and it
// has not already been emitted.
func catalogModelEligible(gw *gateway.NenyaGateway, m discovery.DiscoveredModel, seen map[string]bool) bool {
	provider, ok := gw.Providers[m.Provider]
	if !ok {
		return false
	}
	if provider.APIKey == "" && provider.AuthStyle != config.AuthStyleNone {
		return false
	}
	if provider.IsNonChatModel(m.ID) {
		return false
	}
	return !seen[m.ID]
}

// handleModels returns the list of available models from all configured providers.
func (p *Proxy) handleModels(w http.ResponseWriter) {
	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return
	}

	var models []modelEntry
	seen := make(map[string]bool)

	for agentName, agent := range gw.Config.Agents {
		if len(agent.Models) == 1 && isNonChatModel(gw, agent.Models[0].Model) {
			// A single-model agent that resolves to a non-chat model (e.g.
			// jev-1.13-free) is not a chat target; don't advertise it.
			continue
		}
		seen[agentName] = true
		models = append(models, buildAgentModelEntry(agentName, agent, gw))
	}

	models = append(models, catalogModelEntries(gw, seen)...)

	resp := map[string]interface{}{
		"object": "list",
		"data":   models,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		gw.Logger.Error("failed to encode models response", "err", err)
	}
}

// handleStats provides runtime statistics including usage, circuit breaker state, and MCP status.
func (p *Proxy) handleStats(w http.ResponseWriter) {
	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	stats := gw.Stats.Snapshot()
	stats["circuit_breakers"] = gw.AgentState.CBDetailedSnapshot()

	mcpServers := make(map[string]interface{})
	for name, client := range gw.MCPClients {
		serverInfo := client.ServerInfo()
		tools := client.ListTools()
		mcpServers[name] = map[string]interface{}{
			"ready":   client.Ready(),
			"tools":   len(tools),
			"version": serverInfo.Version,
		}
	}
	stats["mcp"] = mcpServers

	// Per-key/per-provider budget usage (NENYA-20), additive for backward
	// compatibility.
	if gw.KeyUsage != nil {
		stats["key_usage"] = gw.KeyUsage.Snapshot()
	}

	if gw.HealthRegistry != nil {
		stats["provider_health"] = gw.HealthRegistry.Snapshot()
	}

	streamPool := stream.GetPoolStats()
	stats["stream_buffer_pool"] = map[string]interface{}{
		"hits":   streamPool["hits"],
		"misses": streamPool["misses"],
	}

	p.addBillingStats(stats, gw)

	if err := json.NewEncoder(w).Encode(stats); err != nil {
		gw.Logger.Error("failed to encode stats response", "err", err)
	}
}

// handleHealthz provides health status including engine readiness.
func (p *Proxy) handleHealthz(w http.ResponseWriter, r *http.Request) {
	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return
	}
	engineOK := p.checkSecurityFilterEngineHealth(r.Context())

	resp := map[string]interface{}{
		"status": "ok",
		"engine": map[string]interface{}{
			"status": engineOK,
		},
	}

	status := http.StatusOK
	if !engineOK {
		status = http.StatusServiceUnavailable
		resp["status"] = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		gw.Logger.Error("failed to encode healthz response", "err", err)
	}
}

// checkSecurityFilterEngineHealth verifies that the security filter engine is operational.
func (p *Proxy) checkSecurityFilterEngineHealth(ctx context.Context) bool {
	gw := p.Gateway()
	if gw == nil {
		return false
	}
	ref := gw.Config.Bouncer.Engine

	if len(ref.ResolvedTargets) > 0 {
		for _, target := range ref.ResolvedTargets {
			if target.Provider.ApiFormat != "ollama" {
				return true
			}
			if p.checkOllamaProviderHealth(ctx, gw, target.Provider.URL) {
				return true
			}
		}
		return false
	}

	pr, ok := gw.Providers[ref.Provider]
	if !ok {
		gw.Logger.Warn("engine provider not found", "provider", ref.Provider)
		return false
	}
	if pr.ApiFormat != "ollama" {
		return true
	}
	return p.checkOllamaProviderHealth(ctx, gw, pr.URL)
}

// checkOllamaProviderHealth checks the health of an Ollama provider instance.
func (p *Proxy) checkOllamaProviderHealth(ctx context.Context, gw *gateway.NenyaGateway, providerURL string) bool {
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, config.OllamaHealthURL(providerURL), nil)
	if err != nil {
		return false
	}

	resp, err := util.DoWithRetryResp(healthCtx, 2, func() (*http.Response, error) {
		r, fetchErr := gw.OllamaClient.Do(req)
		if fetchErr != nil {
			if r != nil {
				_ = r.Body.Close()
			}
			return nil, fetchErr
		}
		if r.StatusCode >= 500 {
			_ = r.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", r.StatusCode)
		}
		return r, nil
	})
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}

// handlePprof serves pprof profiles for performance analysis.
func (p *Proxy) handlePprof(w http.ResponseWriter, r *http.Request) {
	gw := p.Gateway()
	if gw == nil {
		writeStructuredError(w, http.StatusServiceUnavailable, infra.ErrorKindInternal, "Gateway not initialized")
		return
	}

	if gw.Config.Debug.PprofEnabled == nil || !*gw.Config.Debug.PprofEnabled {
		writeStructuredError(w, http.StatusForbidden, infra.ErrorKindInvalidRequest, "pprof is disabled")
		return
	}

	handler := http.DefaultServeMux
	handler.ServeHTTP(w, r)
}

func (p *Proxy) addBillingStats(stats map[string]interface{}, gw *gateway.NenyaGateway) {
	if gw.BillingTracker == nil {
		return
	}
	accounts := gw.BillingTracker.GetAllAccounts()
	billingInfo := make([]map[string]interface{}, 0, len(accounts))
	totalSpend := 0.0
	exhaustedCount := 0
	for _, acc := range accounts {
		spend := gw.BillingTracker.GetTotalSpend(acc.Provider, acc.AccountName)
		isExhausted := gw.BillingTracker.IsExhausted(acc.Provider, acc.AccountName)
		totalSpend += spend
		if isExhausted {
			exhaustedCount++
		}
		billingInfo = append(billingInfo, map[string]interface{}{
			"provider":     acc.Provider,
			"account":      acc.AccountName,
			"total_spend":  spend,
			"is_exhausted": isExhausted,
		})
	}
	stats["billing"] = map[string]interface{}{
		"accounts":        billingInfo,
		"total_spend_usd": totalSpend,
		"exhausted_count": exhaustedCount,
		"total_requests":  gw.BillingTracker.TotalRequests.Load(),
	}
}
