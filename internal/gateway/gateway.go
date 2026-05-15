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
	"sync"
	"time"

	"nenya/config"
	"nenya/internal/adapter"
	"nenya/internal/auth"
	"nenya/internal/discovery"
	"nenya/internal/infra"
	"nenya/internal/mcp"
	"nenya/internal/pipeline"
	"nenya/internal/routing"
	"nenya/internal/security"
	"nenya/internal/tiktoken"
	"nenya/internal/util"
)

// NenyaGateway is the top-level container that holds all gateway components:
// configuration, HTTP clients, provider registry, MCP clients, metrics,
// rate limiter, caches, and the token counter.
type NenyaGateway struct {
	Config            config.Config
	Client            *http.Client
	OllamaClient      *http.Client
	Secrets           *config.SecretsConfig
	Providers         map[string]*config.Provider
	RateLimiter       *infra.RateLimiter
	SecretPatterns    []*regexp.Regexp
	BlockedPatterns   []*regexp.Regexp
	EntropyFilter     *pipeline.EntropyFilter
	Stats             *infra.UsageTracker
	Metrics           *infra.Metrics
	Logger            *slog.Logger
	AgentState        *routing.AgentState
	ThoughtSigCache   *infra.ThoughtSignatureCache
	ResponseCache     *infra.ResponseCache
	Embedder          infra.EmbeddingProvider
	MCPClients        map[string]*mcp.Client
	MCPToolIndex      *mcp.ToolRegistry
	ModelCatalog      *discovery.ModelCatalog
	HealthRegistry    *discovery.HealthRegistry
	LatencyTracker    *infra.LatencyTracker
	CostTracker       *infra.CostTracker
	AccountManager    *auth.AccountManager
	SecureMem         *security.SecureMem
	ClientTokenRef    security.SecureToken
	ProviderKeyTokens map[string]security.SecureToken
	tokMu             sync.RWMutex
}

// New creates a new NenyaGateway with the given configuration, secrets,
// and logger. It initializes HTTP clients, provider registry, rate limiter,
// metrics, MCP clients, and starts dynamic model discovery.
func New(ctx context.Context, cfg config.Config, secrets *config.SecretsConfig, logger *slog.Logger) *NenyaGateway {
	cfg = mergeBuiltInProviders(cfg)
	secureClient, ollamaClient := createHTTPClients(cfg)
	providers := config.ResolveProviders(&cfg, secrets)

	metrics := infra.NewMetrics()

	sm, clientTokenRef := initSecureMem(secrets, logger, cfg.Server.SecureMemoryRequired, metrics)
	providerKeyTokens := initProviderKeyTokens(sm, secrets, logger, metrics)

	sealSecureMem(sm, logger, metrics)

	keyProvider := buildKeyProvider(sm, providerKeyTokens, providers)

	mergedCatalog, healthRegistry := performModelDiscovery(ctx, &cfg, providers, metrics, logger, keyProvider)

	secretPatterns, blockedPatterns := compilePatterns(cfg, logger)
	entropyFilter := createEntropyFilter(cfg, logger)

	gw := buildGateway(cfg, secrets, secureClient, ollamaClient, providers,
		secretPatterns, blockedPatterns, entropyFilter, mergedCatalog, healthRegistry, logger, sm, clientTokenRef, providerKeyTokens, metrics)

	gw.Metrics = metrics
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot
	gw.Metrics.Cooldowns = gw.AgentState.ActiveCooldowns
	gw.Metrics.CBStates = gw.AgentState.CBSnapshot

	if gw.ResponseCache != nil {
		gw.Embedder = gw.ResponseCache.GetEmbedder()
	}

	gw.buildMCPToolIndex(ctx, logger)

	debugLogAgentModels(ctx, logger, cfg, mergedCatalog, providers)

	adapter.InitWithDeps(logger, gw.ThoughtSigCache, ExtractContentText)

	return gw
}

// buildKeyProvider creates a callback for retrieving provider API keys.
// The callback first checks secure memory (ProviderKeyTokens), then falls back
// to Provider.APIKey from config. This fallback is intentional: if a provider key
// cannot be stored securely (e.g., SecureMem not available), the gateway continues
// with reduced security rather than failing entirely. Operators should ensure
// secure_memory_required=true in production to enforce secure storage.
func buildKeyProvider(sm *security.SecureMem, providerKeyTokens map[string]security.SecureToken, providers map[string]*config.Provider) func(providerName string) ([]byte, bool) {
	return func(providerName string) ([]byte, bool) {
		if sm != nil && providerKeyTokens != nil {
			if ref, ok := providerKeyTokens[providerName]; ok {
				return sm.GetToken(ref)
			}
		}
		if provider, ok := providers[providerName]; ok {
			if provider.APIKey != "" {
				return []byte(provider.APIKey), true
			}
		}
		return nil, false
	}
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
		Timeout:   120 * time.Second,
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

func performModelDiscovery(ctx context.Context, cfg *config.Config, providers map[string]*config.Provider, metrics *infra.Metrics, logger *slog.Logger, keyProvider func(string) ([]byte, bool)) (*discovery.ModelCatalog, *discovery.HealthRegistry) {
	var mergedCatalog *discovery.ModelCatalog
	var healthRegistry *discovery.HealthRegistry

	if cfg.Discovery.Enabled == nil || !*cfg.Discovery.Enabled {
		mergedCatalog = discovery.MergeCatalog(discovery.NewModelCatalog(), cfg)
		return mergedCatalog, nil
	}

	fetcher := discovery.NewDiscoveryFetcher(cfg.Governance.EffectiveMaxRetryAttempts()).
		WithMetrics(metrics).
		WithKeyProvider(keyProvider)
	catalog := fetcher.FetchAll(ctx, providers, logger)
	mergedCatalog = discovery.MergeCatalog(catalog, cfg)

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

func generateAutoAgents(cfg *config.Config, catalog *discovery.ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) {
	enabled := cfg.Discovery.Enabled != nil && *cfg.Discovery.Enabled
	hasAutoAgents := cfg.Discovery.AutoAgents != nil && *cfg.Discovery.AutoAgents
	logger.Debug("auto-agents check", "discovery_enabled", enabled, "auto_agents", hasAutoAgents)
	if !hasAutoAgents {
		return
	}
	agents := discovery.GenerateAutoAgents(catalog, providers, cfg.Discovery.AutoAgentsConfig, logger)
	for name, agent := range agents {
		if _, exists := cfg.Agents[name]; !exists {
			cfg.Agents[name] = agent
		}
	}
}

// compilePatterns compiles security filter and blocked execution regexps.
// Pattern order matters: patterns are applied in config order, and the first
// matching pattern wins for redaction label assignment. Place more specific
// patterns before more general ones to ensure correct label attribution.
func compilePatterns(cfg config.Config, logger *slog.Logger) ([]*regexp.Regexp, []*regexp.Regexp) {
	var secretPatterns []*regexp.Regexp
	if cfg.Bouncer.Enabled != nil && *cfg.Bouncer.Enabled && len(cfg.Bouncer.RedactPatterns) > 0 {
		for _, pattern := range cfg.Bouncer.RedactPatterns {
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
	if !cfg.Bouncer.EntropyEnabled {
		return nil
	}
	entropyFilter := pipeline.NewEntropyFilter(
		cfg.Bouncer.EntropyThreshold,
		cfg.Bouncer.EntropyMinToken,
	)
	logger.Info("entropy filter enabled", "threshold", cfg.Bouncer.EntropyThreshold, "min_token", cfg.Bouncer.EntropyMinToken)
	return entropyFilter
}

func buildGateway(cfg config.Config, secrets *config.SecretsConfig, secureClient *http.Client, ollamaClient *http.Client, providers map[string]*config.Provider, secretPatterns []*regexp.Regexp, blockedPatterns []*regexp.Regexp, entropyFilter *pipeline.EntropyFilter, mergedCatalog *discovery.ModelCatalog, healthRegistry *discovery.HealthRegistry, logger *slog.Logger, sm *security.SecureMem, clientTokenRef security.SecureToken, providerKeyTokens map[string]security.SecureToken, metrics *infra.Metrics) *NenyaGateway {
	var rpm, tpm int
	if cfg.Governance.RatelimitMaxRPM != nil {
		rpm = *cfg.Governance.RatelimitMaxRPM
	}
	if cfg.Governance.RatelimitMaxTPM != nil {
		tpm = *cfg.Governance.RatelimitMaxTPM
	}

	gw := &NenyaGateway{
		Config:            cfg,
		Client:            secureClient,
		OllamaClient:      ollamaClient,
		Secrets:           secrets,
		Providers:         providers,
		RateLimiter:       infra.NewRateLimiter(rpm, tpm),
		SecretPatterns:    secretPatterns,
		BlockedPatterns:   blockedPatterns,
		EntropyFilter:     entropyFilter,
		Stats:             infra.NewUsageTracker(),
		Metrics:           nil,
		Logger:            logger,
		AgentState:        nil,
		ThoughtSigCache:   infra.NewThoughtSignatureCache(1000, 30*time.Minute),
		ResponseCache:     newResponseCache(cfg, logger, metrics),
		Embedder:          nil,
		MCPClients:        buildMCPClients(cfg, logger),
		MCPToolIndex:      mcp.NewToolRegistry(),
		ModelCatalog:      mergedCatalog,
		HealthRegistry:    healthRegistry,
		LatencyTracker:    infra.NewLatencyTracker(),
		CostTracker:       infra.NewCostTracker(),
		SecureMem:         sm,
		ClientTokenRef:    clientTokenRef,
		ProviderKeyTokens: providerKeyTokens,
	}
	gw.AgentState = routing.NewAgentStateWithConfig(logger, metrics, &cfg.Governance)
	// Account pool initialized with nil storage — backoff levels and cooldowns
	// are not persisted across restarts. Wire JSONFileStorage here when needed.
	gw.AccountManager = auth.NewAccountManager(nil)
	for name, pcfg := range cfg.Providers {
		accounts := auth.ToProviderAccounts(&pcfg)
		if len(accounts) > 0 {
			pool := auth.NewAccountPool(name, accounts)
			gw.AccountManager.RegisterPool(name, pool)
		}
	}
	return gw
}

func initSecureMem(secrets *config.SecretsConfig, logger *slog.Logger, secureMemoryRequired *bool, metrics *infra.Metrics) (*security.SecureMem, security.SecureToken) {
	if secrets == nil || secrets.ClientToken == "" {
		return nil, security.SecureToken{}
	}
	numKeys := 1 + len(secrets.ApiKeys)
	numProviderKeys := len(secrets.ProviderKeys)
	sm, err := security.NewSecureMem(security.TokenSizeHint(numKeys, numProviderKeys))
	if err != nil {
		if metrics != nil {
			metrics.RecordSecureMemInitFailure()
		}
		if secureMemoryRequired != nil && *secureMemoryRequired {
			logger.Error("secure memory unavailable but secure_memory_required is set",
				"err", err,
				"hint", "see docs/SECURITY.md for platform-specific mlock configuration")
			return nil, security.SecureToken{}
		}
		logger.Warn("secure memory unavailable, falling back to heap storage", "err", err)
		return nil, security.SecureToken{}
	}
	ref, err := sm.StoreToken(secrets.ClientToken)
	if err != nil {
		logger.Warn("failed to store client token in secure memory", "err", err)
		sm.Destroy()
		return nil, security.SecureToken{}
	}
	return sm, ref
}

func sealSecureMem(sm *security.SecureMem, logger *slog.Logger, metrics *infra.Metrics) {
	if sm == nil {
		return
	}
	if err := sm.Seal(); err != nil {
		if metrics != nil {
			metrics.RecordSecureMemSealFailure()
		}
		logger.Warn("failed to seal secure memory", "err", err)
		return
	}
	logger.Debug("secure memory sealed (read-only)")
}

func initProviderKeyTokens(sm *security.SecureMem, secrets *config.SecretsConfig, logger *slog.Logger, metrics *infra.Metrics) map[string]security.SecureToken {
	if sm == nil || secrets == nil || len(secrets.ProviderKeys) == 0 {
		return nil
	}
	tokens := make(map[string]security.SecureToken, len(secrets.ProviderKeys))
	skipped := 0
	for name, key := range secrets.ProviderKeys {
		if key == "" {
			continue
		}
		ref, err := sm.StoreToken(key)
		if err != nil {
			logger.Warn("failed to store provider key in secure memory (provider skipped)", "provider", name, "err", err)
			skipped++
			continue
		}
		tokens[name] = ref
	}
	if len(tokens) > 0 {
		logger.Info("provider API keys stored in secure memory", "count", len(tokens))
	}
	if skipped > 0 {
		logger.Warn("some provider keys were not stored in secure memory", "skipped", skipped)
	}
	return tokens
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
		for _, dm := range catalog.AllModels() {
			if !m.MatchesCatalog(dm.Provider, dm.ID) {
				continue
			}
			if !util.ProviderCanServe(providers[dm.Provider]) {
				continue
			}
			modelEntries = append(modelEntries, fmt.Sprintf("%s/%s", dm.Provider, dm.ID))
		}
		if len(modelEntries) > 0 {
			return modelEntries
		}
	}
	registryModels := util.FindRegistryModels(m, providers)
	for _, rm := range registryModels {
		modelEntries = append(modelEntries, fmt.Sprintf("%s/%s", rm.Provider, rm.Model))
	}
	if len(modelEntries) > 0 {
		return modelEntries
	}
	return []string{fmt.Sprintf("rx:%s (unresolved)", m.ModelRgx)}
}

func resolveStringModelEntry(m config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider) []string {
	if catalog != nil {
		if entries := lookupStringModelInCatalog(m, catalog, providers); entries != nil {
			return entries
		}
	}
	if entry, ok := config.ModelRegistry[m.Model]; ok && util.ProviderCanServe(providers[entry.Provider]) {
		return []string{fmt.Sprintf("%s/%s", entry.Provider, m.Model)}
	}
	return []string{fmt.Sprintf("%s (unresolved)", m.Model)}
}

func lookupStringModelInCatalog(m config.AgentModel, catalog *discovery.ModelCatalog, providers map[string]*config.Provider) []string {
	entries := catalog.LookupAll(m.Model)
	if len(entries) == 0 {
		return nil
	}
	modelEntries := make([]string, 0, len(entries))
	for _, e := range entries {
		if util.ProviderCanServe(providers[e.Provider]) {
			modelEntries = append(modelEntries, fmt.Sprintf("%s/%s", e.Provider, m.Model))
		}
	}
	if len(modelEntries) == 0 {
		return nil
	}
	return modelEntries
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
				sb.WriteString(extractContentFromPart(part))
			}
		}
		return sb.String()
	default:
		return ""
	}
}

func extractContentFromPart(part map[string]interface{}) string {
	typ, ok := part["type"].(string)
	if !ok {
		return ""
	}
	switch typ {
	case "text":
		return extractTextFromContentPart(part)
	case "image_url":
		return "[image]"
	case "input_json":
		return extractInputJSONFromPart(part)
	default:
		return ""
	}
}

func extractTextFromContentPart(part map[string]interface{}) string {
	if text, ok := part["text"].(string); ok {
		return text
	}
	return ""
}

func extractInputJSONFromPart(part map[string]interface{}) string {
	if input, ok := part["input_json"]; ok {
		if input == nil {
			return ""
		}
		if jsonBytes, err := json.Marshal(input); err == nil {
			return string(jsonBytes)
		}
	}
	return ""
}

func newResponseCache(cfg config.Config, logger *slog.Logger, metrics *infra.Metrics) *infra.ResponseCache {
	if cfg.ResponseCache.Enabled == nil || !*cfg.ResponseCache.Enabled {
		return nil
	}
	rc := cfg.ResponseCache

	var embedder infra.EmbeddingProvider
	if rc.EnableSemantic {
		// Create an HTTP client with a reasonable timeout for embedding requests.
		embedder = infra.NewOllamaEmbedder(&http.Client{
			Timeout: 10 * time.Second,
		}, rc.EmbeddingModel, rc.EmbeddingURL)
	}

	cache := infra.NewResponseCache(
		rc.MaxEntries,
		rc.MaxEntryBytes,
		time.Duration(rc.TTLSeconds)*time.Second,
		time.Duration(rc.EvictEverySeconds)*time.Second,
		metrics,
		rc.EnableSemantic,
		rc.SimilarityThreshold,
		embedder,
	)
	logger.Info("response cache enabled",
		"max_entries", rc.MaxEntries,
		"max_entry_bytes", rc.MaxEntryBytes,
		"ttl_seconds", rc.TTLSeconds,
		"evict_every_seconds", rc.EvictEverySeconds,
		"semantic_enabled", rc.EnableSemantic,
		"similarity_threshold", rc.SimilarityThreshold,
		"embedding_model", rc.EmbeddingModel,
		"embedding_url", rc.EmbeddingURL)

	return cache
}

// Close cleans up gateway resources: response cache, MCP clients,
// and secure memory. Waits for in-flight MCP operations to complete.
func (g *NenyaGateway) Close() {
	if g.ResponseCache != nil {
		g.ResponseCache.Stop()
	}
	for name, client := range g.MCPClients {
		_ = client.Close()
		g.Logger.Debug("MCP client closed", "server", name)
	}
	if g.SecureMem != nil {
		g.SecureMem.Destroy()
		g.ProviderKeyTokens = nil
		g.ClientTokenRef = security.SecureToken{}
	}
}

// Shutdown gracefully shuts down the gateway with a context timeout.
// It waits for in-flight MCP operations to complete and cleans up resources.
func (g *NenyaGateway) Shutdown(ctx context.Context) error {
	g.Logger.Info("starting graceful shutdown")

	done := make(chan struct{})
	go func() {
		g.Close()
		close(done)
	}()

	select {
	case <-done:
		g.Logger.Info("graceful shutdown completed")
		return nil
	case <-ctx.Done():
		g.Logger.Warn("graceful shutdown timed out", "err", ctx.Err())
		return ctx.Err()
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
		if g.Metrics != nil {
			client.SetGatewayMetrics(g.Metrics)
		}

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

func (g *NenyaGateway) GetProviderAPIKey(providerName string) ([]byte, bool) {
	if g.SecureMem != nil {
		g.tokMu.RLock()
		ref, ok := g.ProviderKeyTokens[providerName]
		g.tokMu.RUnlock()
		if ok {
			keyBytes, ok := g.SecureMem.GetToken(ref)
			return keyBytes, ok
		}
	}
	if provider, ok := g.Providers[providerName]; ok {
		if provider.APIKey != "" {
			return []byte(provider.APIKey), true
		}
	}
	return nil, false
}

// GetProviderAPIKeyForModel returns an API key for the given provider and model.
// It checks the multi-account pool first (selecting the least-recently-used account),
// then falls back to the legacy single-key path.
func (g *NenyaGateway) GetProviderAPIKeyForModel(ctx context.Context, providerName, model string) ([]byte, bool) {
	if g.AccountManager != nil {
		selected, err := g.AccountManager.SelectCredential(ctx, providerName, model)
		if err == nil && selected != "" {
			return []byte(selected), true
		}
	}
	return g.GetProviderAPIKey(providerName)
}

func (g *NenyaGateway) GetProvidersMap() map[string]*config.Provider {
	return g.Providers
}

func (g *NenyaGateway) ProviderHasAPIKey(providerName string) bool {
	if g.SecureMem != nil {
		g.tokMu.RLock()
		_, ok := g.ProviderKeyTokens[providerName]
		g.tokMu.RUnlock()
		if ok {
			return true
		}
	}
	if provider, ok := g.Providers[providerName]; ok {
		return provider.APIKey != "" || provider.AuthStyle == "none"
	}
	return false
}

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}
