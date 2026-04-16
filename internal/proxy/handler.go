package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
)

const MaxModelNameLength = 256

type Proxy struct {
	GW *gateway.NenyaGateway
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			p.GW.Logger.Error("panic recovered", "err", rec, "stack", string(debug.Stack()))
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		}
	}()

	switch {
	case r.URL.Path == "/healthz":
		infra.ObserveHTTP(p.GW.Metrics, p.handleHealthz)(w, r)
		return
	case r.URL.Path == "/statsz":
		infra.ObserveHTTP(p.GW.Metrics, p.handleStats)(w, r)
		return
	case r.URL.Path == "/metrics":
		if !p.authenticateRequest(r, w) {
			return
		}
		infra.ObserveHTTPFunc(p.GW.Metrics, p.handleMetrics)(w, r)
		return
	case r.URL.Path == "/v1/models" && r.Method == http.MethodGet:
		if !p.authenticateRequest(r, w) {
			return
		}
		infra.ObserveHTTP(p.GW.Metrics, p.handleModels)(w, r)
		return
	case r.URL.Path == "/v1/chat/completions" && r.Method == http.MethodPost:
		if !p.authenticateRequest(r, w) {
			return
		}
		infra.ObserveHTTPFunc(p.GW.Metrics, p.handleChatCompletions)(w, r)
		return
	case r.URL.Path == "/v1/embeddings" && r.Method == http.MethodPost:
		if !p.authenticateRequest(r, w) {
			return
		}
		infra.ObserveHTTPFunc(p.GW.Metrics, p.handleEmbeddings)(w, r)
		return
	case r.URL.Path == "/v1/responses" && r.Method == http.MethodPost:
		if !p.authenticateRequest(r, w) {
			return
		}
		infra.ObserveHTTPFunc(p.GW.Metrics, p.handleResponses)(w, r)
		return
	default:
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
}

func (p *Proxy) authenticateRequest(r *http.Request, w http.ResponseWriter) bool {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		p.GW.Logger.Warn("missing or malformed Authorization header")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return false
	}
	clientToken := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
	if subtle.ConstantTimeCompare([]byte(clientToken), []byte(p.GW.Secrets.ClientToken)) != 1 {
		p.GW.Logger.Warn("invalid client token")
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (p *Proxy) handleMetrics(w http.ResponseWriter, r *http.Request) {
	infra.HandleMetrics(p.GW.Metrics, w, r)
}

func (p *Proxy) handleModels(w http.ResponseWriter) {
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}

	var models []modelEntry
	seen := make(map[string]bool)

	addModel := func(id, ownedBy string) {
		if seen[id] {
			return
		}
		seen[id] = true
		models = append(models, modelEntry{
			ID:      id,
			Object:  "model",
			OwnedBy: ownedBy,
		})
	}

	for agentName, agent := range p.GW.Config.Agents {
		addModel(agentName, "nenya")
		for _, m := range agent.Models {
			addModel(m.Model, m.Provider)
		}
	}

	for _, pr := range p.GW.Providers {
		if pr.APIKey == "" && pr.AuthStyle != "none" {
			continue
		}
		for _, prefix := range pr.RoutePrefixes {
			addModel(prefix+"*", pr.Name)
		}
	}

	resp := map[string]interface{}{
		"object": "list",
		"data":   models,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		p.GW.Logger.Error("failed to encode models response", "err", err)
	}
}

func (p *Proxy) handleStats(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	stats := p.GW.Stats.Snapshot()
	stats["circuit_breakers"] = p.GW.AgentState.CBSnapshot()

	mcpServers := make(map[string]interface{})
	for name, client := range p.GW.MCPClients {
		serverInfo := client.ServerInfo()
		tools := client.ListTools()
		mcpServers[name] = map[string]interface{}{
			"ready":    client.Ready(),
			"tools":    len(tools),
			"version":  serverInfo.Version,
		}
	}
	stats["mcp"] = mcpServers

	if err := json.NewEncoder(w).Encode(stats); err != nil {
		p.GW.Logger.Error("failed to encode stats response", "err", err)
	}
}

func (p *Proxy) handleHealthz(w http.ResponseWriter) {
	engineOK := p.checkSecurityFilterEngineHealth()

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
		p.GW.Logger.Error("failed to encode healthz response", "err", err)
	}
}

func (p *Proxy) checkSecurityFilterEngineHealth() bool {
	ref := p.GW.Config.SecurityFilter.Engine

	if len(ref.ResolvedTargets) > 0 {
		for _, target := range ref.ResolvedTargets {
			apiFormat := target.Provider.ApiFormat
			if apiFormat == "" {
				apiFormat = "openai"
			}
			if apiFormat != "ollama" {
				return true
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.OllamaHealthURL(target.Provider.URL), nil)
			if err != nil {
				cancel()
				continue
			}

			client := p.GW.OllamaClient
			resp, err := client.Do(req)
			if err != nil {
				cancel()
				continue
			}
			resp.Body.Close()
			cancel()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		return false
	}

	pr, ok := p.GW.Providers[ref.Provider]
	if !ok {
		p.GW.Logger.Warn("engine provider not found", "provider", ref.Provider)
		return false
	}
	apiFormat := pr.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}
	if apiFormat != "ollama" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, config.OllamaHealthURL(pr.URL), nil)
	if err != nil {
		cancel()
		return false
	}

	client := p.GW.OllamaClient
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return false
	}
	resp.Body.Close()
	cancel()
	return resp.StatusCode == http.StatusOK
}
