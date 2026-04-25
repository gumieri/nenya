package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"nenya/internal/config"
	"nenya/internal/discovery"
	"nenya/internal/gateway"
	"nenya/internal/infra"
)

const MaxModelNameLength = 256

type Proxy struct {
	gw atomic.Pointer[gateway.NenyaGateway]
}

func (p *Proxy) StoreGateway(gw *gateway.NenyaGateway) {
	p.gw.Store(gw)
}

func (p *Proxy) Gateway() *gateway.NenyaGateway {
	return p.gw.Load()
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			if gw := p.Gateway(); gw != nil {
				gw.Logger.Error("panic recovered", "err", rec, "stack", string(debug.Stack()))
				if gw.Metrics != nil {
					gw.Metrics.RecordPanic()
				}
			}
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}()

	if gw := p.Gateway(); gw != nil {
		switch {
		case r.URL.Path == "/healthz":
			if r.Method != http.MethodGet {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			infra.ObserveHTTP(gw.Metrics, p.handleHealthz)(w, r)
			return
		case r.URL.Path == "/statsz":
			if r.Method != http.MethodGet {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTP(gw.Metrics, p.handleStats)(w, r)
			return
		case r.URL.Path == "/metrics":
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTPFunc(gw.Metrics, p.handleMetrics)(w, r)
			return
		case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTP(gw.Metrics, p.handleModels)(w, r)
			return
		case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTPFunc(gw.Metrics, func(w http.ResponseWriter, r *http.Request) {
				p.handleChatCompletions(gw, w, r)
			})(w, r)
			return
		case r.URL.Path == "/v1/embeddings" && r.Method == http.MethodPost:
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTPFunc(gw.Metrics, func(w http.ResponseWriter, r *http.Request) {
				p.handleEmbeddings(gw, w, r)
			})(w, r)
			return
		case r.URL.Path == "/v1/responses" && r.Method == http.MethodPost:
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTPFunc(gw.Metrics, func(w http.ResponseWriter, r *http.Request) {
				p.handleResponses(gw, w, r)
			})(w, r)
			return
		case strings.HasPrefix(r.URL.Path, "/proxy/"):
			if !p.authenticateRequest(r, w) {
				return
			}
			infra.ObserveHTTPFunc(gw.Metrics, func(w http.ResponseWriter, r *http.Request) {
				p.handlePassthrough(gw, w, r)
			})(w, r)
			return
		default:
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
	} else {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
}

func (p *Proxy) authenticateRequest(r *http.Request, w http.ResponseWriter) bool {
	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return false
	}

	correlationID := r.Header.Get("X-Request-Id")
	if correlationID == "" {
		correlationID = r.Header.Get("X-Correlation-Id")
	}
	if correlationID == "" {
		correlationID = "unknown"
	}

	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		gw.Logger.Warn("missing or malformed Authorization header",
			"correlation_id", correlationID,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.Header.Get("User-Agent"),
			"auth_header_present", authHeader != "",
			"auth_header_prefix", func() string {
				if len(authHeader) > 10 {
					return authHeader[:10] + "..."
				}
				return authHeader
			}())
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	clientToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(clientToken), []byte(gw.Secrets.ClientToken)) != 1 {
		gw.Logger.Warn("invalid client token",
			"correlation_id", correlationID,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.Header.Get("User-Agent"))
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (p *Proxy) handleMetrics(w http.ResponseWriter, r *http.Request) {
	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
	infra.HandleMetrics(gw.Metrics, w, r)
}

func (p *Proxy) handleModels(w http.ResponseWriter) {
	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
	type modelEntry struct {
		ID             string   `json:"id"`
		Object         string   `json:"object"`
		OwnedBy        string   `json:"owned_by"`
		ContextWindow  int      `json:"context_window,omitempty"`
		MaxTokens      int      `json:"max_tokens,omitempty"`
		SupportsVision bool     `json:"supports_vision,omitempty"`
		SupportsTools  bool     `json:"supports_tool_calls,omitempty"`
		SupportsReasoning bool  `json:"supports_reasoning,omitempty"`
		InputCostPer1M float64  `json:"input_cost_per_1m,omitempty"`
		OutputCostPer1M float64 `json:"output_cost_per_1m,omitempty"`
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

	for _, pr := range gw.Providers {
		if pr.APIKey == "" && pr.AuthStyle != "none" {
			continue
		}
		for _, prefix := range pr.RoutePrefixes {
			addModel(prefix+"*", pr.Name, 0, 0, nil, nil)
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

	if err := json.NewEncoder(w).Encode(stats); err != nil {
		gw.Logger.Error("failed to encode stats response", "err", err)
	}
}

func (p *Proxy) handleHealthz(w http.ResponseWriter) {
	gw := p.Gateway()
	if gw == nil {
		http.Error(w, "Gateway not initialized", http.StatusServiceUnavailable)
		return
	}
	engineOK := p.checkSecurityFilterEngineHealth(context.Background())

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

func (p *Proxy) checkSecurityFilterEngineHealth(ctx context.Context) bool {
	gw := p.Gateway()
	if gw == nil {
		return false
	}
	ref := gw.Config.SecurityFilter.Engine

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

func (p *Proxy) checkOllamaProviderHealth(ctx context.Context, gw *gateway.NenyaGateway, providerURL string) bool {
	healthCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, config.OllamaHealthURL(providerURL), nil)
	if err != nil {
		return false
	}

	resp, err := gw.OllamaClient.Do(req)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == http.StatusOK
}
