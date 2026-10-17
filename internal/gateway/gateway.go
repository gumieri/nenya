package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/auth"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/local"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/security"
	"github.com/nenya/internal/tiktoken"
	"github.com/nenya/internal/util"
)

var _ routing.AccountSelector = (*NenyaGateway)(nil)
var _ billing.AccountLister = (*NenyaGateway)(nil)

// warnModelsMissingMaxContext logs warnings for providers with models that lack MaxContext.
// When MaxContext is unknown, proactive truncation is disabled. The full payload is
// sent upstream; if the provider returns context_length_exceeded, Nenya will
// automatically retry with summarization (if AutoRetryOnContextLimitEnabled).
func warnModelsMissingMaxContext(logger *slog.Logger, providers map[string]*config.Provider, catalog *discovery.ModelCatalog) {
	providersMissingMaxContext := make(map[string][]string)

	for _, m := range catalog.AllModels() {
		if m.MaxContext <= 0 {
			providersMissingMaxContext[m.Provider] = append(providersMissingMaxContext[m.Provider], m.ID)
		}
	}

	for provider, models := range providersMissingMaxContext {
		if provider == "" {
			provider = "static config"
		}
		modelList := util.JoinBackticks(models)
		logger.Warn("provider has models without max_context configured — proactive truncation disabled; upstream may return context_length_exceeded (retries with summarization will be attempted)",
			"provider", provider,
			"models", modelList,
			"count", len(models))
	}
}

// NenyaGateway is the top-level container that holds all gateway components:
// configuration, HTTP clients, provider registry, MCP clients, metrics,
// rate limiter, caches, and the token counter.
type NenyaGateway struct {
	Config       config.Config
	Client       *http.Client
	OllamaClient *http.Client
	// ProviderClients maps provider name to a dedicated HTTP client with a
	// non-default response-header timeout (transport-level TTFB bound).
	// Populated in New; providers absent here share Client via ClientFor.
	ProviderClients map[string]*http.Client
	Secrets         *config.SecretsConfig
	Providers       map[string]*config.Provider
	RateLimiter     *infra.RateLimiter
	// KeyUsage enforces per-API-key rate limits and token budgets, plus
	// per-provider daily budget tiers (NENYA-20). In-memory by design.
	KeyUsage           *auth.KeyUsageTracker
	ConcurrencyLimiter *infra.ConcurrencyLimiter
	SecretPatterns     []*regexp.Regexp
	BlockedPatterns    []*regexp.Regexp
	EntropyFilter      *pipeline.EntropyFilter
	Stats              *infra.UsageTracker
	// Calibration learns per-model cl100k drift ratios from real upstream
	// usage (NENYA-135); nil means the loop is disabled and estimates are
	// raw cl100k. Nil-safe methods everywhere.
	Calibration *infra.CalibrationTracker
	Metrics     *infra.Metrics
	Logger      *slog.Logger
	// tokenFallbackWarn gates the single warning emitted when the embedded
	// BPE vocabulary cannot be loaded (NENYA-157).
	tokenFallbackWarn sync.Once
	AgentState        *routing.AgentState
	ThoughtSigCache   *infra.ThoughtSignatureCache
	// WindowSummaries caches window-compaction engine summaries so the
	// compacted head stays stable across turns (NENYA-24).
	WindowSummaries *pipeline.SummaryCache
	// WindowHeads caches the deterministic window "tfidf" head by
	// conversation lineage so the compacted prefix stays byte-identical
	// across appended turns.
	WindowHeads *pipeline.WindowHeadCache
	// TfidfSelections freezes the TF-IDF interceptor's block selections so a
	// message's keep/drop decision is stable across turns even as the
	// prior-messages query evolves.
	TfidfSelections *pipeline.TfidfSelectionCache
	// WindowFidelityGate optionally judges freshly generated window
	// summaries (governance.judgments.summary_fidelity.enabled); built
	// at chain-build time, nil disables the gate. Consumed by
	// buildWindowDeps in the proxy layer.
	WindowFidelityGate *pipeline.Judge
	// EgressScreenJudge optionally adjudicates flagged egress content
	// (governance.judgments.egress_screen.enabled); built at chain-build
	// time, nil disables the screen. Consumed by egressScreenFor in the
	// proxy layer.
	EgressScreenJudge *pipeline.Judge
	ResponseCache     *infra.ResponseCache
	Embedder          infra.EmbeddingProvider
	MCPClients        map[string]*mcp.Client
	MCPToolIndex      *mcp.ToolRegistry
	// mcpUnavailable records configured MCP servers that are NOT usable,
	// keyed by server name, with the reason (NENYA-183): a server missing a
	// declared required header (recorded at construction) or one that refused
	// the configured credential (401/403, recorded at construction or at
	// runtime when a call is rejected). A request whose agent selects such a
	// server fails closed instead of silently running without its tools. A
	// successful call clears the entry, so a recovered credential re-enables
	// the server without a reload.
	mcpUnavailableMu   sync.RWMutex
	mcpUnavailable     map[string]mcpUnavailableEntry
	ModelCatalog       *discovery.ModelCatalog
	HealthRegistry     *discovery.HealthRegistry
	LatencyTracker     *infra.LatencyTracker
	CostTracker        *infra.CostTracker
	BillingTracker     *billing.BillingTracker
	QuotaFetcher       *billing.QuotaFetcher
	AccountManager     *auth.AccountManager
	SecureMem          *security.SecureMem
	ClientTokenRef     security.SecureToken
	ProviderKeyTokens  map[string]security.SecureToken
	LocalEngineManager *local.EngineManager
	InterceptorChain   *pipeline.InterceptorChain
	// MCPSecurityChain is the deterministic security subset of
	// InterceptorChain (redact/entropy/injection tier-1): re-run by the MCP
	// multi-turn loop on tool results appended mid-conversation, which never
	// pass through the request-time chain (NENYA-136). Nil when the main
	// chain was never built.
	MCPSecurityChain *pipeline.InterceptorChain
	SessionRouter    *routing.SessionRouter
	tokMu            sync.RWMutex
}

// New creates a new NenyaGateway with the given configuration, secrets,
// and logger. It initializes HTTP clients, provider registry, rate limiter,
// metrics, MCP clients, and starts dynamic model discovery.
func New(ctx context.Context, cfg config.Config, secrets *config.SecretsConfig, logger *slog.Logger) *NenyaGateway {
	cfg = mergeBuiltInProviders(cfg)
	// The fleet network policy is resolved (and its CA bundle read) once
	// per start and threaded to every transport construction site.
	global, policyErr := globalNetworkPolicy(cfg.Network)
	if policyErr != nil {
		logger.Error("fleet network policy invalid — transports fall back to fleet defaults (per-provider overrides still honored)", "err", policyErr)
		global = networkPolicy{}
	}
	secureClient, ollamaClient, baseTransport := createHTTPClients(cfg, global, logger)

	timeout := cfg.Governance.EffectiveUpstreamTimeout()
	if timeout == 0 {
		idle := cfg.Governance.EffectiveStreamIdleTimeout()
		if idle == 0 {
			logger.Warn("upstream timeout and stream idle timeout are both unlimited (0) — no stall detection")
		} else {
			logger.Warn("upstream client timeout is unlimited (governance.upstream_timeout_seconds=0), relying on stream idle timeout", "idle_timeout", idle)
		}
	} else {
		logger.Info("upstream client timeout configured", "timeout", timeout, "timeout_h", timeout.Hours(), "config_field", "governance.upstream_timeout_seconds")
	}

	providers := config.ResolveProviders(&cfg, secrets)

	metrics := infra.NewMetrics()
	// NENYA-131: with telemetry_unauthenticated the /metrics scrape is
	// unauthenticated too, so auth metrics must not carry API key names.
	metrics.SetAuthLabelRedaction(cfg.Server.TelemetryUnauthenticated)

	sm, clientTokenRef := initSecureMem(secrets, logger, cfg.Server.SecureMemoryRequired, metrics)
	providerKeyTokens := initProviderKeyTokens(sm, secrets, logger, metrics)

	sealSecureMem(sm, logger, metrics)

	keyProvider := buildKeyProvider(sm, providerKeyTokens, providers)

	// Discovery fetches keep the fetcher's tight timeouts (5s dial/TLS,
	// 10s response header) instead of the dispatch defaults: a stalled
	// provider endpoint must not hang its discovery goroutine for the full
	// dispatch window. The fleet policy (CA/proxy/TLS floor) is preserved
	// by cloning the policy-applied base transport.
	discoveryTransport := baseTransport.Clone()
	discoveryTransport.DialContext = (&net.Dialer{
		Timeout:   5 * time.Second,
		KeepAlive: 5 * time.Second,
	}).DialContext
	discoveryTransport.TLSHandshakeTimeout = 5 * time.Second
	discoveryTransport.ResponseHeaderTimeout = 10 * time.Second
	discoveryTransport.IdleConnTimeout = 10 * time.Second
	discoveryClient := &http.Client{Transport: discoveryTransport}
	mergedCatalog, healthRegistry := performModelDiscovery(ctx, &cfg, providers, discoveryDeps{
		discoveryClient: discoveryClient,
		ollamaClient:    ollamaClient,
		metrics:         metrics,
		keyProvider:     keyProvider,
		logger:          logger,
	})

	secretPatterns, blockedPatterns := compilePatterns(cfg, logger)
	entropyFilter := createEntropyFilter(cfg, logger)

	gw := buildGateway(cfg, secrets, secureClient, ollamaClient, providers,
		secretPatterns, blockedPatterns, entropyFilter, mergedCatalog, healthRegistry, logger, sm, clientTokenRef, providerKeyTokens, metrics)
	gw.ProviderClients = buildProviderClients(baseTransport, providers, global, cfg.Network, logger)

	gw.Metrics = metrics
	gw.Metrics.RateLimits = gw.RateLimiter.Snapshot
	gw.Metrics.Cooldowns = gw.AgentState.ActiveCooldowns
	gw.Metrics.CBStates = gw.AgentState.CBSnapshot
	gw.Metrics.SessionActive = gw.AgentState.SessionRouter.Active

	// Service-lifetime context: New() often receives a bounded startup
	// context (main passes a 60s budget), but quota polling must run for
	// the whole process lifetime. WithoutCancel detaches cancellation
	// while keeping context values; QuotaFetcher.Stop (via Close during
	// Shutdown) is the only thing that ends polling.
	gw.QuotaFetcher.Start(context.WithoutCancel(ctx), gw.BillingTracker, cfg.Providers, secrets, gw)

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
	// The provider-agnostic non_chat_models defaults are a semantic default,
	// not a file-loading detail: apply them here too so a Config built in code
	// (not passed through config.ApplyDefaults) still classifies decision-only
	// models as non-chat (NENYA-180). Idempotent.
	config.ApplyNonChatDefaults(&cfg)
	return cfg
}

// createHTTPClients returns the shared secure upstream client, the Ollama
// client, and the base upstream transport (used to derive per-provider
// transports in buildProviderClients). The base and Ollama transports carry
// the fleet-wide network policy (NENYA-137): explicit TLS floor, the global
// CA bundle, and the egress proxy (explicit or environment). An invalid
// policy is a startup-contract violation caught by config validation before
// this runs — New logs and degrades to system defaults + proxy environment
// rather than aborting a direct-built construction.
func createHTTPClients(cfg config.Config, global networkPolicy, logger *slog.Logger) (*http.Client, *http.Client, *http.Transport) {
	transport := newUpstreamTransport(config.DefaultResponseHeaderTimeoutSeconds * time.Second)
	global.apply(transport)

	secureClient := &http.Client{
		Transport: transport,
		// No client-level Timeout. ResponseHeaderTimeout bounds header reading,
		// and stream idle timeout (stall reader) bounds streaming stalls.
		// Non-streaming dispatches use per-request ctx deadlines.
	}

	ollamaResponseHeaderTimeout := 30 * time.Second
	var ollamaProviderConfig *config.ProviderConfig
	if ollamaCfg, ok := cfg.Providers["ollama"]; ok {
		ollamaProviderConfig = &ollamaCfg
		if ollamaCfg.TimeoutSeconds > 0 {
			ollamaResponseHeaderTimeout = time.Duration(ollamaCfg.TimeoutSeconds) * time.Second
		}
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
	// Ollama is typically loopback: the fleet-wide proxy is NOT inherited
	// (it would route local engines through the corporate egress). Explicit
	// providers.ollama.ca_bundle/proxy_url overrides still apply, and the
	// TLS floor always does. The ollama CA bundle is loaded here (the
	// fleet-level pool was loaded for the global policy).
	ollamaPolicy := networkPolicy{}
	if ollamaProviderConfig != nil {
		ollamaPolicy = networkPolicy{caBundlePath: ollamaProviderConfig.CABundle, proxyURL: ollamaProviderConfig.ProxyURL}
		if ollamaPolicy.caBundlePath == "" {
			// Fleet-wide CA inheritance is loopback-safe (extra root trust);
			// only the proxy is explicit-only for ollama.
			ollamaPolicy.caBundlePath = global.caBundlePath
			ollamaPolicy.pool = global.pool
		}
	}
	ollamaPolicy = ollamaPolicy.loadPool(logger)
	ollamaPolicy.apply(ollamaTransport)
	ollamaClient := &http.Client{
		Transport: ollamaTransport,
	}

	return secureClient, ollamaClient, transport
}

// newUpstreamTransport builds the shared upstream HTTP transport with the
// given response-header timeout (time-to-first-byte bound).
func newUpstreamTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}
}

// EvictIdleConnections drops the named provider's pooled idle connections.
// Called on quota exhaustion (429-class failures): an exhausted account's
// pooled connections are dead weight and providers frequently RST them at
// their keep-alive horizons anyway (NENYA-47). Safe to call on the shared
// base client — idle connections are cheap to re-establish. CloseIdleConnections
// is invoked without the gateway lock held (the caller must not hold locks
// that the transport's connection cleanup could interact with).
func (g *NenyaGateway) EvictIdleConnections(providerName string) {
	client := g.ClientFor(providerName)
	if client == nil {
		return
	}
	client.CloseIdleConnections()
}

// ClientFor returns the HTTP client for dispatching requests to the named
// provider: a dedicated per-provider client (built when the provider's
// effective response-header timeout, idle-connection timeout, or network
// policy differs from the fleet defaults, or it declares explicit
// ca_bundle/proxy_url) takes precedence; ollama-format providers without a
// dedicated client fall back to the shared OllamaClient; everything else
// shares the fleet client. Never nil.
func (g *NenyaGateway) ClientFor(providerName string) *http.Client {
	// Dedicated per-provider clients (timeout or network-policy overrides,
	// NENYA-137) take precedence — including for ollama-format providers,
	// whose dedicated client carries their explicit-only CA/proxy policy.
	if client, ok := g.ProviderClients[providerName]; ok {
		return client
	}
	if provider, ok := g.Providers[providerName]; ok && isOllamaProvider(providerName, provider) {
		return g.OllamaClient
	}
	return g.Client
}

// discoveryDeps groups the collaborators performModelDiscovery needs for
// its outbound fetches (NENYA-137): the fleet-policy client and the
// ollama-specific client (explicit-only policy, loopback-safe) so every
// discovery fetch traverses the same network path as runtime dispatches.
type discoveryDeps struct {
	discoveryClient *http.Client
	ollamaClient    *http.Client
	metrics         *infra.Metrics
	keyProvider     func(string) ([]byte, bool)
	logger          *slog.Logger
}

// recordNonChatDrops logs a discovery drop report and turns it into the
// nenya_non_chat_models_dropped_total counter (NENYA-180). The metric method is
// nil-safe, so a metrics-less gateway is fine.
func recordNonChatDrops(logger *slog.Logger, metrics *infra.Metrics, drops discovery.NonChatDrops) {
	for provider, models := range drops {
		if logger != nil {
			logger.Debug("non_chat_models excluded models from the chat catalog", "provider", provider, "models", models)
		}
		metrics.RecordNonChatModelsDropped(provider, len(models))
	}
}

func performModelDiscovery(ctx context.Context, cfg *config.Config, providers map[string]*config.Provider, deps discoveryDeps) (*discovery.ModelCatalog, *discovery.HealthRegistry) {
	var mergedCatalog *discovery.ModelCatalog
	var healthRegistry *discovery.HealthRegistry

	if cfg.Discovery.Enabled == nil || !*cfg.Discovery.Enabled {
		base := discovery.NewModelCatalog()
		recordNonChatDrops(deps.logger, deps.metrics, discovery.ApplyProviderNonChatToCatalog(base, cfg.Providers))
		var drops discovery.NonChatDrops
		mergedCatalog, drops = discovery.MergeCatalogWithDrops(base, cfg)
		recordNonChatDrops(deps.logger, deps.metrics, drops)
		return mergedCatalog, nil
	}

	fetcher := discovery.NewDiscoveryFetcher(cfg.Governance.EffectiveMaxRetryAttempts(), deps.discoveryClient).
		WithMetrics(deps.metrics).
		WithKeyProvider(deps.keyProvider).
		WithOllamaClient(deps.ollamaClient)
	catalog := fetcher.FetchAll(ctx, providers, deps.logger)
	// Belt and braces: the fetch filter above already dropped the classified
	// models, and the merge below would drop the same entries, but this
	// pre-filter keeps a catalog assembled by another component (the
	// pricing/describe personalities) consistent and gives the diagnostic a
	// place to count what it removes (NENYA-180).
	recordNonChatDrops(deps.logger, deps.metrics, discovery.ApplyProviderNonChatToCatalog(catalog, cfg.Providers))
	var drops discovery.NonChatDrops
	mergedCatalog, drops = discovery.MergeCatalogWithDrops(catalog, cfg)
	recordNonChatDrops(deps.logger, deps.metrics, drops)

	if _, hasOR := providers["openrouter"]; hasOR {
		fetchOpenRouterPricing(ctx, providers, mergedCatalog, deps.logger, deps.discoveryClient)
	}

	deps.logger.Info("model discovery completed", "total_models", len(mergedCatalog.AllModels()), "fetched_at", catalog.FetchedAt().Format(time.RFC3339))

	warnModelsMissingMaxContext(deps.logger, providers, mergedCatalog)

	healthRegistry = discovery.ValidateAllProviders(providers, mergedCatalog, deps.logger)

	generateAutoAgents(cfg, mergedCatalog, providers, deps.logger)

	return mergedCatalog, healthRegistry
}

func fetchOpenRouterPricing(ctx context.Context, providers map[string]*config.Provider, catalog *discovery.ModelCatalog, logger *slog.Logger, discoveryClient *http.Client) {
	pfCtx, pfCancel := context.WithTimeout(ctx, 20*time.Second)
	defer pfCancel()

	pf := discovery.NewPricingFetcher(logger, discoveryClient)
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

	// NENYA-183: build the MCP clients and record the servers that cannot be
	// used for a credential reason (missing declared required header), so a
	// request selecting one fails closed instead of silently dropping the
	// server's tools.
	mcpClients, unavailableReasons := buildMCPClients(cfg, logger)
	mcpUnavailable := make(map[string]mcpUnavailableEntry, len(unavailableReasons))
	for name, reason := range unavailableReasons {
		// A build-time refusal is an operator misconfiguration: it lasts
		// until the next reload rather than being re-probed.
		mcpUnavailable[name] = mcpUnavailableEntry{reason: reason, markedAt: time.Now(), permanent: true}
	}

	gw := &NenyaGateway{
		Config:             cfg,
		Client:             secureClient,
		OllamaClient:       ollamaClient,
		Secrets:            secrets,
		Providers:          providers,
		RateLimiter:        infra.NewRateLimiter(rpm, tpm),
		ConcurrencyLimiter: infra.NewConcurrencyLimiter(),
		KeyUsage:           auth.NewKeyUsageTracker(nil),
		SecretPatterns:     secretPatterns,
		BlockedPatterns:    blockedPatterns,
		EntropyFilter:      entropyFilter,
		Stats:              infra.NewUsageTracker(),
		Metrics:            nil,
		Logger:             logger,
		AgentState:         nil,
		ThoughtSigCache:    infra.NewThoughtSignatureCache(1000, 30*time.Minute),
		WindowSummaries:    pipeline.NewSummaryCache(windowSummaryCacheSize(cfg)),
		WindowHeads:        pipeline.NewWindowHeadCache(windowSummaryCacheSize(cfg)),
		TfidfSelections:    pipeline.NewTfidfSelectionCache(pipeline.DefaultTfidfSelectionCacheSize),
		ResponseCache:      newResponseCache(cfg, logger, metrics, ollamaClient),
		Embedder:           nil,
		MCPClients:         mcpClients,
		MCPToolIndex:       mcp.NewToolRegistry(),
		mcpUnavailable:     mcpUnavailable,
		ModelCatalog:       mergedCatalog,
		HealthRegistry:     healthRegistry,
		LatencyTracker:     infra.NewLatencyTracker(),
		CostTracker:        infra.NewCostTracker(),
		Calibration:        newCalibrationTracker(&cfg),
		BillingTracker:     billing.NewBillingTracker(logger, metrics),
		QuotaFetcher:       billing.NewQuotaFetcher(logger, secureClient),
		SecureMem:          sm,
		ClientTokenRef:     clientTokenRef,
		ProviderKeyTokens:  providerKeyTokens,
	}
	gw.AgentState = routing.NewAgentStateWithConfig(logger, metrics, &cfg.Governance, cfg.Agents)
	gw.SessionRouter = gw.AgentState.SessionRouter
	gw.AccountManager = auth.NewAccountManager(nil)
	for name, pcfg := range cfg.Providers {
		accounts := auth.ToProviderAccounts(&pcfg)
		if len(accounts) > 0 {
			pool := auth.NewAccountPool(name, accounts)
			gw.AccountManager.RegisterPool(name, pool)
		}

		if p, ok := providers[name]; ok {
			// Per-dimension resolution: an explicit provider value wins
			// (0 disables the dimension for this provider); nil inherits
			// the governance global (rpm/tpm resolved above).
			providerRPM := rpm
			providerTPM := tpm
			if pcfg.RatelimitMaxRPM != nil {
				providerRPM = max(0, *pcfg.RatelimitMaxRPM)
			}
			if pcfg.RatelimitMaxTPM != nil {
				providerTPM = max(0, *pcfg.RatelimitMaxTPM)
			}
			gw.RateLimiter.SetProviderLimits(name, infra.ProviderRateLimits{
				MaxRPM: providerRPM,
				MaxTPM: providerTPM,
			})
			logger.Debug("applied per-provider rate limits",
				"provider", name,
				"host", infra.ExtractHost(p.BaseURL),
				"rpm", providerRPM,
				"tpm", providerTPM)
		}
	}
	return gw
}

// EffectiveConcurrencyLimit resolves the in-flight request cap for a
// provider+model pair: per-model override first, then the provider-wide
// cap, then the governance global. An explicit model override of 0 means
// unlimited for that model. A return value of 0 means unlimited.
func (g *NenyaGateway) EffectiveConcurrencyLimit(providerName, model string) int {
	if pr, ok := g.Providers[providerName]; ok && pr != nil {
		if _, hasOverride := pr.ModelConcurrency[model]; hasOverride {
			return pr.ConcurrencyLimit(model)
		}
		if pr.MaxConcurrentRequests > 0 {
			return pr.MaxConcurrentRequests
		}
	}
	if g.Config.Governance.MaxConcurrentRequests > 0 {
		return g.Config.Governance.MaxConcurrentRequests
	}
	return 0
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
	return []string{m.Model + " (unresolved)"}
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
// cl100k_base BPE encoding. If the embedded vocabulary cannot be loaded the
// class-weighted fallback (tiktoken.EstimateByClass) answers instead and the
// failure is logged: a zero estimate would silently unbound every budget
// derived from it (NENYA-157).
func (g *NenyaGateway) CountTokens(text string) int {
	n, err := tiktoken.CountTokens(text)
	if err != nil {
		// One warning per gateway: a broken vocabulary would otherwise log
		// once per counted declaration, per request.
		g.tokenFallbackWarn.Do(func() {
			g.Logger.Warn("tokenization failed, using the class-weighted fallback", "err", err)
		})
		return tiktoken.EstimateByClass(text)
	}
	return n
}

// CalibratedCountTokens estimates tokens like CountTokens, then applies the
// model's learned cl100k drift ratio (NENYA-135): actual prompt tokens
// observed from the upstream usage payload divided by dispatched cl100k
// estimates, clamped and gated on a warm-up observation floor. Use this at
// budget-critical sites that know the target model (context-window
// trimming, per-target input budgets); raw CountTokens stays for
// model-agnostic call sites. Unobserved models calibrate to exactly 1.0.
func (g *NenyaGateway) CalibratedCountTokens(text string, model string) int {
	n := g.CountTokens(text)
	if n == 0 {
		return 0
	}
	ratio := g.Calibration.Ratio(model)
	if ratio == 1.0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		// Non-finite ratios (programmatically-built configs bypassing
		// validation) must not poison the conversion below (§7).
		return n
	}
	// math.Round (half away from zero) keeps the calibrated estimate from
	// silently shrinking on fractional ratios.
	calibrated := int(math.Round(float64(n) * ratio))
	if calibrated <= 0 {
		return n
	}
	return calibrated
}

// newCalibrationTracker builds the calibration tracker from governance
// config; a disabled loop yields nil (nil-safe everywhere).
func newCalibrationTracker(cfg *config.Config) *infra.CalibrationTracker {
	if params := newCalibrationParams(cfg); params != nil {
		return infra.NewCalibrationTracker(*params)
	}
	return nil
}

// newCalibrationParams resolves the governance knobs into tracker params;
// nil when the loop is disabled.
func newCalibrationParams(cfg *config.Config) *infra.CalibrationParams {
	enabled, minObs, decay, clampMin, clampMax := cfg.Governance.EffectiveTokenCalibrationParams()
	if !enabled {
		return nil
	}
	return &infra.CalibrationParams{
		Enabled:         true,
		MinObservations: minObs,
		Decay:           decay,
		ClampMin:        clampMin,
		ClampMax:        clampMax,
	}
}

// Framing overhead the message text does not carry: the role/delimiter tokens
// a provider wraps every message in. Providers document 3-4 tokens per message
// (OpenAI's chat framing), so the estimate adds one constant per message
// instead of under-counting a long conversation by a few tokens per turn
// (NENYA-157).
const perMessageOverheadTokens = 4

// perDeclarationOverheadTokens is the framing of one declaration — a tool or a
// structured-output schema (the name/description/JSON skeleton the provider
// renders around it), on top of its text.
const perDeclarationOverheadTokens = 8

// CountRequestTokens estimates the tokens a chat request costs its target: the
// message text (cl100k BPE), the message framing overhead, any image blocks
// (priced by pixel area, see tiktoken.ImageTokens), and the tool/structured-
// output declarations — all of which were previously invisible to the estimate
// (NENYA-157). It is the seed for budget-critical decisions (per-target input
// budgets, trimming, the pipeline's savings accounting), so an under-count
// there is what lets an oversized payload reach an upstream 400.
func (g *NenyaGateway) CountRequestTokens(payload map[string]interface{}) int {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		// No messages to price, but the declarations and any image they
		// carry are still forwarded upstream: returning zero here would
		// resurrect the zero-estimate hole this change closes (NENYA-157).
		return g.declarationTokens(payload)
	}
	var sb strings.Builder
	n := len(msgs)
	if n > math.MaxInt/500 {
		n = math.MaxInt / 500
	}
	sb.Grow(n * 500)
	images := 0
	framed := 0
	for _, msgRaw := range msgs {
		msg, ok := msgRaw.(map[string]interface{})
		if !ok {
			continue
		}
		framed++
		sb.WriteString(textForCount(msg))
		appendToolCallText(&sb, msg)
		images += imageTokensInMessage(msg)
	}
	tokens := g.CountTokens(sb.String())
	tokens += images
	tokens += framed * perMessageOverheadTokens
	tokens += g.declarationTokens(payload)
	return tokens
}

// textForCount is ExtractContentText without the "[image]" placeholder: an
// image is priced by pixel area, so counting its placeholder text as well would
// double-count it (the placeholder stays in ExtractContentText, where it is a
// useful marker for the TF-IDF and windowing consumers).
func textForCount(msg map[string]interface{}) string {
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
			part, ok := partRaw.(map[string]interface{})
			if !ok {
				continue
			}
			switch typ, _ := part["type"].(string); typ {
			case "text":
				sb.WriteString(extractTextFromContentPart(part))
			case "input_json":
				sb.WriteString(extractInputJSONFromPart(part))
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// appendToolCallText adds an assistant message's tool calls (name plus
// arguments) to the counted text: an assistant turn that called a tool carries
// a nil or empty content, so without this the MCP loop's re-dispatches — the
// exact shape the gateway itself builds — estimated as framing alone
// (NENYA-157).
func appendToolCallText(sb *strings.Builder, msg map[string]interface{}) {
	calls, ok := msg["tool_calls"].([]interface{})
	if !ok {
		return
	}
	for _, callRaw := range calls {
		call, ok := callRaw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := call["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok {
			sb.WriteString(name)
		}
		if args, ok := fn["arguments"].(string); ok {
			sb.WriteString(args)
		}
	}
}

// imageTokensInMessage prices the image blocks of one message, in either wire
// shape: OpenAI's `image_url` (a data URL or a remote URL) and Anthropic's
// `image` block with an inlined base64 source.
func imageTokensInMessage(msg map[string]interface{}) int {
	content, ok := msg["content"].([]interface{})
	if !ok {
		return 0
	}
	total := 0
	for _, partRaw := range content {
		part, ok := partRaw.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := part["type"].(string)
		switch typ {
		case "image_url":
			// The data URL carries its own media type; a remote URL has none.
			total += tiktoken.ImageTokens("", imageURLPayload(part["image_url"]))
		case "image":
			total += anthropicImageTokens(part)
		}
	}
	return total
}

// imageURLPayload extracts the URL string from an OpenAI image_url field, which
// may be a string or an object with a `url`.
func imageURLPayload(raw interface{}) string {
	switch value := raw.(type) {
	case string:
		return value
	case map[string]interface{}:
		url, _ := value["url"].(string)
		return url
	default:
		return ""
	}
}

// anthropicImageTokens prices an Anthropic `image` block
// (`{"source":{"type":"base64","media_type":...,"data":...}}`).
func anthropicImageTokens(part map[string]interface{}) int {
	source, ok := part["source"].(map[string]interface{})
	if !ok {
		return tiktoken.DefaultImageTokens
	}
	sourceType, _ := source["type"].(string)
	mediaType, _ := source["media_type"].(string)
	data, _ := source["data"].(string)
	switch sourceType {
	case "base64":
		return tiktoken.ImageTokens(mediaType, data)
	case "url":
		url, _ := source["url"].(string)
		return tiktoken.ImageTokens(mediaType, url)
	default:
		return tiktoken.DefaultImageTokens
	}
}

// declarationTokens prices the request's tool declarations and structured-output
// schema, which the message text does not cover. The serialized declaration is
// what the provider renders, so it is counted verbatim plus a framing constant;
// a declaration that cannot be marshaled still costs the framing.
func (g *NenyaGateway) declarationTokens(payload map[string]interface{}) int {
	total := 0
	if tools, ok := payload["tools"].([]interface{}); ok {
		for _, tool := range tools {
			total += perDeclarationOverheadTokens + g.CountTokens(marshalForCount(tool))
		}
	}
	if format, ok := payload["response_format"].(map[string]interface{}); ok {
		total += perDeclarationOverheadTokens + g.CountTokens(marshalForCount(format))
	}
	return total
}

// marshalForCount renders a declaration for counting. An unmarshalable value
// contributes no text (it is rejected elsewhere), never a panic.
func marshalForCount(value interface{}) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
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

func newResponseCache(cfg config.Config, logger *slog.Logger, metrics *infra.Metrics, ollamaClient *http.Client) *infra.ResponseCache {
	if cfg.ResponseCache.Enabled == nil || !*cfg.ResponseCache.Enabled {
		return nil
	}
	rc := cfg.ResponseCache

	var embedder infra.EmbeddingProvider
	if rc.EnableSemantic {
		// Embedding requests go to the (usually local) Ollama endpoint —
		// reuse the gateway's Ollama transport so the TLS floor and proxy
		// policy match every other transport, with a client-level timeout
		// for the synchronous embed call (NENYA-137). Known limitation: the
		// ollama transport never inherits the fleet proxy, so a remote
		// rc.EmbeddingURL behind a corporate egress proxy needs an explicit
		// providers.ollama.proxy_url.
		embedder = infra.NewOllamaEmbedder(&http.Client{
			Timeout:   10 * time.Second,
			Transport: ollamaClient.Transport,
		}, rc.EmbeddingModel, rc.EmbeddingURL)
	}

	cache := infra.NewResponseCache(
		rc.MaxEntries,
		rc.MaxEntryBytes,
		time.Duration(rc.TTLSeconds)*time.Second,
		time.Duration(rc.EvictEverySeconds)*time.Second,
		metrics,
		logger,
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
// quota fetcher, and secure memory. Waits for in-flight MCP operations
// to complete.
func (g *NenyaGateway) Close() {
	if g.ResponseCache != nil {
		g.ResponseCache.Stop()
	}
	if g.QuotaFetcher != nil {
		g.QuotaFetcher.Stop()
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
// It unloads pinned local-engine models, waits for in-flight MCP
// operations to complete, and cleans up resources.
func (g *NenyaGateway) Shutdown(ctx context.Context) error {
	g.Logger.Info("starting graceful shutdown")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if g.LocalEngineManager != nil {
			// Startup models are pinned with keep_alive=-1; unload them
			// (keep_alive=0) so they do not stay resident in Ollama after
			// the process exits. Errors are logged inside, never fatal.
			g.LocalEngineManager.Shutdown(ctx)
		}
		g.Close()
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
	// The carried-over Metrics instance predates this reload's config, so
	// re-apply the auth-label redaction decision (NENYA-131).
	newGW.Metrics.SetAuthLabelRedaction(cfg.Server.TelemetryUnauthenticated)
	newGW.ThoughtSigCache = g.ThoughtSigCache
	// Carry the TF-IDF selection memo across reloads: frozen selections are
	// keyed by content + selection parameters (not the judge engine identity),
	// so those unaffected by the new config keep the prompt-cache prefix stable
	// instead of re-freezing after a SIGHUP. (WindowSummaries is intentionally
	// rebuilt: engine summaries depend on the summary model, which a reload may
	// have changed.)
	newGW.TfidfSelections = g.TfidfSelections
	// WindowHeads is deterministic (lineage + covered-region keyed), so it is
	// safe to carry across reloads too: unaffected conversations keep their
	// frozen head rather than re-freezing once.
	newGW.WindowHeads = g.WindowHeads
	newGW.BillingTracker = g.BillingTracker
	// Calibration is per-process learned state (NENYA-135): carry the
	// learned sums across SIGHUP like Stats, but honor config — a disabled
	// loop drops the tracker, changed knobs re-parameterize it
	// (Reconfigured carries the sums under the new decay/clamps).
	if newParams := newCalibrationParams(&cfg); newParams != nil {
		newGW.Calibration = g.Calibration.Reconfigured(*newParams)
		if newGW.Calibration == nil {
			newGW.Calibration = infra.NewCalibrationTracker(*newParams)
		}
	}
	newGW.SessionRouter = g.SessionRouter
	newGW.AgentState.SessionRouter = g.SessionRouter
	newGW.AgentState.CountersMerge(g.AgentState.CountersCopy())

	newGW.Metrics.RateLimits = newGW.RateLimiter.Snapshot
	newGW.Metrics.Cooldowns = newGW.AgentState.ActiveCooldowns
	newGW.Metrics.CBStates = newGW.AgentState.CBSnapshot

	g.Close()

	return newGW
}

func buildMCPClients(cfg config.Config, logger *slog.Logger) (map[string]*mcp.Client, map[string]string) {
	clients := make(map[string]*mcp.Client)
	unavailable := make(map[string]string)
	for name, serverCfg := range cfg.MCPServers {
		if serverCfg.URL == "" {
			logger.Warn("MCP server has empty URL, skipping", "server", name)
			unavailable[name] = "no url configured"
			continue
		}
		if missing := missingRequiredHeader(serverCfg); missing != "" {
			logger.Error("MCP server missing a required credential header, refusing to connect",
				"server", name, "header", missing)
			unavailable[name] = "missing required header " + missing
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
	return clients, unavailable
}

// mcpUnavailableEntry is one unusable-server record. A permanent entry (an
// operator misconfiguration detected at build time) lasts until the next
// reload; a transient entry (a credential the server refused) expires after
// mcpCredentialRejectionCooldown so the server is re-probed instead of being
// latched out for the life of the process (NENYA-183).
type mcpUnavailableEntry struct {
	reason    string
	markedAt  time.Time
	permanent bool
}

// mcpCredentialRejectionCooldown bounds how long a runtime credential
// rejection keeps a server out of service before it is probed again. During
// the window requests that need the server fail closed. A var so tests can
// shorten the window.
var mcpCredentialRejectionCooldown = 60 * time.Second

// MarkMCPUnavailable records a permanent reason a server is unusable, so
// requests selecting it fail closed until the next reload (NENYA-183). It is
// used for conditions detected at construction (a declared required header
// that is missing, or a credential the server refused during the handshake):
// nothing re-probes a client that never became ready, so making those
// self-healing would admit requests that then run without the server's tools.
func (g *NenyaGateway) MarkMCPUnavailable(server, reason string) {
	g.markMCPUnavailable(server, reason, true)
}

// MCPCredentialRejectionCooldown reports how long a runtime credential
// rejection keeps a server out of service.
func MCPCredentialRejectionCooldown() time.Duration {
	return mcpCredentialRejectionCooldown
}

// MarkMCPCredentialRejected records that the server refused the configured
// credential at runtime. The mark expires after mcpCredentialRejectionCooldown
// so a rotated/restored credential recovers without a reload; a successful
// call clears it immediately.
func (g *NenyaGateway) MarkMCPCredentialRejected(server, reason string) {
	g.markMCPUnavailable(server, reason, false)
}

func (g *NenyaGateway) markMCPUnavailable(server, reason string, permanent bool) {
	if g == nil {
		return
	}
	g.mcpUnavailableMu.Lock()
	defer g.mcpUnavailableMu.Unlock()
	if g.mcpUnavailable == nil {
		g.mcpUnavailable = make(map[string]mcpUnavailableEntry)
	}
	if existing, ok := g.mcpUnavailable[server]; ok && existing.permanent && !permanent {
		// A runtime rejection must not downgrade an operator misconfiguration
		// into a self-healing mark.
		return
	}
	g.mcpUnavailable[server] = mcpUnavailableEntry{reason: reason, markedAt: time.Now(), permanent: permanent}
}

// ClearMCPUnavailable drops the record for a server: a successful call proves
// the credential works again, so the server is re-enabled without a reload.
func (g *NenyaGateway) ClearMCPUnavailable(server string) {
	if g == nil {
		return
	}
	g.mcpUnavailableMu.RLock()
	observed, recorded := g.mcpUnavailable[server]
	g.mcpUnavailableMu.RUnlock()
	if !recorded {
		// The common case: nothing to clear, so skip the write lock.
		return
	}
	g.mcpUnavailableMu.Lock()
	defer g.mcpUnavailableMu.Unlock()
	// Delete only the entry that was observed: a mark recorded between the
	// read and the lock (a fresh rejection) must survive, otherwise a success
	// on one call would silently re-enable a server another call just found
	// refusing the credential.
	if current, ok := g.mcpUnavailable[server]; ok && current.markedAt.Equal(observed.markedAt) {
		delete(g.mcpUnavailable, server)
	}
}

// MCPUnavailableReason returns the effective reason a server is unusable.
// Expired transient entries read as available (no write happens on this read
// path), which is what lets a recovered credential be re-probed; the stale
// entry is dropped by the next mark or clear.
func (g *NenyaGateway) MCPUnavailableReason(server string) (string, bool) {
	if g == nil {
		return "", false
	}
	g.mcpUnavailableMu.RLock()
	entry, ok := g.mcpUnavailable[server]
	g.mcpUnavailableMu.RUnlock()
	if !ok {
		return "", false
	}
	if !entry.permanent && time.Since(entry.markedAt) >= mcpCredentialRejectionCooldown {
		return "", false
	}
	return entry.reason, true
}

// MCPUnavailableSnapshot returns a copy of the effective unusable-server
// registry (expired transient entries excluded).
func (g *NenyaGateway) MCPUnavailableSnapshot() map[string]string {
	if g == nil {
		return nil
	}
	g.mcpUnavailableMu.RLock()
	names := make([]string, 0, len(g.mcpUnavailable))
	for name := range g.mcpUnavailable {
		names = append(names, name)
	}
	g.mcpUnavailableMu.RUnlock()
	var out map[string]string
	for _, name := range names {
		if reason, ok := g.MCPUnavailableReason(name); ok {
			if out == nil {
				out = make(map[string]string)
			}
			out[name] = reason
		}
	}
	return out
}

// missingRequiredHeader returns the name of the first declared required header
// that is absent or empty, or "" when all are present (NENYA-183).
func missingRequiredHeader(serverCfg config.MCPServerConfig) string {
	for _, header := range serverCfg.RequiredHeaders {
		if strings.TrimSpace(serverCfg.Headers[header]) == "" {
			return header
		}
	}
	return ""
}

func (g *NenyaGateway) buildMCPToolIndex(ctx context.Context, logger *slog.Logger) {
	// Register in a deterministic order: the first server to claim a namespaced
	// tool name keeps it, and map iteration would otherwise vary between starts
	// (NENYA-172).
	serverNames := make([]string, 0, len(g.MCPClients))
	for name := range g.MCPClients {
		serverNames = append(serverNames, name)
	}
	slices.Sort(serverNames)
	for _, name := range serverNames {
		client := g.MCPClients[name]
		if g.Metrics != nil {
			client.SetGatewayMetrics(g.Metrics)
		}

		initCtx, cancel := contextWithTimeout(ctx, 10*time.Second)
		err := client.Initialize(initCtx)
		cancel()
		if err != nil {
			if errors.Is(err, mcp.ErrAuthRejected) {
				// A declared/expected credential was refused: record it so a
				// request selecting this server fails closed (NENYA-183)
				// rather than running without the server's tools.
				logger.Error("MCP server rejected the configured credential",
					"server", name, "err", err)
				g.MarkMCPUnavailable(name, "credential rejected")
				continue
			}
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
	servers := g.AgentMCPServers(agentName)
	if len(servers) == 0 {
		return nil
	}
	clients := make(map[string]*mcp.Client, len(servers))
	for _, serverName := range servers {
		if client, ok := g.MCPClients[serverName]; ok {
			clients[serverName] = client
		}
	}
	if len(clients) == 0 {
		return nil
	}
	return clients
}

// AgentMCPServers returns the MCP server names an agent's configuration lists
// (nil for an unknown agent or an agent without MCP). It is the outer bound
// for tool injection and dispatch (NENYA-183).
func (g *NenyaGateway) AgentMCPServers(agentName string) []string {
	if g == nil {
		return nil
	}
	agent, ok := g.Config.Agents[agentName]
	if !ok || agent.MCP == nil {
		return nil
	}
	return agent.MCP.Servers
}

// AgentHasMCPServer reports whether an agent's MCP configuration lists a
// server. An unknown agent (or one without MCP) has none, so callers that gate
// dispatch on it fail closed.
func (g *NenyaGateway) AgentHasMCPServer(agentName, serverName string) bool {
	return slices.Contains(g.AgentMCPServers(agentName), serverName)
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

// selectAccountKey resolves an LRU-selected account credential for a
// provider/model. The pool is billing-agnostic, so after the LRU pick the
// selection is retried with a growing exclude set while the pick is
// billing-exhausted — bounded by pool size — so a healthy sibling is served
// instead of a target that filterExhaustedTargets would drop (spurious
// provider/model failover). When no healthy sibling exists the exhausted pick
// is returned; on the chat target-build path the downstream target filter
// remains the safety net, though other consumers of this credential (e.g.
// passthrough auth injection) have no such filter. Each skipped pick has its
// LastUsed bumped by the pool — a mild, self-correcting LRU distortion
// accepted to avoid extra pool API surface.
func (g *NenyaGateway) selectAccountKey(ctx context.Context, providerName, model string) ([]byte, string, bool) {
	if g.AccountManager == nil {
		return nil, "", false
	}
	selected, err := g.AccountManager.SelectAccount(ctx, providerName, model)
	if err == nil && selected != nil {
		selected = g.skipExhaustedPick(ctx, providerName, model, selected)
	}
	if err == nil && selected != nil {
		if g.Metrics != nil {
			status := "selected"
			if g.BillingTracker != nil && g.BillingTracker.IsExhausted(providerName, selected.ID) {
				// The pick is still exhausted (no healthy sibling): the
				// chat target-build filter will drop it, so keep the metric
				// honest about usable selections.
				status = "selected_exhausted_fallback"
			}
			g.Metrics.RecordAccountSelection(providerName, status)
		}
		return []byte(selected.Credential), selected.ID, true
	}
	if g.Metrics != nil {
		if err != nil {
			g.Metrics.RecordAccountSelection(providerName, "error")
		} else {
			g.Metrics.RecordAccountSelection(providerName, "none_available")
		}
	}
	return nil, "", false
}

// skipExhaustedPick advances the LRU pick past billing-exhausted accounts by
// retrying selection with a growing exclude set — bounded by pool size, so
// no loop is possible. When no healthy sibling exists the (possibly
// exhausted) pick is returned unchanged; on the chat target-build path the
// downstream target filter remains the full-exhaustion safety net. Each
// skipped pick has its LastUsed bumped by the pool — a mild, self-correcting
// LRU distortion accepted to avoid extra pool API surface.
func (g *NenyaGateway) skipExhaustedPick(ctx context.Context, providerName, model string, selected *auth.SelectedAccount) *auth.SelectedAccount {
	if g.BillingTracker == nil {
		return selected
	}
	exclude := map[string]bool{}
	for g.BillingTracker.IsExhausted(providerName, selected.ID) {
		exclude[selected.ID] = true
		sibling, serr := g.AccountManager.SelectAccountExcluding(ctx, providerName, model, exclude)
		if serr != nil || sibling == nil {
			if g.Logger != nil {
				g.Logger.Debug("lru pick billing-exhausted, no healthy sibling; target filter will decide",
					"provider", providerName, "model", model, "account", selected.ID)
			}
			break
		}
		if g.Logger != nil {
			g.Logger.Debug("lru pick billing-exhausted, using healthy sibling",
				"provider", providerName, "model", model, "from_account", selected.ID, "to_account", sibling.ID)
		}
		selected = sibling
	}
	return selected
}

// selectPreferredAccountKey resolves a specific account pinned by a sticky
// session. The credential is returned only when the account is not
// billing-exhausted (checked before the pool touch so a rejected account does
// not get its LastUsed — and LRU position — bumped) and healthy for the model
// (present, active, not cooling, not model-locked — enforced by
// AccountPool.SelectAccountByID). Misses are logged at Debug and silently let
// the caller fall back to the LRU path; no account-selection metric is
// recorded on the miss itself so the fallback's own outcome stays accurate.
// Successful resolutions are recorded with the distinct "selected_preferred"
// status so sticky pin health is observable separately from LRU hits.
func (g *NenyaGateway) selectPreferredAccountKey(ctx context.Context, providerName, model, accountID string) (string, string, bool) {
	if g.AccountManager == nil || accountID == "" {
		return "", "", false
	}
	if g.BillingTracker != nil && g.BillingTracker.IsExhausted(providerName, accountID) {
		return "", "", false
	}
	selected, err := g.AccountManager.SelectAccountByID(ctx, providerName, model, accountID)
	if err != nil || selected == nil {
		if g.Logger != nil {
			attrs := []any{"provider", providerName, "model", model, "account", accountID}
			if err != nil {
				attrs = append(attrs, "err", err)
			}
			g.Logger.Debug("sticky preferred account unavailable, falling back to LRU", attrs...)
		}
		return "", "", false
	}
	if g.Metrics != nil {
		g.Metrics.RecordAccountSelection(providerName, "selected_preferred")
	}
	return selected.Credential, selected.ID, true
}

// GetProviderAPIKeyForModel returns an API key for the given provider and model.
// It checks the multi-account pool first (selecting the least-recently-used account),
// then falls back to the legacy single-key path.
// Returns the key, the selected account name (empty if not using multi-account), and
// whether a key was found.
func (g *NenyaGateway) GetProviderAPIKeyForModel(ctx context.Context, providerName, model string) ([]byte, string, bool) {
	if g.AccountManager != nil {
		if key, accountID, ok := g.selectAccountKey(ctx, providerName, model); ok {
			return key, accountID, true
		}
	}
	key, ok := g.GetProviderAPIKey(providerName)
	return key, "", ok
}

func (g *NenyaGateway) GetProvidersMap() map[string]*config.Provider {
	return g.Providers
}

// SelectCredentialForModel satisfies the routing.AccountSelector interface.
func (g *NenyaGateway) SelectCredentialForModel(ctx context.Context, provider, model string) (string, string, bool) {
	key, accountID, ok := g.GetProviderAPIKeyForModel(ctx, provider, model)
	return string(key), accountID, ok
}

// SelectCredentialForPreferredAccount satisfies the
// routing.PreferredAccountSelector interface: it resolves the credential of a
// sticky session's pinned account, or reports not-ok when the account is
// absent, unhealthy, or billing-exhausted so the caller falls back to LRU.
func (g *NenyaGateway) SelectCredentialForPreferredAccount(ctx context.Context, provider, model, preferredAccountID string) (string, string, bool) {
	return g.selectPreferredAccountKey(ctx, provider, model, preferredAccountID)
}

// ListAccountIDs satisfies the billing.AccountLister interface.
func (g *NenyaGateway) ListAccountIDs(ctx context.Context, provider string) []string {
	if g.AccountManager == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	pool, err := g.AccountManager.GetPool(ctx, provider)
	if err != nil {
		return nil
	}
	return pool.ListAccountIDs(ctx)
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
		return provider.APIKey != "" || provider.AuthStyle == config.AuthStyleNone
	}
	return false
}

func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

func (g *NenyaGateway) ExtractQuotaFromResponseHeaders(ctx context.Context, provider, accountName string, headers http.Header) {
	p, ok := g.Providers[provider]
	if !ok || p.Billing == nil {
		return
	}
	if p.Billing.QuotaSource != "headers" || p.Billing.QuotaExtraction == nil {
		return
	}
	info, err := billing.ExtractQuotaFromHeaders(ctx, headers, billing.QuotaExtractionConfig{
		Mode:            string(p.Billing.QuotaExtraction.Mode),
		RemainingHeader: p.Billing.QuotaExtraction.RemainingHeader,
		LimitHeader:     p.Billing.QuotaExtraction.LimitHeader,
		ResetHeader:     p.Billing.QuotaExtraction.ResetHeader,
	}, g.Logger)
	if err == nil && info != nil && info.BalanceUSD <= 0 && g.BillingTracker != nil {
		g.BillingTracker.MarkExhausted(ctx, provider, accountName, "quota exhausted via response headers")
	}
}

// windowSummaryCacheSize resolves the summary-cache bound from config,
// defaulting to 512 lineages when unset or non-positive.
func windowSummaryCacheSize(cfg config.Config) int {
	if cfg.Window.SummaryCacheSize != nil && *cfg.Window.SummaryCacheSize > 0 {
		return *cfg.Window.SummaryCacheSize
	}
	return 512
}
