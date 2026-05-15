package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"nenya/config"
	"nenya/internal/auth"
	"nenya/internal/discovery"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/stream"
	"nenya/internal/util"
)

// MaxModelNameLength is the maximum allowed length for model names.
const MaxModelNameLength = 256

// Proxy handles HTTP requests and routes them to upstream AI providers.
type Proxy struct {
	gw          atomic.Pointer[gateway.NenyaGateway]
	ShutdownCtx context.Context
}

// Shutdown gracefully shuts down the gateway and waits for in-flight operations.
func (p *Proxy) Shutdown(ctx context.Context) error {
	if gw := p.Gateway(); gw != nil {
		return gw.Shutdown(ctx)
	}
	return nil
}

// StoreGateway sets the gateway instance for the proxy.
func (p *Proxy) StoreGateway(gw *gateway.NenyaGateway) {
	p.gw.Store(gw)
}

// Gateway returns the current gateway instance.
func (p *Proxy) Gateway() *gateway.NenyaGateway {
	return p.gw.Load()
}

// ServeHTTP handles incoming HTTP requests and routes them to appropriate handlers.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer p.recoverPanic(w)

	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}

	if handler := p.resolveRoute(r.URL.Path); handler != nil {
		handler(gw, w, r)
		return
	}
	http.Error(w, "Not Found", http.StatusNotFound)
}

type routeHandler func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request)

func (p *Proxy) recoverPanic(w http.ResponseWriter) {
	if rec := recover(); rec != nil {
		if gw := p.Gateway(); gw != nil {
			gw.Logger.Error("panic recovered", "err", rec)
			gw.Metrics.RecordPanic()
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
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
		{false, "/statsz", p.chainAuthStats},
		{false, "/metrics", p.chainAuthMetric},
		{false, "/debug/pprof", p.chainAuthPprof},
		{false, "/v1/models", p.chainModels},
		{false, "/v1/chat/completions", p.chainChat},
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
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
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

func (p *Proxy) chainAuthStats(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint(http.MethodGet, "/statsz", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		infra.ObserveHTTP(gw.Metrics, p.handleStats)(w, r)
	})(gw, w, r)
}

func (p *Proxy) chainAuthMetric(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/metrics", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
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
	p.chainEndpoint("", "/v1/chat/completions", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleChatCompletions(gw, w, r, apiKey)
	})(gw, w, r)
}

func (p *Proxy) chainEmbeddings(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request) {
	p.chainEndpoint("", "/v1/embeddings", true, func(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
		p.handleEmbeddings(gw, w, r, apiKey)
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
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return nil, false
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		p.logAuthWarning(gw, "missing or malformed Authorization header", r)
		gw.Metrics.RecordAuthFailure("missing_header")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	clientToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))

	apiKey, ok := p.resolveAuthenticatedKey(gw, clientToken)
	if !ok {
		http.Error(w, "Forbidden", http.StatusForbidden)
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
		http.Error(w, "Forbidden", http.StatusForbidden)
		return nil, false
	}

	if apiKey.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, apiKey.ExpiresAt); err == nil && time.Now().After(t) {
			gw.Metrics.IncAuthDenials(apiKey.Name, "expired")
			p.logAuthDenial(gw, apiKey, "expired key", r)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return nil, false
		}
	}

	// Enforce endpoint-level RBAC.
	if !auth.AuthorizeEndpoint(apiKey, r.Method, r.URL.Path) {
		gw.Metrics.IncAuthDenials(apiKey.Name, "endpoint")
		p.logAuthDenial(gw, apiKey, fmt.Sprintf("endpoint %s %s", r.Method, r.URL.Path), r)
		http.Error(w, "Forbidden", http.StatusForbidden)
		return nil, false
	}

	// Agent-level authorization for endpoints carrying a model/agent name.
	// NOTE: Only /v1/chat/completions is checked here. /v1/responses
	// is handled by its handler (handleResponses) which reads the body for
	// the model field and performs RBAC checks there.
	if r.URL.Path == "/v1/chat/completions" {
		agentName := extractAgentName(r)
		if agentName != "" && !auth.AuthorizeAgent(apiKey, agentName) {
			gw.Metrics.IncAuthDenials(apiKey.Name, "agent")
			p.logAuthDenial(gw, apiKey, fmt.Sprintf("agent %s", agentName), r)
			http.Error(w, "Forbidden", http.StatusForbidden)
			return nil, false
		}
	}

	gw.Metrics.RecordAuthSuccess("api_key", apiKey.Name)
	return apiKey, true
}

// resolveAuthenticatedKey checks the Bearer token against configured credentials.
// Returns (*config.ApiKey, true) for a matched API key, (nil, true) for a
// matching primary token, or (nil, false) on failure.
func (p *Proxy) resolveAuthenticatedKey(gw *gateway.NenyaGateway, clientToken string) (*config.ApiKey, bool) {
	// Primary token check
	if gw.Secrets.ClientToken != "" {
		tokenOK := false
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

// extractAgentName reads the model name from the request body for chat/responses endpoints.
// The body is restored after reading.
func extractAgentName(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return ""
	}
	r.Body = io.NopCloser(bytes.NewReader(body))

	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) == nil {
		return req.Model
	}
	return ""
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
	if gw.Logger == nil {
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
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
	infra.HandleMetrics(gw.Metrics, w, r)
}

// handleModels returns the list of available models from all configured providers.
func (p *Proxy) handleModels(w http.ResponseWriter) {
	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
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
	}

	var models []modelEntry
	seen := make(map[string]bool)

	addModel := func(id, ownedBy string, maxCtx, maxOut int, meta *discovery.ModelMetadata, pricing *discovery.PricingEntry) {
		if seen[id] {
			return
		}
		seen[id] = true
		entry := modelEntry{
			ID:      id,
			Object:  "model",
			OwnedBy: ownedBy,
		}
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
		models = append(models, entry)
	}

	for agentName := range gw.Config.Agents {
		addModel(agentName, "nenya", 0, 0, nil, nil)
	}

	if gw.ModelCatalog != nil {
		for _, m := range gw.ModelCatalog.AllModels() {
			provider, ok := gw.Providers[m.Provider]
			if !ok {
				continue
			}
			if provider.APIKey == "" && provider.AuthStyle != "none" {
				continue
			}
			addModel(m.ID, m.OwnedBy, m.MaxContext, m.MaxOutput, m.Metadata, m.Pricing)
		}
	}

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
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	stats := gw.Stats.Snapshot()
	stats["circuit_breakers"] = gw.AgentState.CBSnapshot()

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

	if gw.HealthRegistry != nil {
		stats["provider_health"] = gw.HealthRegistry.Snapshot()
	}

	streamPool := stream.GetPoolStats()
	stats["stream_buffer_pool"] = map[string]interface{}{
		"hits":   streamPool["hits"],
		"misses": streamPool["misses"],
	}

	if err := json.NewEncoder(w).Encode(stats); err != nil {
		gw.Logger.Error("failed to encode stats response", "err", err)
	}
}

// handleHealthz provides health status including engine readiness.
func (p *Proxy) handleHealthz(w http.ResponseWriter, r *http.Request) {
	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
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

	var resp *http.Response
	err = util.DoWithRetry(healthCtx, 2, func() error {
		var fetchErr error
		resp, fetchErr = gw.OllamaClient.Do(req)
		if fetchErr != nil {
			return fetchErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return nil
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
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}

	if gw.Config.Debug.PprofEnabled == nil || !*gw.Config.Debug.PprofEnabled {
		http.Error(w, "pprof is disabled", http.StatusForbidden)
		return
	}

	handler := http.DefaultServeMux
	handler.ServeHTTP(w, r)
}
