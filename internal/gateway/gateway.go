package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"nenya/internal/adapter"
	"nenya/internal/config"
	"nenya/internal/infra"
	"nenya/internal/mcp"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
)

type NenyaGateway struct {
	Config          config.Config
	Client          *http.Client
	OllamaClient    *http.Client
	Secrets         *config.SecretsConfig
	Providers       map[string]*config.Provider
	RateLimiter     *infra.RateLimiter
	SecretPatterns  []*regexp.Regexp
	BlockedPatterns []*regexp.Regexp
	EntropyFilter   *pipeline.EntropyFilter
	Stats           *infra.UsageTracker
	Metrics         *infra.Metrics
	Logger          *slog.Logger
	AgentState      *routing.AgentState
	ThoughtSigCache *infra.ThoughtSignatureCache
	ResponseCache   *infra.ResponseCache
	MCPClients      map[string]*mcp.Client
	MCPToolIndex    *mcp.ToolRegistry
}

func New(cfg config.Config, secrets *config.SecretsConfig, logger *slog.Logger) *NenyaGateway {
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]config.ProviderConfig)
	}
	for name, builtIn := range config.BuiltInProviders() {
		if _, exists := cfg.Providers[name]; !exists {
			cfg.Providers[name] = builtIn
		}
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}

	secureClient := &http.Client{
		Transport: transport,
	}

	ollamaResponseHeaderTimeout := 30 * time.Second
	if ollamaCfg, ok := cfg.Providers["ollama"]; ok && ollamaCfg.TimeoutSeconds > 0 {
		ollamaResponseHeaderTimeout = time.Duration(ollamaCfg.TimeoutSeconds) * time.Second
	}

	ollamaTransport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: ollamaResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   2,
	}
	ollamaClient := &http.Client{
		Transport: ollamaTransport,
	}

	providers := config.ResolveProviders(&cfg, secrets)

	var secretPatterns []*regexp.Regexp
	if cfg.SecurityFilter.Enabled && len(cfg.SecurityFilter.Patterns) > 0 {
		for _, pattern := range cfg.SecurityFilter.Patterns {
			re, err := regexp.Compile(pattern)
			if err != nil {
				logger.Warn("failed to compile secret pattern, skipping", "pattern", pattern, "err", err)
				continue
			}
			secretPatterns = append(secretPatterns, re)
		}
		logger.Info("compiled secret patterns for Tier-0 filtering", "count", len(secretPatterns))
	}

	var blockedPatterns []*regexp.Regexp
	for _, pattern := range cfg.Governance.BlockedExecutionPatterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			logger.Warn("failed to compile blocked execution pattern, skipping", "pattern", pattern, "err", err)
			continue
		}
		blockedPatterns = append(blockedPatterns, re)
	}
	if len(blockedPatterns) > 0 {
		logger.Info("compiled blocked execution patterns for stream kill switch", "count", len(blockedPatterns))
	}

	var entropyFilter *pipeline.EntropyFilter
	if cfg.SecurityFilter.EntropyEnabled {
		entropyFilter = pipeline.NewEntropyFilter(
			cfg.SecurityFilter.EntropyThreshold,
			cfg.SecurityFilter.EntropyMinToken,
		)
		logger.Info("entropy filter enabled", "threshold", cfg.SecurityFilter.EntropyThreshold, "min_token", cfg.SecurityFilter.EntropyMinToken)
	}

	gw := &NenyaGateway{
		Config:          cfg,
		Client:          secureClient,
		OllamaClient:    ollamaClient,
		Secrets:         secrets,
		Providers:       providers,
		RateLimiter:     infra.NewRateLimiter(cfg.Governance.RatelimitMaxRPM, cfg.Governance.RatelimitMaxTPM),
		SecretPatterns:  secretPatterns,
		BlockedPatterns: blockedPatterns,
		EntropyFilter:   entropyFilter,
		Stats:           infra.NewUsageTracker(),
		Metrics:         nil,
		Logger:          logger,
		AgentState:      routing.NewAgentState(logger),
		ThoughtSigCache: infra.NewThoughtSignatureCache(1000, 30*time.Minute),
		ResponseCache:   newResponseCache(cfg, logger),
		MCPClients:      buildMCPClients(cfg, logger),
		MCPToolIndex:    mcp.NewToolRegistry(),
	}

	gw.buildMCPToolIndex(logger)

	gw.Metrics = infra.NewMetrics()
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot
	gw.Metrics.Cooldowns = gw.AgentState.ActiveCooldowns
	gw.Metrics.CBStates = gw.AgentState.CBSnapshot

	adapter.InitWithDeps(logger, gw.ThoughtSigCache, ExtractContentText)

	return gw
}

func (g *NenyaGateway) InitMetrics() {
	g.Metrics = infra.NewMetrics()
	g.Metrics.RateLimits = g.RateLimiter.Snapshot
	g.Metrics.Cooldowns = g.AgentState.ActiveCooldowns
	g.Metrics.CBStates = g.AgentState.CBSnapshot
}

func (g *NenyaGateway) CountTokens(text string) int {
	ratio := g.Config.Server.TokenRatio
	if ratio <= 0 {
		ratio = 4.0
	}
	return int(float64(utf8.RuneCountInString(text)) / ratio)
}

func (g *NenyaGateway) CountRequestTokens(payload map[string]interface{}) int {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		return 0
	}
	var sb strings.Builder
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		sb.WriteString(ExtractContentText(msg))
	}
	return g.CountTokens(sb.String())
}

func ExtractContentText(msg map[string]interface{}) string {
	contentRaw, ok := msg["content"]
	if !ok {
		return ""
	}
	switch content := contentRaw.(type) {
	case string:
		return content
	case []interface{}:
		var sb strings.Builder
		for _, partRaw := range content {
			if part, ok := partRaw.(map[string]interface{}); ok {
				if typ, ok := part["type"].(string); ok {
					switch typ {
					case "text":
						if text, ok := part["text"].(string); ok {
							sb.WriteString(text)
						}
					case "image_url":
						sb.WriteString("[image]")
					case "input_json":
						if input, ok := part["input_json"]; ok {
							if jsonBytes, err := json.Marshal(input); err == nil {
								sb.Write(jsonBytes)
							}
						}
					}
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func newResponseCache(cfg config.Config, logger *slog.Logger) *infra.ResponseCache {
	if !cfg.ResponseCache.Enabled {
		return nil
	}
	rc := cfg.ResponseCache
	cache := infra.NewResponseCache(
		rc.MaxEntries,
		rc.MaxEntryBytes,
		time.Duration(rc.TTLSeconds)*time.Second,
		time.Duration(rc.EvictEverySeconds)*time.Second,
	)
	logger.Info("response cache enabled",
		"max_entries", rc.MaxEntries,
		"max_entry_bytes", rc.MaxEntryBytes,
		"ttl_seconds", rc.TTLSeconds,
		"evict_every_seconds", rc.EvictEverySeconds)
	return cache
}

func (g *NenyaGateway) Close() {
	if g.ResponseCache != nil {
		g.ResponseCache.Stop()
	}
	for name, client := range g.MCPClients {
		client.Close()
		g.Logger.Debug("MCP client closed", "server", name)
	}
}

func (g *NenyaGateway) Reload(cfg config.Config, secrets *config.SecretsConfig) *NenyaGateway {
	newGW := New(cfg, secrets, g.Logger)

	newGW.Stats = g.Stats
	newGW.Metrics = g.Metrics
	newGW.ThoughtSigCache = g.ThoughtSigCache

	newGW.Metrics.RateLimits = newGW.RateLimiter.Snapshot
	newGW.Metrics.Cooldowns = newGW.AgentState.ActiveCooldowns
	newGW.Metrics.CBStates = newGW.AgentState.CBSnapshot

	g.Close()

	return newGW
}

func buildMCPClients(cfg config.Config, logger *slog.Logger) map[string]*mcp.Client {
	clients := make(map[string]*mcp.Client)
	for name, serverCfg := range cfg.MCPServers {
		if serverCfg.URL == "" {
			logger.Warn("MCP server has empty URL, skipping", "server", name)
			continue
		}
		client := mcp.NewClient(mcp.ClientConfig{
			Name:              "nenya",
			URL:               serverCfg.URL,
			Headers:           serverCfg.Headers,
			RequestTimeout:    time.Duration(serverCfg.Timeout) * time.Second,
			KeepAliveInterval: time.Duration(serverCfg.KeepAliveInterval) * time.Second,
			Logger:            logger,
		})
		clients[name] = client
	}
	return clients
}

func (g *NenyaGateway) buildMCPToolIndex(logger *slog.Logger) {
	for name, client := range g.MCPClients {
		ctx, cancel := contextWithTimeout(10 * time.Second)
		err := client.Initialize(ctx)
		cancel()
		if err != nil {
			logger.Warn("MCP client initialization failed, skipping",
				"server", name, "err", err)
			continue
		}

		ctx, cancel = contextWithTimeout(15 * time.Second)
		tools, err := client.RefreshTools(ctx)
		cancel()
		if err != nil {
			logger.Warn("MCP tool list refresh failed",
				"server", name, "err", err)
			continue
		}

		g.MCPToolIndex.Register(name, tools)
		logger.Info("MCP server connected",
			"server", name,
			"tools", len(tools),
			"server_version", client.ServerInfo().Version)
	}
}

func (g *NenyaGateway) GetMCPClientsForAgent(agentName string) map[string]*mcp.Client {
	agent, ok := g.Config.Agents[agentName]
	if !ok || agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return nil
	}
	clients := make(map[string]*mcp.Client, len(agent.MCP.Servers))
	for _, serverName := range agent.MCP.Servers {
		if client, ok := g.MCPClients[serverName]; ok {
			clients[serverName] = client
		}
	}
	if len(clients) == 0 {
		return nil
	}
	return clients
}

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
