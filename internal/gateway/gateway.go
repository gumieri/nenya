package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"nenya/internal/adapter"
	"nenya/internal/config"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	"nenya/internal/mcp"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
	"nenya/internal/tiktoken"
)

// NenyaGateway is the top-level container that holds all gateway components:
// configuration, HTTP clients, provider registry, MCP clients, metrics,
// rate limiter, caches, and the token counter.
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
	ModelCatalog    *discovery.ModelCatalog
	HealthRegistry  *discovery.HealthRegistry
	LatencyTracker  *infra.LatencyTracker
	CostTracker     *infra.CostTracker
}

// New creates a new NenyaGateway with the given configuration, secrets,
// and logger. It initializes HTTP clients, provider registry, rate limiter,
// metrics, MCP clients, and starts dynamic model discovery.
func New(ctx context.Context, cfg config.Config, secrets *config.SecretsConfig, logger *slog.Logger) *NenyaGateway {
	cfg = mergeBuiltInProviders(cfg)
	secureClient, ollamaClient := createHTTPClients(cfg)
	providers := config.ResolveProviders(&cfg, secrets)

	mergedCatalog, healthRegistry := performModelDiscovery(ctx, cfg, providers, logger)

	secretPatterns, blockedPatterns := compilePatterns(cfg, logger)
	entropyFilter := createEntropyFilter(cfg, logger)

	gw := buildGateway(cfg, secrets, secureClient, ollamaClient, providers,
		secretPatterns, blockedPatterns, entropyFilter, mergedCatalog, healthRegistry, logger)

	gw.buildMCPToolIndex(ctx, logger)

	gw.Metrics = infra.NewMetrics()
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot
	gw.Metrics.Cooldowns = gw.AgentState.ActiveCooldowns
	gw.Metrics.CBStates = gw.AgentState.CBSnapshot

	debugLogAgentModels(ctx, logger, cfg, mergedCatalog, providers)

	adapter.InitWithDeps(logger, gw.ThoughtSigCache, ExtractContentText)

	return gw
}

func mergeBuiltInProviders(cfg config.Config) config.Config {
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]config.ProviderConfig)
	}
	for name, builtIn := range config.BuiltInProviders() {
		if _, exists := cfg.Providers[name]; !exists {
			cfg.Providers[name] = builtIn
		}
	}
	return cfg
}

func createHTTPClients(cfg config.Config) (*http.Client, *http.Client) {
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

	return secureClient, ollamaClient
}

func performModelDiscovery(ctx context.Context, cfg config.Config, providers map[string]*config.Provider, logger *slog.Logger) (*discovery.ModelCatalog, *discovery.HealthRegistry) {
	var mergedCatalog *discovery.ModelCatalog
	var healthRegistry *discovery.HealthRegistry

	if !cfg.Discovery.Enabled {
		mergedCatalog = discovery.MergeCatalog(discovery.NewModelCatalog(), &cfg)
		return mergedCatalog, nil
	}

	fetcher := discovery.NewDiscoveryFetcher(cfg.Governance.EffectiveMaxRetryAttempts())
	catalog := fetcher.FetchAll(ctx, providers, logger)
	mergedCatalog = discovery.MergeCatalog(catalog, &cfg)

	if _, hasOR := providers["openrouter"]; hasOR {
		fetchOpenRouterPricing(ctx, providers, mergedCatalog, logger)
	}

	logger.Info("model discovery completed", "total_models", len(mergedCatalog.AllModels()), "fetched_at", catalog.FetchedAt().Format(time.RFC3339))

	healthRegistry = discovery.ValidateAllProviders(providers, mergedCatalog, logger)

	generateAutoAgents(cfg, mergedCatalog, providers, logger)

	return mergedCatalog, healthRegistry
}

func fetchOpenRouterPricing(ctx context.Context, providers map[string]*config.Provider, catalog *discovery.ModelCatalog, logger *slog.Logger) {
	pfCtx, pfCancel := context.WithTimeout(ctx, 20*time.Second)
	defer pfCancel()

	pf := discovery.NewPricingFetcher(logger)
	if orPricing, err := pf.FetchOpenRouterPricing(pfCtx); err != nil {
		logger.Warn("failed to fetch openrouter pricing, skipping", "err", err)
	} else {
		logger.Info("fetched openrouter pricing", "models_with_pricing", len(orPricing))
		catalog.AttachPricing(orPricing)
	}
}

func generateAutoAgents(cfg config.Config, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) {
	logger.Debug("auto-agents check", "discovery_enabled", cfg.Discovery.Enabled, "auto_agents", cfg.Discovery.AutoAgents)
	if !cfg.Discovery.AutoAgents {
		return
	}
	autoAgents := discovery.GenerateAutoAgents(catalog, providers, cfg.Discovery.AutoAgentsConfig, logger)
	for name, agent := range autoAgents {
		if _, exists := cfg.Agents[name]; !exists {
			cfg.Agents[name] = agent
		}
	}
}

func compilePatterns(cfg config.Config, logger *slog.Logger) ([]*regexp.Regexp, []*regexp.Regexp) {
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

	return secretPatterns, blockedPatterns
}

func createEntropyFilter(cfg config.Config, logger *slog.Logger) *pipeline.EntropyFilter {
	if !cfg.SecurityFilter.EntropyEnabled {
		return nil
	}
	entropyFilter := pipeline.NewEntropyFilter(
		cfg.SecurityFilter.EntropyThreshold,
		cfg.SecurityFilter.EntropyMinToken,
	)
	logger.Info("entropy filter enabled", "threshold", cfg.SecurityFilter.EntropyThreshold, "min_token", cfg.SecurityFilter.EntropyMinToken)
	return entropyFilter
}

func buildGateway(cfg config.Config, secrets *config.SecretsConfig, secureClient *http.Client, ollamaClient *http.Client, providers map[string]*config.Provider, secretPatterns []*regexp.Regexp, blockedPatterns []*regexp.Regexp, entropyFilter *pipeline.EntropyFilter, mergedCatalog *discovery.ModelCatalog, healthRegistry *discovery.HealthRegistry, logger *slog.Logger) *NenyaGateway {
	return &NenyaGateway{
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
		ModelCatalog:    mergedCatalog,
		HealthRegistry:  healthRegistry,
		LatencyTracker:  infra.NewLatencyTracker(),
		CostTracker:     infra.NewCostTracker(),
	}
}

func debugLogAgentModels(ctx context.Context, logger *slog.Logger, cfg config.Config, mergedCatalog *discovery.ModelCatalog, providers map[string]*config.Provider) {
	if !logger.Enabled(ctx, slog.LevelDebug) {
		return
	}
	for name, agent := range cfg.Agents {
		modelEntries := make([]string, 0, len(agent.Models))
		for _, m := range agent.Models {
			entries := resolveModelEntry(m, mergedCatalog, providers)
			modelEntries = append(modelEntries, entries...)
		}
		mcpSrv := []string{}
		if agent.MCP != nil {
			mcpSrv = agent.MCP.Servers
		}
		logger.Debug("agent model chain",
			"name", name,
			"strategy", agent.Strategy,
			"models", modelEntries,
			"system_prompt", agent.SystemPromptFile,
			"mcp_servers", mcpSrv,
		)
	}
}

func resolveModelEntry(m config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider) []string {
	if m.Provider != "" && m.Model != "" {
		return []string{fmt.Sprintf("%s/%s", m.Provider, m.Model)}
	}
	if m.ProviderRgx != "" || m.ModelRgx != "" {
		return resolveRegexModelEntry(m, catalog, providers)
	}
	if m.Model != "" {
		return resolveStringModelEntry(m, catalog, providers)
	}
	return []string{}
}

func resolveRegexModelEntry(m config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider) []string {
	modelEntries := make([]string, 0)
	if catalog != nil {
		matched := false
		for _, dm := range catalog.AllModels() {
			if !m.MatchesCatalog(dm.Provider, dm.ID) {
				continue
			}
			if !providerCanServe(providers[dm.Provider]) {
				continue
			}
			matched = true
			modelEntries = append(modelEntries, fmt.Sprintf("%s/%s", dm.Provider, dm.ID))
		}
		if matched {
			return modelEntries
		}
	}
	return []string{fmt.Sprintf("rx:%s (unresolved)", m.ModelRgx)}
}

func resolveStringModelEntry(m config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider) []string {
	if catalog != nil {
		entries := catalog.LookupAll(m.Model)
		if len(entries) > 0 {
			modelEntries := make([]string, 0, len(entries))
			for _, e := range entries {
				if providerCanServe(providers[e.Provider]) {
					modelEntries = append(modelEntries, fmt.Sprintf("%s/%s", e.Provider, m.Model))
				}
			}
			if len(modelEntries) > 0 {
				return modelEntries
			}
		}
	}
	if entry, ok := config.ModelRegistry[m.Model]; ok {
		if providerCanServe(providers[entry.Provider]) {
			return []string{fmt.Sprintf("%s/%s", entry.Provider, m.Model)}
		}
	}
	return []string{fmt.Sprintf("%s (unresolved)", m.Model)}
}

func (g *NenyaGateway) InitMetrics() {
	g.Metrics = infra.NewMetrics()
	g.Metrics.RateLimits = g.RateLimiter.Snapshot
	g.Metrics.Cooldowns = g.AgentState.ActiveCooldowns
	g.Metrics.CBStates = g.AgentState.CBSnapshot
}

// CountTokens estimates the number of tokens in the given text using the
// cl100k_base BPE encoding. Returns 0 and logs a warning if tokenization
// fails (graceful degradation).
func (g *NenyaGateway) CountTokens(text string) int {
	n, err := tiktoken.CountTokens(text)
	if err != nil {
		g.Logger.Warn("tokenization failed, returning 0", "err", err)
		return 0
	}
	return n
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
		_ = client.Close()
		g.Logger.Debug("MCP client closed", "server", name)
	}
}

func (g *NenyaGateway) Reload(ctx context.Context, cfg config.Config, secrets *config.SecretsConfig) *NenyaGateway {
	newGW := New(ctx, cfg, secrets, g.Logger)

	newGW.Stats = g.Stats
	newGW.Metrics = g.Metrics
	newGW.ThoughtSigCache = g.ThoughtSigCache

	newGW.Metrics.RateLimits = newGW.RateLimiter.Snapshot
	newGW.Metrics.Cooldowns = newGW.AgentState.ActiveCooldowns
	newGW.Metrics.CBStates = newGW.AgentState.CBSnapshot

	g.Close()

	return newGW
}

// providerCanServe returns true if the provider is configured with either
// an API key or auth_style "none" (i.e. can actually make upstream requests).
func providerCanServe(p *config.Provider) bool {
	return p != nil && (p.APIKey != "" || p.AuthStyle == "none")
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

func (g *NenyaGateway) buildMCPToolIndex(ctx context.Context, logger *slog.Logger) {
	for name, client := range g.MCPClients {
		initCtx, cancel := contextWithTimeout(ctx, 10*time.Second)
		err := client.Initialize(initCtx)
		cancel()
		if err != nil {
			logger.Warn("MCP client initialization failed, skipping",
				"server", name, "err", err)
			continue
		}

		toolsCtx, cancel := contextWithTimeout(ctx, 15*time.Second)
		tools, err := client.RefreshTools(toolsCtx)
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

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
