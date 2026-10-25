package config

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nenya/internal/netutil"
)

func closeBody(resp *http.Response) {
	if resp != nil {
		_ = resp.Body.Close()
	}
}

// ValidateConfiguration validates the entire configuration, including
// the bouncer engine health (if pingProviders is true). Returns an error
// if any validation rule fails.
func ValidateConfiguration(ctx context.Context, cfg *Config, secrets *SecretsConfig, logger *slog.Logger) error {
	return ValidateConfigurationWithPing(ctx, cfg, secrets, logger, true)
}

// ValidateConfigurationNoPing validates the configuration without
// performing health checks on providers or the bouncer engine. Useful for
// offline validation or tests that mock network calls.
func ValidateConfigurationNoPing(ctx context.Context, cfg *Config, secrets *SecretsConfig, logger *slog.Logger) error {
	return ValidateConfigurationWithPing(ctx, cfg, secrets, logger, false)
}

// ValidateConfigurationWithPing is the internal implementation of
// configuration validation, optionally enabling provider health checks.
func ValidateConfigurationWithPing(ctx context.Context, cfg *Config, secrets *SecretsConfig, logger *slog.Logger, pingProviders bool) error {
	logger.Info("starting configuration validation")

	providers := ResolveProviders(cfg, secrets)

	if err := validateOllamaEngine(ctx, cfg, providers, pingProviders, logger); err != nil {
		return err
	}

	if err := validateLocalEngine(ctx, cfg, pingProviders, logger); err != nil {
		return err
	}

	errors := collectValidationErrors(ctx, cfg, providers, pingProviders, logger)
	errors = append(errors, validateKeyLimits(cfg, secrets)...)
	// NENYA-196: identity-referencing configuration is checked here so that
	// -validate and the SIGHUP reload reject an unsafe document; boot runs the
	// same checks via ValidateConfigReferences.
	errors = append(errors, ValidateConfigReferences(cfg, secrets, logger)...)
	if len(errors) > 0 {
		return fmt.Errorf("provider validation failed:\n  - %s", strings.Join(errors, "\n  - "))
	}

	logger.Info("configuration validation completed successfully")
	return nil
}

func validateOllamaEngine(ctx context.Context, cfg *Config, providers map[string]*Provider, pingProviders bool, logger *slog.Logger) error {
	if !pingProviders {
		return nil
	}
	if cfg.Bouncer.Enabled == nil || !*cfg.Bouncer.Enabled {
		return nil
	}
	if cfg.Bouncer.Engine.Provider != "ollama" {
		return nil
	}

	p, ok := providers[cfg.Bouncer.Engine.Provider]
	if !ok || p == nil || p.URL == "" {
		return nil
	}

	logger.Info("checking Ollama engine health", "provider", cfg.Bouncer.Engine.Provider, "url", p.URL)
	// The runtime ollama policy inherits the fleet CA bundle (extra root
	// trust on loopback is harmless) but never the fleet proxy — mirror
	// that split here so the probe matches runtime reachability.
	if !validateOllamaHealth(ctx, p.URL, p.EffectiveCABundle(cfg.Network), p.ProxyURL) {
		return fmt.Errorf("ollama engine provider %q at %s is not reachable", cfg.Bouncer.Engine.Provider, p.URL)
	}
	logger.Info("Ollama engine health check passed")
	return nil
}

func validateLocalEngine(ctx context.Context, cfg *Config, pingProviders bool, logger *slog.Logger) error {
	if !pingProviders {
		return nil
	}
	if cfg.LocalEngine == nil {
		return nil
	}

	logger.Info("checking local engine health", "url", cfg.LocalEngine.BaseURL)
	if !validateOllamaHealth(ctx, cfg.LocalEngine.BaseURL, cfg.Network.GetCABundle(), "") {
		return fmt.Errorf("local engine at %s is not reachable", cfg.LocalEngine.BaseURL)
	}
	logger.Info("local engine health check passed")
	return nil
}

func collectValidationErrors(ctx context.Context, cfg *Config, providers map[string]*Provider, pingProviders bool, logger *slog.Logger) []string {
	errors := []string{}

	errors = append(errors, ValidateConfigCredentialsStrength(cfg)...)
	errors = append(errors, validateTFIDFQuerySource(cfg.Context.TFIDFQuerySource)...)
	errors = append(errors, validatePatternsToList("bouncer.patterns", cfg.Bouncer.RedactPatterns, logger)...)
	errors = append(errors, validatePatternsToList("governance.blocked_execution_patterns", cfg.Governance.BlockedExecutionPatterns, logger)...)
	errors = append(errors, validateInjectionConfig(cfg, logger)...)
	errors = append(errors, validateSpotlightConfig(cfg)...)
	errors = append(errors, validateJudgmentsConfig(cfg)...)
	errors = append(errors, validateTfidfRerankConfig(cfg)...)
	errors = append(errors, validateSpotlightRiskTiersConfig(cfg)...)
	errors = append(errors, validateParamCompatConfig(cfg)...)
	errors = append(errors, validateAutoCacheBreakpointsConfig(cfg, logger)...)
	errors = append(errors, validateBYOKConfig(cfg, logger)...)
	errors = append(errors, validateMCPToolSurfaces(cfg)...)
	if cfg.Governance.SecretResolutionTimeoutSeconds != nil {
		t := *cfg.Governance.SecretResolutionTimeoutSeconds
		if t < 0 {
			errors = append(errors, fmt.Sprintf("governance.secret_resolution_timeout_seconds %d: must not be negative", t))
		} else if t > MaxSecretResolutionTimeoutSeconds {
			errors = append(errors, fmt.Sprintf("governance.secret_resolution_timeout_seconds %d: capped at %d", t, MaxSecretResolutionTimeoutSeconds))
		}
	}
	if cfg.Governance.ReloadDrainTimeoutSeconds != nil {
		t := *cfg.Governance.ReloadDrainTimeoutSeconds
		if t < 0 {
			errors = append(errors, fmt.Sprintf("governance.reload_drain_timeout_seconds %d: must not be negative", t))
		} else if t > MaxReloadDrainTimeoutSeconds {
			errors = append(errors, fmt.Sprintf("governance.reload_drain_timeout_seconds %d: capped at %d", t, MaxReloadDrainTimeoutSeconds))
		}
	}
	errors = append(errors, validateSelfLoopGuard(cfg)...)
	errors = append(errors, validateTelemetryExposure(cfg, logger)...)
	errors = append(errors, validateNetworkPolicy(cfg)...)
	errors = append(errors, validateExfilGuardConfig(cfg)...)
	errors = append(errors, validateCanaryConfig(cfg)...)
	errors = append(errors, validateMCPGuardConfig(cfg)...)
	errors = append(errors, validateModelRegistryErrors(logger)...)
	errors = append(errors, validateEntropyConfig(cfg.Bouncer)...)
	errors = append(errors, validateModelsHiddenConfig(cfg)...)
	errors = append(errors, validateProviderRateLimits(cfg)...)
	errors = append(errors, validateProviderOptions(cfg)...)
	errors = append(errors, validateBillingConfig(cfg)...)
	errors = append(errors, validateUpstreamTimeoutSeconds(cfg.Governance.UpstreamTimeoutSeconds)...)
	errors = append(errors, validateStreamStallTimeoutSeconds(cfg.Server.StreamStallTimeoutSeconds)...)
	errors = append(errors, validateStreamIdleTimeoutSeconds(cfg.Governance.StreamIdleTimeoutSeconds)...)
	errors = append(errors, validateThinkingStreamIdleTimeoutSeconds(cfg.Governance.ThinkingStreamIdleTimeoutSeconds)...)
	errors = append(errors, validateBootstrapBufferBytes(cfg.Governance.StreamBootstrapBufferBytes)...)
	if v := cfg.Governance.StreamBootstrapMaxHoldSeconds; v < 0 || v > MaxTimeoutSeconds {
		errors = append(errors, fmt.Sprintf("governance.stream_bootstrap_max_hold_seconds must be between 0 and %d seconds, got %d", MaxTimeoutSeconds, v))
	}
	errors = append(errors, validateSlowCallThresholdSeconds(cfg.Governance.SlowCallThresholdSeconds, "governance.slow_call_threshold_seconds")...)
	errors = append(errors, validateAgentStrategies(cfg.Agents)...)
	errors = append(errors, validateTokenCalibration(cfg.Governance.TokenCalibration)...)

	if pingProviders {
		errors = append(errors, validateProviders(ctx, providers, cfg.Network, logger)...)
	}

	return errors
}

// judgmentNameRe constrains judgment names: they flow into metric
// labels and resolution error labels.
var judgmentNameRe = regexp.MustCompile(`^[a-z0-9_]+$`)

// validateSpotlightRiskTiersConfig checks the advisory spotlight
// risk-tier surface. Must run after ApplyDefaults so inheritance has
// applied.
func validateSpotlightRiskTiersConfig(cfg *Config) []string {
	rt := cfg.Governance.SpotlightRiskTiers()
	if rt == nil {
		return nil
	}
	var errs []string
	if rt.MaxBytes < 0 {
		errs = append(errs, "governance.spotlight.risk_tiers.max_bytes must be >= 0")
	}
	if rt.MaxBytes > 0 && rt.MaxBytes < 64 {
		errs = append(errs, "governance.spotlight.risk_tiers.max_bytes must be >= 64 when set (smaller budgets cannot fit the judgment framing)")
	}
	if rt.TimeoutSeconds < 0 {
		errs = append(errs, "governance.spotlight.risk_tiers.timeout_seconds must be >= 0")
	}
	if v := rt.EscalateBelowConfidence; v < 0 || v > 1 {
		errs = append(errs, "governance.spotlight.risk_tiers.escalate_below_confidence must be in (0, 1] when set (0 disables the cascade)")
	}
	if rt.Engine != nil && rt.Engine.AgentName == "" && rt.Engine.Provider == "" {
		errs = append(errs, "governance.spotlight.risk_tiers.engine: empty engine reference (omit the key to inherit, or set provider/model or agent)")
	}
	if rt.Enabled != nil && *rt.Enabled && (rt.Engine == nil || len(rt.Engine.ResolvedTargets) == 0) {
		errs = append(errs, "governance.spotlight.risk_tiers: enabled but engine missing or unresolved")
	}
	return errs
}

// validateTfidfRerankConfig checks the advisory TF-IDF rerank surface.
// Must run after ApplyDefaults so the engine inheritance has applied.
func validateTfidfRerankConfig(cfg *Config) []string {
	r := cfg.Governance.TfidfRerank
	if r == nil {
		return nil
	}
	var errs []string
	// Band and MaxBlocks are coerced to defaults for <= 0 in
	// applyTfidfRerankDefaults, so only byte/timeout bounds are checked.
	if r.MaxBytes < 0 {
		errs = append(errs, "governance.tfidf_rerank.max_bytes must be >= 0")
	}
	if r.MaxBytes > 0 && r.MaxBytes < 64 {
		errs = append(errs, "governance.tfidf_rerank.max_bytes must be >= 64 when set (smaller budgets cannot fit the judgment framing)")
	}
	if r.Engine != nil && r.Engine.AgentName == "" && r.Engine.Provider == "" {
		errs = append(errs, "governance.tfidf_rerank.engine: empty engine reference (omit the key to inherit, or set provider/model or agent)")
	}
	if r.TimeoutSeconds < 0 {
		errs = append(errs, "governance.tfidf_rerank.timeout_seconds must be >= 0")
	}
	if r.Enabled != nil && *r.Enabled && (r.Engine == nil || len(r.Engine.ResolvedTargets) == 0) {
		errs = append(errs, "governance.tfidf_rerank: enabled but engine missing or unresolved")
	}
	return errs
}

// selfLoopSurfaces lists every resolved engine target set with its
// config-surface label for the self-loop guard.
func selfLoopSurfaces(cfg *Config) []struct {
	label   string
	targets []EngineTarget
} {
	surfaces := []struct {
		label   string
		targets []EngineTarget
	}{
		{"bouncer_engine", cfg.Bouncer.Engine.ResolvedTargets},
		{"window_engine", cfg.Window.Engine.ResolvedTargets},
	}
	if cfg.Governance.Injection != nil {
		if esc := cfg.Governance.Injection.GetEscalation(); esc != nil && esc.Engine != nil {
			surfaces = append(surfaces, struct {
				label   string
				targets []EngineTarget
			}{"injection_escalation_engine", esc.Engine.ResolvedTargets})
		}
	}
	for name, judgment := range cfg.Governance.Judgments {
		if judgment == nil || judgment.Engine == nil {
			continue
		}
		surfaces = append(surfaces, struct {
			label   string
			targets []EngineTarget
		}{"judgment_" + name, judgment.Engine.ResolvedTargets})
	}
	if cfg.Governance.TfidfRerank != nil && cfg.Governance.TfidfRerank.Engine != nil {
		surfaces = append(surfaces, struct {
			label   string
			targets []EngineTarget
		}{"tfidf_rerank_engine", cfg.Governance.TfidfRerank.Engine.ResolvedTargets})
	}
	if rt := cfg.Governance.SpotlightRiskTiers(); rt != nil && rt.Engine != nil {
		surfaces = append(surfaces, struct {
			label   string
			targets []EngineTarget
		}{"spotlight_tier_engine", rt.Engine.ResolvedTargets})
	}
	return surfaces
}

// splitListenAddr splits a configured listen address into host and
// port. Returns ok=false for empty or unparseable addresses — the HTTP
// server will reject those at startup, so the guard stays silent.
func splitListenAddr(addr string) (host, port string, ok bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", "", false
	}
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", "", false
	}
	return h, p, true
}

// listenHostMatchesSelfHost reports whether a target URL host can be
// the gateway itself given the configured listen host. Wildcard binds
// ("", "0.0.0.0", "::") match any loopback target host; a concrete
// listen host matches itself. Documented limitation: with a wildcard
// bind, non-loopback local interface addresses are not enumerated.
func listenHostMatchesSelfHost(targetHost, listenHost string) bool {
	norm := func(h string) string {
		h = strings.ToLower(strings.TrimSpace(h))
		switch h {
		case "localhost", "127.0.0.1", "::1":
			return "loopback"
		}
		return h
	}
	t, l := norm(targetHost), norm(listenHost)
	switch l {
	case "", "0.0.0.0", "::", "*":
		return t == "loopback"
	}
	return t == l
}

// isGatewayPath reports whether a URL path hits the gateway's own
// proxying endpoints (/v1/* or /proxy/*): routing an engine or
// judgment call through them would make the gateway proxy to itself.
func isGatewayPath(p string) bool {
	return p == "/v1" || p == "/proxy" ||
		strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/proxy/")
}

// WarnTelemetryExposure re-checks the NENYA-131 telemetry exposure warning
// against the effective listen address. HOST/PORT override server.listen_addr
// after config validation runs, so callers that apply the override must call
// this afterwards.
//
// Limitation: on SIGHUP reload the check evaluates the reloaded config's
// listen_addr, not the address bound at startup — an env override
// (HOST/PORT) is not visible to the reload path. Operators overriding the
// listener via env should keep the config file consistent, or rely on the
// startup warning.
func WarnTelemetryExposure(cfg *Config, logger *slog.Logger) {
	validateTelemetryExposure(cfg, logger)
}

// validateNetworkPolicy validates the NENYA-137 outbound network settings:
// CA bundles must be readable PEM files and proxy URLs must use a supported
// scheme — checked for the network section and for every provider override,
// so a bad path or scheme fails at startup instead of at first dispatch.
func validateNetworkPolicy(cfg *Config) []string {
	var errs []string
	check := func(what, caBundle, proxyURL string) {
		if caBundle != "" {
			// Read per start (also read again by the transport builders): the
			// duplicate open is startup-only cost, kept for locality.
			if _, err := netutil.LoadCABundle(caBundle); err != nil {
				errs = append(errs, fmt.Sprintf("%s.ca_bundle: %v", what, err))
			}
		}
		if proxyURL != "" {
			if err := netutil.ValidateProxyURL(proxyURL); err != nil {
				errs = append(errs, fmt.Sprintf("%s.proxy_url: %v", what, err))
			}
		}
	}

	if cfg.Network != nil {
		check("network", cfg.Network.CABundle, cfg.Network.ProxyURL)
	}
	for name, p := range cfg.Providers {
		check(fmt.Sprintf("providers[%q]", name), p.CABundle, p.ProxyURL)
	}
	return errs
}

// ValidateNetworkPolicy validates the NENYA-137 outbound network settings
// (CA bundle paths readable PEM, proxy URLs with a supported scheme) across
// the network section and every provider override. Exported for the startup
// path, where the ping-free validation subset runs before the gateway is
// constructed — an invalid policy must fail startup, not degrade silently.
func ValidateNetworkPolicy(cfg *Config) []string {
	return validateNetworkPolicy(cfg)
}

// validateTelemetryExposure warns (never errors) when
// server.telemetry_unauthenticated is combined with a non-loopback listener
// (NENYA-131): the unauthenticated /statsz and /metrics surfaces would be
// reachable from the network. A warning, not an error, because the operator
// may legitimately front the port with a firewall or network policy.
func validateTelemetryExposure(cfg *Config, logger *slog.Logger) []string {
	if !cfg.Server.TelemetryUnauthenticated {
		return nil
	}
	host, port, ok := splitListenAddr(cfg.Server.ListenAddr)
	if !ok {
		return nil
	}
	// Malformed ports are rejected at startup; stay silent like the
	// self-loop guard does for unparseable addresses.
	if _, err := strconv.Atoi(port); err != nil {
		return nil
	}
	loopback := false
	switch strings.ToLower(host) {
	case "localhost":
		loopback = true
	default:
		if ip := net.ParseIP(host); ip != nil {
			loopback = ip.IsLoopback()
		}
	}
	if loopback {
		return nil
	}
	logger.Warn("telemetry_unauthenticated exposes /statsz and /metrics without authentication on a non-loopback listener",
		"listen_addr", cfg.Server.ListenAddr,
		"hint", "bind 127.0.0.1 or rely on an external network policy")
	return nil
}

// validateSelfLoopGuard rejects engine and judgment targets whose URL
// points at the gateway's own /v1/* or /proxy/ endpoints on the
// configured listen address: such a target makes the gateway proxy to
// itself, deadlocking under load. Same host on a different port,
// non-gateway paths, and third-party hosts remain allowed. Must run
// after ApplyDefaults so engine references are resolved.
func validateSelfLoopGuard(cfg *Config) []string {
	listenHost, listenPort, ok := splitListenAddr(cfg.Server.ListenAddr)
	if !ok {
		return nil
	}
	var errs []string
	for _, surface := range selfLoopSurfaces(cfg) {
		for _, target := range surface.targets {
			if target.Provider == nil {
				continue
			}
			// Check the primary URL and every format-specific override
			// (e.g. format_urls.systemone): any of them can be the
			// actual dispatch target.
			urls := append([]string{target.Provider.URL}, formatURLValues(target.Provider.FormatURLs)...)
			for _, raw := range urls {
				u, err := url.Parse(raw)
				if err != nil || u.Port() != listenPort {
					continue
				}
				if !listenHostMatchesSelfHost(u.Hostname(), listenHost) {
					continue
				}
				if isGatewayPath(u.EscapedPath()) {
					errs = append(errs, fmt.Sprintf(
						"%s: engine target for provider %q (%s) points at the gateway's own endpoint — engines and judgment targets must not route through the gateway itself (self-proxying deadlock)",
						surface.label, target.Provider.Name, raw))
				}
			}
		}
	}
	return errs
}

// formatURLValues returns the values of a format-URL map sorted for
// deterministic error ordering.
func formatURLValues(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// validateJudgmentsConfig checks the shape and budget fields of every
// advisory judgment entry. Contracts are code-owned, so only the
// user-visible surface (name, engine presence, budgets) is validated
// here; engine references resolve during ApplyDefaults.
func validateJudgmentsConfig(cfg *Config) []string {
	var errs []string
	for name, judgment := range cfg.Governance.Judgments {
		errs = append(errs, validateJudgmentEntry(name, judgment)...)
	}
	return errs
}

// validateJudgmentEntry checks one advisory judgment entry. Contracts
// are code-owned, so only the user-visible surface (name, engine
// presence, budgets) is validated here; engine references resolve
// during ApplyDefaults.
func validateJudgmentEntry(name string, judgment *JudgmentConfig) []string {
	var errs []string
	if judgment == nil {
		errs = append(errs, fmt.Sprintf("governance.judgments.%s: entry must not be null", name))
		return errs
	}
	if !judgmentNameRe.MatchString(name) {
		errs = append(errs, fmt.Sprintf("governance.judgments.%s: name must match %s", name, judgmentNameRe.String()))
	}
	if judgment.MaxBytes < 0 {
		errs = append(errs, fmt.Sprintf("governance.judgments.%s.max_bytes must be >= 0", name))
	}
	if judgment.MaxBytes > 0 && judgment.MaxBytes < 64 {
		errs = append(errs, fmt.Sprintf("governance.judgments.%s.max_bytes must be >= 64 when set (smaller budgets cannot fit the judgment framing)", name))
	}
	if judgment.TimeoutSeconds < 0 {
		errs = append(errs, fmt.Sprintf("governance.judgments.%s.timeout_seconds must be >= 0", name))
	}
	if v := judgment.EscalateBelowConfidence; v < 0 || v > 1 {
		errs = append(errs, fmt.Sprintf("governance.judgments.%s.escalate_below_confidence must be in (0, 1] when set (0 disables the cascade)", name))
	}
	switch judgment.Action {
	case "", "log", "strict":
	default:
		errs = append(errs, fmt.Sprintf("governance.judgments.%s.action: invalid value %q, must be empty, \"log\", or \"strict\"", name, judgment.Action))
	}
	return append(errs, validateJudgmentEngineRef(name, judgment)...)
}

// validateJudgmentEngineRef checks the engine reference surface of one
// advisory judgment entry (AGENTS.md §11 decomposition).
func validateJudgmentEngineRef(name string, judgment *JudgmentConfig) []string {
	// An explicitly empty engine object ("engine": {}) would otherwise
	// skip inheritance and resolution silently and fail only at first
	// judgment construction — reject it at load. Same for an enabled
	// site whose engine resolved to nothing.
	if judgment.Engine != nil && judgment.Engine.AgentName == "" && judgment.Engine.Provider == "" {
		return []string{fmt.Sprintf("governance.judgments.%s.engine: empty engine reference (omit the key to inherit, or set provider/model or agent)", name)}
	}
	if judgment.Enabled != nil && *judgment.Enabled && judgment.Engine != nil && len(judgment.Engine.ResolvedTargets) == 0 {
		return []string{fmt.Sprintf("governance.judgments.%s: enabled but engine missing or unresolved", name)}
	}
	return nil
}

// validateSpotlightConfig checks the spotlight mode value and rejects
// per-agent mode/byte-cap fields (only enabled/history-enabled are
// configurable per agent; history-enabled is validated by ApplyDefaults
// semantics — unset inherits the global enabled flag). The global
// enabled=false + history_enabled=true combination is intentionally legal
// (proxy surfaces off, history on); the per-agent combo is rejected
// because resolveSettings force-disables history on an explicit per-agent
// disable, making that combination unreachable.
func validateSpotlightConfig(cfg *Config) []string {
	var errs []string
	if cfg.Governance.Spotlight != nil {
		mode := cfg.Governance.Spotlight.Mode
		if mode != SpotlightModeDelimiters && mode != SpotlightModeDatamarking {
			errs = append(errs, fmt.Sprintf("governance.spotlight.mode %q is not one of: %s, %s", mode, SpotlightModeDelimiters, SpotlightModeDatamarking))
		}
		if cfg.Governance.Spotlight.MaxToolResultBytes < 0 {
			errs = append(errs, "governance.spotlight.max_tool_result_bytes must be >= 0")
		}
	}
	for name, agent := range cfg.Agents {
		if agent.Spotlight == nil {
			continue
		}
		if agent.Spotlight.Mode != "" {
			errs = append(errs, fmt.Sprintf("agents.%s.spotlight.mode is global-only; configure governance.spotlight.mode", name))
		}
		if agent.Spotlight.MaxToolResultBytes != 0 {
			errs = append(errs, fmt.Sprintf("agents.%s.spotlight.max_tool_result_bytes is global-only; configure governance.spotlight.max_tool_result_bytes", name))
		}
		if agent.Spotlight.Enabled != nil && !*agent.Spotlight.Enabled && agent.Spotlight.HistoryEnabled != nil && *agent.Spotlight.HistoryEnabled {
			errs = append(errs, fmt.Sprintf("agents.%s.spotlight has contradictory enabled=false with history_enabled=true", name))
		}
	}
	return errs
}

// validateInjectionConfig compiles the injection detector's extra/ignore
// patterns (global) so malformed regexes fail at startup rather than at
// first request, and rejects per-agent pattern fields (only enabled/strict
// are configurable per agent).
func validateInjectionConfig(cfg *Config, logger *slog.Logger) []string {
	var errs []string
	if cfg.Governance.Injection != nil {
		errs = append(errs, validatePatternsToList("governance.injection.extra_patterns", cfg.Governance.Injection.ExtraPatterns, logger)...)
		errs = append(errs, validatePatternsToList("governance.injection.ignore_patterns", cfg.Governance.Injection.IgnorePatterns, logger)...)
	}
	for name, agent := range cfg.Agents {
		if agent.Injection == nil {
			continue
		}
		if len(agent.Injection.ExtraPatterns) > 0 {
			errs = append(errs, fmt.Sprintf("agents.%s.injection.extra_patterns is global-only; configure governance.injection.extra_patterns", name))
		}
		if len(agent.Injection.IgnorePatterns) > 0 {
			errs = append(errs, fmt.Sprintf("agents.%s.injection.ignore_patterns is global-only; configure governance.injection.ignore_patterns", name))
		}
		if agent.Injection.Escalation != nil {
			errs = append(errs, fmt.Sprintf("agents.%s.injection.escalation is global-only; configure governance.injection.escalation", name))
		}
	}
	errs = append(errs, validateInjectionEscalation(cfg)...)
	return errs
}

// validateMCPToolSurfaces validates the MCP tool-injection policy knobs
// (NENYA-162): tool_search is non-negative, include/exclude entries are
// non-empty printable-ASCII names or globs, and tool_search_floor entries are
// EXACT names (the floor matches by equality, so a glob would silently never
// match).
func validateMCPToolSurfaces(cfg *Config) []string {
	var errors []string
	for name, agent := range cfg.Agents {
		if agent.MCP == nil {
			continue
		}
		if agent.MCP.ToolSearch < 0 {
			errors = append(errors, fmt.Sprintf("agents.%s.mcp.tool_search %d: must not be negative (0 disables selection)", name, agent.MCP.ToolSearch))
		}
		if agent.MCP.ToolSearch == 0 && len(agent.MCP.ToolSearchFloor) > 0 {
			errors = append(errors, fmt.Sprintf("agents.%s.mcp.tool_search_floor set but tool_search is off: the floor only applies with selection enabled", name))
		}
		for _, pat := range agent.MCP.ToolSearchFloor {
			if !validToolName(pat) {
				errors = append(errors, fmt.Sprintf("agents.%s.mcp.tool_search_floor %q: must be an exact non-empty tool name without glob metacharacters or control characters", name, pat))
			}
		}
	}
	for name, server := range cfg.MCPServers {
		for _, list := range [][]string{server.ToolInclude, server.ToolExclude} {
			for _, pat := range list {
				if !validToolPattern(pat) {
					errors = append(errors, fmt.Sprintf("mcp_servers.%s pattern %q: must be a non-empty name/glob without control characters", name, pat))
				}
			}
		}
	}
	return errors
}

// validToolPattern reports whether an include/exclude pattern is sane: a
// non-empty, printable-ASCII name or '*'/'?' glob.
func validToolPattern(pat string) bool {
	if pat == "" {
		return false
	}
	for i := 0; i < len(pat); i++ {
		if pat[i] < 0x21 || pat[i] > 0x7e {
			return false
		}
	}
	return true
}

// validToolName reports whether an exact tool name (the tool_search floor) is
// sane: printable ASCII with no glob metacharacters, which the exact-match
// floor would silently never match.
func validToolName(pat string) bool {
	if !validToolPattern(pat) {
		return false
	}
	return !strings.ContainsAny(pat, "*?")
}

// validateBYOKConfig validates the bring-your-own-key surface (NENYA-195):
// the credential header must be a canonical, gateway-unowned HTTP header name,
// and a global enable with no provider opted in is a warning (it can never
// take effect, which is almost always a mistake).
func validateBYOKConfig(cfg *Config, logger *slog.Logger) []string {
	b := cfg.Governance.BYOK
	if b == nil {
		return nil
	}
	var errors []string
	if b.Header != "" {
		switch {
		case !isValidHeaderToken(b.Header):
			errors = append(errors, fmt.Sprintf("governance.byok.header %q is not a valid HTTP header name", b.Header))
		case textproto.CanonicalMIMEHeaderKey(b.Header) != b.Header:
			errors = append(errors, fmt.Sprintf("governance.byok.header %q: use the canonical header name %q", b.Header, textproto.CanonicalMIMEHeaderKey(b.Header)))
		case strings.EqualFold(b.Header, "Authorization"):
			errors = append(errors, "governance.byok.header must not be Authorization: the caller's gateway token rides there, and an upstream credential must not be able to masquerade as it")
		case reservedProviderHeaders[strings.ToLower(b.Header)]:
			errors = append(errors, fmt.Sprintf("governance.byok.header %q is a header the gateway sets or forwards itself; the credential would ride upstream as metadata and leak into provider logs", b.Header))
		}
	}
	if !b.Enabled {
		return errors
	}
	optedIn := 0
	for _, p := range cfg.Providers {
		if p.AllowBYOK != nil && *p.AllowBYOK {
			optedIn++
		}
	}
	if optedIn == 0 && logger != nil {
		logger.Warn("governance.byok is enabled but no provider declares allow_byok: a caller-supplied credential can never be used")
	}
	return errors
}

// validateAutoCacheBreakpointsConfig WARNS (never fails the load) when an
// agent opts into automatic cache-breakpoint anchoring while the prefix cache
// is disabled: every dispatch would record a skipped_disabled outcome, which is
// almost certainly a configuration mistake rather than an intent, but it is a
// legal one (the prefix cache can be enabled later without touching the agent
// config) — NENYA-185.
func validateAutoCacheBreakpointsConfig(cfg *Config, logger *slog.Logger) []string {
	if cfg.PrefixCache.Enabled || logger == nil {
		return nil
	}
	for name, agent := range cfg.Agents {
		if agent.AutoCacheBreakpoints != nil && *agent.AutoCacheBreakpoints {
			logger.Warn("auto_cache_breakpoints is enabled while prefix_cache is disabled: anchoring can never happen (every dispatch records skipped_disabled)",
				"agent", name)
		}
	}
	return nil
}

// validateParamCompatConfig validates the target axis of the param-compat
// rules (NENYA-179): token_field names the token-cap spelling a target
// requires, and a typo would rename the payload's only cap to a junk key —
// the cap would be lost with no error, only an unknown-field report.
func validateParamCompatConfig(cfg *Config) []string {
	var errors []string
	for i, rule := range cfg.Governance.ParamCompat {
		if rule.ModelPrefix == "" && (rule.TokenField != "" || len(rule.IntTypedParams) > 0) {
			errors = append(errors, fmt.Sprintf(
				"governance.param_compat[%d] declares the target axis (token_field/int_typed_params) with no model_prefix, so it can never match a target",
				i))
		}
		switch strings.TrimSpace(rule.TokenField) {
		case "", "max_tokens", "max_completion_tokens":
		default:
			errors = append(errors, fmt.Sprintf(
				"governance.param_compat[%d].token_field must be one of max_tokens, max_completion_tokens, got %q",
				i, rule.TokenField))
		}
		for _, param := range rule.IntTypedParams {
			switch strings.TrimSpace(param) {
			case "max_tokens", "max_completion_tokens":
			default:
				errors = append(errors, fmt.Sprintf(
					"governance.param_compat[%d].int_typed_params contains %q, which no target-aware coercion applies to",
					i, param))
			}
		}
	}
	return errors
}

// validateInjectionEscalation checks the tier-2 classifier config: band
// ordering, budget, and the engine reference (presence here; concrete
// target resolution happens in resolveEngineRefs during ApplyDefaults).
func validateInjectionEscalation(cfg *Config) []string {
	esc := cfg.Governance.Injection.GetEscalation()
	if esc == nil {
		return nil
	}
	var errs []string
	// GetEscalation guarantees Enabled; an empty engine reference would
	// silently no-op at runtime, breaking the fail-closed contract.
	if esc.Engine == nil || (esc.Engine.AgentName == "" && esc.Engine.Provider == "") {
		errs = append(errs, "governance.injection.escalation.enabled requires engine (agent reference, provider/model shorthand, or inline object)")
	}
	if esc.MinScore < 1 {
		errs = append(errs, "governance.injection.escalation.min_score must be >= 1")
	}
	if esc.MaxScore < 0 {
		errs = append(errs, "governance.injection.escalation.max_score must be >= 0")
	}
	if esc.MaxScore > 0 && esc.MaxScore <= esc.MinScore {
		errs = append(errs, "governance.injection.escalation.max_score must be greater than min_score")
	}
	if esc.PerRequestLimit < 0 {
		errs = append(errs, "governance.injection.escalation.per_request_limit must be >= 0")
	}
	if esc.MaxBytes < 0 {
		errs = append(errs, "governance.injection.escalation.max_bytes must be >= 0")
	}
	return errs
}

func validateTFIDFQuerySource(source string) []string {
	switch source {
	case "", "prior_messages", "self":
		return nil
	default:
		return []string{fmt.Sprintf("governance.tfidf_query_source: invalid value %q, must be empty, \"prior_messages\", or \"self\"", source)}
	}
}

var validAgentStrategies = map[string]bool{
	"":            true,
	"round-robin": true,
	"fallback":    true,
	"sticky":      true,
}

func validateAgentStrategies(agents map[string]AgentConfig) []string {
	var errs []string
	for name, agent := range agents {
		if !validAgentStrategies[agent.Strategy] {
			errs = append(errs, fmt.Sprintf("agents[%q].strategy: invalid value %q, must be one of round-robin, fallback, sticky", name, agent.Strategy))
		}
		switch agent.StickyProvider {
		case "", "off", "lenient", "strict":
		default:
			errs = append(errs, fmt.Sprintf("agents[%q].sticky_provider: invalid value %q, must be one of off, lenient, strict", name, agent.StickyProvider))
		}
		switch agent.CacheAware {
		case "", CacheAwareOff, CacheAwareAuto, CacheAwareForce:
		default:
			errs = append(errs, fmt.Sprintf("agents[%q].cache_aware: invalid value %q, must be one of off, auto, force", name, agent.CacheAware))
		}
	}
	return errs
}

// validateTokenCalibration validates the NENYA-135 cl100k drift-correction
// knobs: the decay must be a strict per-record retain factor in (0, 1) and
// the clamps must stay ordered and non-negative.
func validateTokenCalibration(cfg *TokenCalibrationConfig) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	if cfg.MinObservations < 0 {
		errs = append(errs, fmt.Sprintf("governance.token_calibration.min_observations must be non-negative (0 uses the default), got %d", cfg.MinObservations))
	}
	if cfg.Decay != 0 && (math.IsNaN(cfg.Decay) || cfg.Decay <= 0 || cfg.Decay >= 1) {
		errs = append(errs, fmt.Sprintf("governance.token_calibration.decay must be in (0, 1), got %v", cfg.Decay))
	}
	if cfg.RatioClampMin < 0 {
		errs = append(errs, fmt.Sprintf("governance.token_calibration.ratio_clamp_min must be non-negative, got %v", cfg.RatioClampMin))
	}
	if math.IsNaN(cfg.RatioClampMax) || (cfg.RatioClampMax != 0 && cfg.RatioClampMax < cfg.RatioClampMin) {
		errs = append(errs, fmt.Sprintf("governance.token_calibration.ratio_clamp_max (%v) must be >= ratio_clamp_min (%v)", cfg.RatioClampMax, cfg.RatioClampMin))
	}
	// A max below the default min with min unset would be silently dropped
	// by resolution (clamped to defaults) — reject it explicitly.
	if cfg.RatioClampMax > 0 && cfg.RatioClampMin == 0 && cfg.RatioClampMax < DefaultTokenCalibrationClampMin {
		errs = append(errs, fmt.Sprintf("governance.token_calibration.ratio_clamp_max (%v) is below the default ratio_clamp_min (%v); set ratio_clamp_min too", cfg.RatioClampMax, DefaultTokenCalibrationClampMin))
	}
	return errs
}

// MaxSlowCallThresholdSeconds caps governance/agents slow_call_threshold_seconds
// (24 hours). A longer threshold would make the slow-call breaker unreachable by
// design, which is what "disabled" is for.
const MaxSlowCallThresholdSeconds = 86400

// MaxStickySessionTTLSeconds caps agents[].sticky_session_ttl_seconds (24
// hours). Exported so the proxy's runtime clamp uses the same value.
const MaxStickySessionTTLSeconds = 86400

func validateStickySessionTTL(agents map[string]AgentConfig) []string {
	var errs []string
	for name, agent := range agents {
		// Pins and affinity resolve for EVERY strategy (NENYA-29): non-sticky
		// agents still use the pin for credential-only affinity, so the TTL is
		// validated regardless of strategy (NENYA-196).
		if agent.StickySessionTTLSeconds < 0 {
			errs = append(errs, fmt.Sprintf("agents[%q].sticky_session_ttl_seconds must be non-negative, got %d", name, agent.StickySessionTTLSeconds))
		}
		if agent.StickySessionTTLSeconds > MaxStickySessionTTLSeconds {
			errs = append(errs, fmt.Sprintf("agents[%q].sticky_session_ttl_seconds exceeds maximum allowed value (%d seconds / 24 hours), got %d", name, MaxStickySessionTTLSeconds, agent.StickySessionTTLSeconds))
		}
	}
	return errs
}

// validateAgentMCPReferences rejects agents that reference MCP servers which
// do not exist (NENYA-196): a dangling name disables the agent's tools at
// request time (the client lookup misses, with only a per-request warning),
// which is exactly the kind of unsafe state that must be unrepresentable at
// load.
func validateAgentMCPReferences(cfg *Config) []string {
	var errs []string
	for name, agent := range cfg.Agents {
		if agent.MCP == nil {
			continue
		}
		for i, server := range agent.MCP.Servers {
			if server == "" {
				errs = append(errs, fmt.Sprintf("agents[%q].mcp.servers[%d]: empty server name", name, i))
				continue
			}
			if _, ok := cfg.MCPServers[server]; !ok {
				errs = append(errs, fmt.Sprintf("agents[%q].mcp.servers[%d]: unknown mcp_servers[%q]", name, i, server))
			}
		}
	}
	// Deliberately not validated: a model entry may set provider_rgx, which
	// supersedes provider, so a reference check on provider would reject valid
	// configurations; a genuinely unknown provider is reported at target-build
	// time.
	return errs
}

// validateSecretAgentScopes rejects API keys whose allowed_agents list
// references an agent that does not exist (NENYA-196): the key would be
// denied that agent at request time, and a typo is indistinguishable from an
// intentional restriction. Admin keys are exempt (AuthorizeAgent returns true
// for admin before consulting the list, so the field is inert for them).
// Auto-generated agents (discovery enabled with auto_agents) are created after
// validation, so the reference check is skipped in that mode; an empty list is
// the documented backward-compatible "all agents" grant and only warrants a
// warning for keys that can reach the chat routes.
func validateSecretAgentScopes(cfg *Config, secrets *SecretsConfig, logger *slog.Logger) []string {
	if secrets == nil {
		return nil
	}
	autoAgents := cfg.Discovery.Enabled != nil && *cfg.Discovery.Enabled &&
		cfg.Discovery.AutoAgents != nil && *cfg.Discovery.AutoAgents
	var errs []string
	for name, key := range secrets.ApiKeys {
		if !key.Enabled || authKeyIsAdmin(key) {
			continue
		}
		if len(key.AllowedAgents) == 0 {
			if authKeyCanChat(key) && logger != nil {
				logger.Warn("api key grants access to every agent wherever agent scoping applies (empty allowed_agents)",
					"key", name,
					"note", "set allowed_agents to restrict the chat routes; admin keys are unaffected")
			}
			continue
		}
		if autoAgents {
			continue
		}
		for _, allowed := range key.AllowedAgents {
			if allowed == "" {
				errs = append(errs, fmt.Sprintf("api_keys[%q].allowed_agents: empty agent name", name))
				continue
			}
			if _, ok := cfg.Agents[allowed]; !ok {
				errs = append(errs, fmt.Sprintf("api_keys[%q].allowed_agents: unknown agent %q", name, allowed))
			}
		}
	}
	return errs
}

// authKeyIsAdmin reports whether the key carries the admin role.
func authKeyIsAdmin(key ApiKey) bool {
	for _, role := range key.Roles {
		if role == RoleAdmin {
			return true
		}
	}
	return false
}

// authKeyCanChat reports whether the key can reach the agent-scoped chat
// routes: a user role can (unless an endpoint allowlist excludes them), and
// read-only keys cannot. An endpoint allowlist overrides role defaults, so a
// read-only key explicitly allowed onto the chat routes is treated as
// chat-capable too. Admin keys never reach this helper (they are exempt from
// the scope check).
func authKeyCanChat(key ApiKey) bool {
	// An endpoint allowlist overrides role defaults, so it decides on its own:
	// chat-capable only when it names one of the chat routes.
	if len(key.AllowedEndpoints) > 0 {
		for _, endpoint := range key.AllowedEndpoints {
			if endpoint == "POST /v1/chat/completions" || endpoint == "POST /v1/messages" {
				return true
			}
		}
		return false
	}
	// Read-only narrows the key to GET monitoring surfaces (AuthorizeEndpoint),
	// so a read-only role denies the POST chat routes regardless of any other
	// role on the key.
	for _, role := range key.Roles {
		if role == RoleReadOnly {
			return false
		}
	}
	for _, role := range key.Roles {
		if role == RoleUser {
			return true
		}
	}
	return false
}

// validateMCPServerConfigs validates each configured MCP server (NENYA-183):
// the URL must be an absolute http(s) URL with a host and no embedded
// credentials, plaintext http is refused for non-local hosts, required
// header names must be well-formed, and the timeouts must be non-negative.
func validateMCPServerConfigs(cfg *Config, logger *slog.Logger) []string {
	var errs []string
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		server := cfg.MCPServers[name]
		if server.URL == "" {
			errs = append(errs, fmt.Sprintf("mcp_servers[%q].url is required", name))
		} else {
			errs = append(errs, validateMCPServerURL(name, server.URL)...)
		}
		if server.Timeout < 0 {
			errs = append(errs, fmt.Sprintf("mcp_servers[%q].timeout must be non-negative, got %d", name, server.Timeout))
		}
		if server.KeepAliveInterval < 0 {
			errs = append(errs, fmt.Sprintf("mcp_servers[%q].keep_alive_interval must be non-negative, got %d", name, server.KeepAliveInterval))
		}
		for i, header := range server.RequiredHeaders {
			if strings.TrimSpace(header) == "" {
				errs = append(errs, fmt.Sprintf("mcp_servers[%q].required_headers[%d]: empty header name", name, i))
				continue
			}
			// AUD-19 (NENYA-223): names match case-insensitively at runtime,
			// so a spelling mismatch is no longer fatal — but a header that is
			// declared required and NOT configured at all still fails closed
			// with 503 on every request selecting the server. Surface it at
			// load (Warn, not an error: a header may legitimately arrive from
			// an environment-specific deployment overlay).
			if !serverHasHeader(server.Headers, header) {
				logger.Warn("mcp server declares a required header that is not configured; requests selecting it will fail closed",
					"server", name, "header", header)
			}
		}
	}
	return errs
}

// validateMCPServerURL enforces the MCP server origin policy: absolute
// http(s), a host, no userinfo (credentials must live in headers, never in a
// URL that diagnostics may print), and no plaintext http for a non-local host
// — a remote tool server reached over cleartext exposes both the credential
// and the tool traffic.
func validateMCPServerURL(name, raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return []string{fmt.Sprintf("mcp_servers[%q].url is not a valid URL: %v", name, err)}
	}
	var errs []string
	switch parsed.Scheme {
	case "http", "https":
	default:
		errs = append(errs, fmt.Sprintf("mcp_servers[%q].url: unsupported scheme %q (must be http or https)", name, parsed.Scheme))
	}
	if parsed.Hostname() == "" {
		errs = append(errs, fmt.Sprintf("mcp_servers[%q].url: missing host", name))
	}
	if parsed.User != nil {
		errs = append(errs, fmt.Sprintf("mcp_servers[%q].url must not embed credentials; put them in headers", name))
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "" && !isLocalMCPHost(parsed.Hostname()) {
		errs = append(errs, fmt.Sprintf("mcp_servers[%q].url: plaintext http is only allowed for local hosts (loopback/private); use https for %q", name, parsed.Hostname()))
	}
	return errs
}

// isLocalMCPHost reports whether a host is loopback, private, or a local
// name, i.e. a host for which plaintext http does not leave the machine or
// the private network.
func isLocalMCPHost(host string) bool {
	lower := strings.ToLower(strings.TrimSuffix(host, "."))
	if lower == "localhost" || strings.HasSuffix(lower, ".localhost") {
		return true
	}
	ip := net.ParseIP(lower)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		// IsUnspecified covers 0.0.0.0/::, the conventional "bind every local
		// interface" address, which dials as loopback on Linux.
		return true
	}
	// Link-local unicast (169.254.0.0/16, fe80::/10) is NOT treated as local:
	// the cloud metadata endpoint lives there and the tool-argument guard
	// denies it explicitly, so blessing it here would contradict that policy.
	return false
}

// validateAPIKeyToolGrants validates api_keys[].allowed_tools syntax and
// server references (NENYA-183): entries must be non-empty, and a namespaced
// entry ("server__tool" / "server__*") must name a configured MCP server, so a
// typo cannot silently produce a key that can invoke nothing (or, worse, a
// grant the operator believes is narrower than it is).
func validateAPIKeyToolGrants(cfg *Config, secrets *SecretsConfig) []string {
	if secrets == nil {
		return nil
	}
	names := make([]string, 0, len(secrets.ApiKeys))
	for name := range secrets.ApiKeys {
		names = append(names, name)
	}
	slices.Sort(names)
	var errs []string
	for _, name := range names {
		key := secrets.ApiKeys[name]
		if !key.Enabled {
			continue
		}
		for i, tool := range key.AllowedTools {
			if strings.TrimSpace(tool) == "" {
				errs = append(errs, fmt.Sprintf("api_keys[%q].allowed_tools[%d]: empty tool name", name, i))
				continue
			}
			server, _, found := strings.Cut(tool, "__")
			if !found {
				// Bare tool name: matched against the tool's own name on any
				// server, so there is no server reference to validate.
				continue
			}
			if server == "" {
				errs = append(errs, fmt.Sprintf("api_keys[%q].allowed_tools[%d]: missing server prefix in %q", name, i, tool))
				continue
			}
			if _, ok := cfg.MCPServers[server]; !ok {
				errs = append(errs, fmt.Sprintf("api_keys[%q].allowed_tools[%d]: unknown mcp_servers[%q]", name, i, server))
			}
		}
	}
	return errs
}

// ValidateConfigReferences runs the reference/identity cross-checks that must
// hold at boot, on -validate, and on SIGHUP reload (NENYA-196): identity-
// referencing configuration (API-key agent scopes, agent MCP server
// references) must name sources that exist, and session-affinity settings must
// be in range. It takes both documents because the API-key scopes live in
// secrets while the referenced agents live in the config.
func ValidateConfigReferences(cfg *Config, secrets *SecretsConfig, logger *slog.Logger) []string {
	if cfg == nil {
		return nil
	}
	var errs []string
	errs = append(errs, validateStickySessionTTL(cfg.Agents)...)
	errs = append(errs, validateAgentNames(cfg.Agents)...)
	errs = append(errs, validateAgentBreakerThresholds(cfg.Agents)...)
	errs = append(errs, validateAgentSlowCallThresholds(cfg.Agents)...)
	errs = append(errs, validateAgentMCPReferences(cfg)...)
	errs = append(errs, validateSecretAgentScopes(cfg, secrets, logger)...)
	errs = append(errs, validateMCPServerConfigs(cfg, logger)...)
	errs = append(errs, validateAPIKeyToolGrants(cfg, secrets)...)
	return errs
}

func validateModelRegistryErrors(logger *slog.Logger) []string {
	errors := []string{}
	for modelID, entry := range ModelRegistry {
		if err := entry.Validate(); err != nil {
			errors = append(errors, fmt.Sprintf("model_registry[%q]: %v", modelID, err))
		}
	}
	return errors
}

func validateEntropyConfig(sf BouncerConfig) []string {
	if !sf.EntropyEnabled {
		return nil
	}

	errors := []string{}
	if sf.EntropyThreshold <= 0 || sf.EntropyThreshold > 8.0 {
		errors = append(errors, "bouncer.entropy_threshold must be between 0 and 8.0")
	}
	if sf.EntropyMinToken < 8 {
		errors = append(errors, "bouncer.entropy_min_token must be >= 8")
	}
	return errors
}

// validateModelsHiddenConfig rejects invalid models_hidden patterns at the
// RAW config level (NENYA-200): ResolveProviders skips a provider whose
// patterns fail to compile, so the resolved-map check never sees it — the
// raw pass is the real gate.
func validateModelsHiddenConfig(cfg *Config) []string {
	errors := []string{}
	for name, pc := range cfg.Providers {
		if _, err := CompileModelsHidden(pc.ModelsHidden); err != nil {
			errors = append(errors, fmt.Sprintf("providers[%q]: %v", name, err))
		}
	}
	return errors
}

// validateHeaderFilters rejects empty header names, non-canonical header
// names, and empty filter patterns in providers.<name>.header_filters
// (NENYA-201): an empty pattern would match every value (never what the
// operator meant), and a non-canonical name would never match a dispatch
// header (the rejection also rules out case-variant duplicates, since only
// one spelling per header is canonical).
func validateHeaderFilters(provider string, filters map[string]HeaderFilter) []string {
	errors := []string{}
	for header, filter := range filters {
		if header == "" {
			errors = append(errors, fmt.Sprintf("providers[%q].header_filters: header name must not be empty", provider))
			continue
		}
		if canonical := textproto.CanonicalMIMEHeaderKey(header); canonical != header {
			errors = append(errors, fmt.Sprintf("providers[%q].header_filters[%q]: use the canonical header name %q", provider, header, canonical))
		}
		for _, patterns := range [][]string{filter.Allow, filter.Deny} {
			for _, p := range patterns {
				if p == "" {
					errors = append(errors, fmt.Sprintf("providers[%q].header_filters[%q]: patterns must not be empty", provider, header))
				}
			}
		}
	}
	return errors
}

// headerTokenExtra are the non-alphanumeric characters an HTTP header field
// name may contain (RFC 9110 tchar).
const headerTokenExtra = "!#$%&'*+-.^_`|~"

// isValidHeaderToken reports whether name is a syntactically valid HTTP
// header field name (RFC 9110 token): the provider's idempotency_header is
// written straight onto an outbound request, so a malformed value must be
// rejected at startup rather than making every dispatch fail (NENYA-165).
func isValidHeaderToken(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		isAlnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !isAlnum && !strings.ContainsRune(headerTokenExtra, rune(c)) {
			return false
		}
	}
	return true
}

// reservedProviderHeaders are the outbound headers the gateway populates
// itself at dispatch. A provider declaring one of these as its idempotency
// header would have the key overwrite the gateway's own value (NENYA-165), so
// they are rejected at load. KEEP IN SYNC with the dispatch sets: the auth
// injectors (internal/adapter/*.InjectAuth — Authorization, x-api-key, api-key,
// x-goog-api-key, anthropic-version, HTTP-Referer/X-Title on OpenRouter),
// buildUpstreamRequest's transport/tracing/session-identity headers, its
// client passthrough allowlist, and this repository's own binding header.
// config cannot import internal/adapter, so the list is a mirror: a name
// missing here would clobber the gateway's value on every dispatch.
var reservedProviderHeaders = map[string]bool{
	"authorization":                true,
	"x-api-key":                    true,
	"api-key":                      true,
	"x-goog-api-key":               true,
	"anthropic-version":            true,
	"http-referer":                 true,
	"x-title":                      true,
	"content-type":                 true,
	"content-length":               true,
	"host":                         true,
	"user-agent":                   true,
	"accept-encoding":              true,
	"connection":                   true,
	"traceparent":                  true,
	"tracestate":                   true,
	"session-id":                   true,
	"session_id":                   true,
	"anthropic-beta":               true,
	"x-opencode-session":           true,
	"x-opencode-session-id":        true,
	"x-opencode-parent-session-id": true,
	"x-request-id":                 true,
	"x-correlation-id":             true,
	"x-trace-id":                   true,
	"x-nenya-idempotency-body":     true,
}

// validateProviderOptions validates the per-provider enum- and name-valued
// option fields on the RAW config, so it also runs for the paths that do not
// resolve providers (a `-validate` without ping, and the SIGHUP reload).
func validateProviderOptions(cfg *Config) []string {
	errors := []string{}
	for name, provider := range cfg.Providers {
		errors = append(errors, validateProviderOptionFields(name, &provider)...)
	}
	return errors
}

// validateProviderOptionFields validates one provider's option fields.
func validateProviderOptionFields(name string, provider *ProviderConfig) []string {
	var errors []string
	switch provider.CacheAffinity {
	case "", "body", "header":
		// valid (NENYA-158)
	default:
		errors = append(errors, fmt.Sprintf("providers[%q].cache_affinity must be body or header, got %q", name, provider.CacheAffinity))
	}
	if header := derefString(provider.IdempotencyHeader); header != "" {
		switch {
		case !isValidHeaderToken(header):
			errors = append(errors, fmt.Sprintf("providers[%q].idempotency_header %q is not a valid HTTP header name", name, header))
		case textproto.CanonicalMIMEHeaderKey(header) != header:
			errors = append(errors, fmt.Sprintf("providers[%q].idempotency_header %q: use the canonical header name %q", name, header, textproto.CanonicalMIMEHeaderKey(header)))
		case reservedProviderHeaders[strings.ToLower(header)]:
			errors = append(errors, fmt.Sprintf("providers[%q].idempotency_header %q is a header the gateway sets itself; the idempotency key would overwrite it", name, header))
		}
	}
	switch policy := derefString(provider.ThoughtSignaturePolicy); policy {
	case "", "strip", "placeholder", "passthrough":
		// valid (NENYA-50)
	default:
		errors = append(errors, fmt.Sprintf("providers[%q].thought_signature_policy must be one of strip, placeholder, passthrough, got %q", name, policy))
	}
	return errors
}

func validateProviders(ctx context.Context, providers map[string]*Provider, network *NetworkConfig, logger *slog.Logger) []string {
	errors := []string{}
	for name, provider := range providers {
		if provider.ResponseHeaderTimeoutSeconds < 0 {
			errors = append(errors, fmt.Sprintf("providers[%q].response_header_timeout_seconds must be non-negative (set to 0 to fall back to timeout_seconds or the 30s default), got %d", name, provider.ResponseHeaderTimeoutSeconds))
		}
		if provider.ResponseHeaderTimeoutSeconds > MaxTimeoutSeconds {
			errors = append(errors, fmt.Sprintf("providers[%q].response_header_timeout_seconds exceeds maximum allowed value (%d seconds / 24 hours), got %d", name, MaxTimeoutSeconds, provider.ResponseHeaderTimeoutSeconds))
		}
		if _, err := CompileNonChatModels(provider.NonChatModels); err != nil {
			errors = append(errors, fmt.Sprintf("providers[%q]: %v", name, err))
		}

		errors = append(errors, validateHeaderFilters(name, provider.HeaderFilters)...)
		if provider.StreamIdleTimeoutSeconds < 0 {
			errors = append(errors, fmt.Sprintf("providers[%q].stream_idle_timeout_seconds must be non-negative (set to 0 to use global default), got %d", name, provider.StreamIdleTimeoutSeconds))
		}
		if provider.StreamIdleTimeoutSeconds > 86400 {
			errors = append(errors, fmt.Sprintf("providers[%q].stream_idle_timeout_seconds exceeds maximum allowed value (86400 seconds / 24 hours), got %d", name, provider.StreamIdleTimeoutSeconds))
		}
		if b := provider.StreamBootstrapBufferBytes; b != nil {
			if *b < 0 {
				errors = append(errors, fmt.Sprintf("providers[%q].stream_bootstrap_buffer_bytes must be non-negative (nil = inherit global, 0 = off), got %d", name, *b))
			}
			if *b > MaxBootstrapBufferBytes {
				errors = append(errors, fmt.Sprintf("providers[%q].stream_bootstrap_buffer_bytes exceeds maximum allowed value (%d bytes), got %d", name, MaxBootstrapBufferBytes, *b))
			}
		}
		if provider.APIKey == "" {
			logger.Warn("provider has no API key configured", "provider", name)
			continue
		}

		logger.Info("validating provider", "provider", name, "url", provider.URL)
		if err := validateProvider(ctx, name, provider, network, logger); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", name, err))
			logger.Error("provider validation failed", "provider", name, "err", err)
		} else {
			logger.Info("provider validation passed", "provider", name)
		}
	}
	return errors
}

// validationClientFor builds an HTTP client honoring a provider's effective
// CA bundle and egress proxy (NENYA-137): validation pings must traverse the
// same network path as runtime dispatches. The Ollama engine probe passes
// the provider's explicit overrides; the local-engine probe passes empty
// (environment default; loopback is never proxied). The transport carries
// the explicit TLS floor.
func validationClientFor(caBundle, proxyURL string) (*http.Client, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
	if proxy, proxyErr := netutil.ProxyOrDefault(proxyURL); proxyErr == nil {
		transport.Proxy = proxy
	} // invalid explicit proxy: env default (startup validation rejects it first)
	if caBundle != "" {
		pool, err := netutil.LoadCABundle(caBundle)
		if err != nil {
			return nil, err
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: netutil.MinTLSVersion, RootCAs: pool}
	}
	netutil.ApplyTransportSecurity(transport)
	return &http.Client{Timeout: 30 * time.Second, Transport: transport}, nil
}

func validateProvider(ctx context.Context, name string, provider *Provider, network *NetworkConfig, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return validateWithMinimalRequest(ctx, provider, network, logger)
}

func validateWithMinimalRequest(ctx context.Context, provider *Provider, network *NetworkConfig, logger *slog.Logger) error {
	// Mirror the runtime proxy split: ollama-format providers are
	// explicit-only on the proxy (loopback-safe), everything else inherits
	// the fleet proxy; the fleet CA is inherited by both.
	proxyURL := provider.EffectiveProxyURL(network)
	if strings.EqualFold(provider.ApiFormat, "ollama") || strings.EqualFold(provider.Name, "ollama") {
		proxyURL = provider.ProxyURL
	}
	client, err := validationClientFor(provider.EffectiveCABundle(network), proxyURL)
	if err != nil {
		return fmt.Errorf("build validation client: %w", err)
	}
	defer client.CloseIdleConnections()

	buildRequest := func() (*http.Request, error) {
		// A fresh request per attempt: retries after a body-consuming
		// failure would otherwise send a 0-byte body against the original
		// ContentLength.
		payload := `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":false,"max_tokens":1}`
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, strings.NewReader(payload))
		if reqErr != nil {
			return nil, fmt.Errorf("failed to create request: %v", reqErr)
		}
		req.Header.Set("Content-Type", "application/json")
		if authErr := applyAuthHeader(req, provider); authErr != nil {
			return nil, fmt.Errorf("failed to apply authentication: %w", authErr)
		}
		return req, nil
	}

	resp, err := doWithRetryResp(ctx, 3, func() (*http.Response, error) {
		req, reqErr := buildRequest()
		if reqErr != nil {
			return nil, reqErr
		}
		r, doErr := client.Do(req)
		if doErr != nil {
			if r != nil {
				_ = r.Body.Close()
			}
			return nil, doErr
		}
		if r.StatusCode >= 500 {
			_ = r.Body.Close()
			return nil, fmt.Errorf("upstream error: %d", r.StatusCode)
		}
		return r, nil
	})
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API key rejected (HTTP 401)")
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

func applyAuthHeader(req *http.Request, provider *Provider) error {
	switch provider.AuthStyle {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	case "bearer+x-goog":
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		req.Header.Set("X-Goog-Api-Key", provider.APIKey)
	case "none":
	default:
		return fmt.Errorf("unsupported auth style: %s", provider.AuthStyle)
	}
	return nil
}

func validatePatternsToList(label string, patterns []string, logger *slog.Logger) []string {
	var errs []string
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			msg := fmt.Sprintf("%s[%d]: %v", label, i, err)
			errs = append(errs, msg)
			logger.Error("pattern compile failed", "label", label, "index", i, "pattern", p, "err", err)
		}
	}
	return errs
}

// ValidatePatterns compiles all patterns in the slice and returns an
// error if any pattern is invalid. Uses label in error messages for context.
func ValidatePatterns(label string, patterns []string, logger *slog.Logger) error {
	errs := validatePatternsToList(label, patterns, logger)
	if len(errs) > 0 {
		return fmt.Errorf("invalid patterns in %s:\n  - %s", label, strings.Join(errs, "\n  - "))
	}
	return nil
}

// OllamaHealthURL derives the health check URL (/api/tags) from an
// Ollama engine URL by stripping common suffixes (e.g., /v1/chat/completions,
// /api/generate).
func OllamaHealthURL(engineURL string) string {
	const nativeSuffix = "/api/generate"
	const openaiSuffix = "/v1/chat/completions"
	if strings.HasSuffix(engineURL, nativeSuffix) {
		return engineURL[:len(engineURL)-len(nativeSuffix)] + "/api/tags"
	}
	if strings.HasSuffix(engineURL, openaiSuffix) {
		return engineURL[:len(engineURL)-len(openaiSuffix)] + "/api/tags"
	}
	return engineURL
}

func validateOllamaHealth(ctx context.Context, ollamaURL, caBundle, proxyURL string) bool {
	healthURL := OllamaHealthURL(ollamaURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, http.NoBody)
	if err != nil {
		return false
	}
	validationHTTPClient, clientErr := validationClientFor(caBundle, proxyURL)
	if clientErr != nil {
		return false
	}
	defer validationHTTPClient.CloseIdleConnections()
	resp, err := validationHTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer closeBody(resp)
	return resp.StatusCode == http.StatusOK
}

func doWithRetry(ctx context.Context, maxAttempts int, fn func() error) error {
	if maxAttempts <= 1 {
		return fn()
	}

	var lastErr error
	for attempt := range maxAttempts {
		if err := fn(); err != nil {
			lastErr = err
			if attempt == maxAttempts-1 {
				return lastErr
			}
			backoff := calculateBackoff(attempt)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			continue
		}
		return nil
	}
	return lastErr
}

// doWithRetryResp is like doWithRetry but for HTTP functions returning
// (*http.Response, error). Callback must close body on error and return
// response on success.
func doWithRetryResp(ctx context.Context, maxAttempts int, fn func() (*http.Response, error)) (*http.Response, error) {
	if maxAttempts <= 1 {
		return fn()
	}

	var lastErr error
	for attempt := range maxAttempts {
		resp, err := fn()
		if err != nil {
			if resp != nil {
				_ = resp.Body.Close()
			}
			lastErr = err
			if attempt == maxAttempts-1 {
				return nil, lastErr
			}
			backoff := calculateBackoff(attempt)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			}
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func calculateBackoff(attempt int) time.Duration {
	base := 500 * time.Millisecond
	maxDelay := 8 * time.Second
	delay := base
	for range attempt {
		delay *= 2
		if delay >= maxDelay {
			return maxDelay
		}
	}
	jitter := time.Duration(rand.Int63n(int64(delay / 10)))
	return delay + jitter
}

func validateProviderRateLimits(cfg *Config) []string {
	var errs []string
	for name, p := range cfg.Providers {
		if p.RatelimitMaxRPM != nil && *p.RatelimitMaxRPM < 0 {
			errs = append(errs, fmt.Sprintf("providers[%q].ratelimit_max_rpm must be non-negative, got %d", name, *p.RatelimitMaxRPM))
		}
		if p.RatelimitMaxTPM != nil && *p.RatelimitMaxTPM < 0 {
			errs = append(errs, fmt.Sprintf("providers[%q].ratelimit_max_tpm must be non-negative, got %d", name, *p.RatelimitMaxTPM))
		}
		if p.MaxConcurrentRequests < 0 {
			errs = append(errs, fmt.Sprintf("providers[%q].max_concurrent_requests must be non-negative, got %d", name, p.MaxConcurrentRequests))
		}
		for model, limit := range p.ModelConcurrency {
			if model == "" {
				errs = append(errs, fmt.Sprintf("providers[%q].model_concurrency contains an empty model key", name))
			}
			if limit < 0 {
				errs = append(errs, fmt.Sprintf("providers[%q].model_concurrency[%q] must be non-negative, got %d", name, model, limit))
			}
		}
	}
	errs = append(errs, validateProviderModelAliases(cfg.Providers)...)
	if cfg.Governance.MaxConcurrentRequests < 0 {
		errs = append(errs, fmt.Sprintf("governance.max_concurrent_requests must be non-negative, got %d", cfg.Governance.MaxConcurrentRequests))
	}
	return errs
}

// validateProviderModelAliases checks the per-provider model alias maps:
// canonical keys and physical IDs must be non-empty.
func validateProviderModelAliases(providers map[string]ProviderConfig) []string {
	var errs []string
	for name, p := range providers {
		for canonical, physical := range p.ModelAliases {
			if canonical == "" {
				errs = append(errs, fmt.Sprintf("providers[%q].model_aliases contains an empty canonical key", name))
			}
			if physical == "" {
				errs = append(errs, fmt.Sprintf("providers[%q].model_aliases[%q] has an empty physical model ID", name, canonical))
			}
		}
	}
	return errs
}

// validateKeyLimits checks per-key rate-limit/budget fields and per-provider
// budget fields (NENYA-20).
func validateKeyLimits(cfg *Config, secrets *SecretsConfig) []string {
	var errs []string
	if secrets != nil {
		for name, k := range secrets.ApiKeys {
			if k.RatelimitMaxRPM < 0 {
				errs = append(errs, fmt.Sprintf("api_keys[%q].ratelimit_max_rpm must be non-negative, got %d", name, k.RatelimitMaxRPM))
			}
			if k.TokenBudgetDaily < 0 {
				errs = append(errs, fmt.Sprintf("api_keys[%q].token_budget_daily must be non-negative, got %d", name, k.TokenBudgetDaily))
			}
			switch k.BudgetTier {
			case "", "always", "fill":
			default:
				errs = append(errs, fmt.Sprintf("api_keys[%q].budget_tier: invalid value %q, must be one of always, fill", name, k.BudgetTier))
			}
		}
	}
	for name, p := range cfg.Providers {
		if p.TokenBudgetDaily < 0 {
			errs = append(errs, fmt.Sprintf("providers[%q].token_budget_daily must be non-negative, got %d", name, p.TokenBudgetDaily))
		}
		for _, w := range p.PeakWindows {
			if err := w.Validate(); err != nil {
				errs = append(errs, fmt.Sprintf("providers[%q].peak_windows: %v", name, err))
			}
		}
	}
	return errs
}

func validateBillingConfig(cfg *Config) []string {
	var errs []string

	validModes := map[string]bool{
		"economy":  true,
		"balanced": true,
		"quality":  true,
	}

	if cfg.Governance.CostMode != "" {
		if !validModes[cfg.Governance.CostMode] {
			errs = append(errs, fmt.Sprintf("governance.cost_mode: invalid value %q, must be one of economy, balanced, quality", cfg.Governance.CostMode))
		}
	}

	if cfg.Governance.BillingEconomyScale < 0 {
		errs = append(errs, fmt.Sprintf("governance.billing_economy_scale must be non-negative, got %f", cfg.Governance.BillingEconomyScale))
	}

	if cfg.Governance.BillingQualityScale < 0 {
		errs = append(errs, fmt.Sprintf("governance.billing_quality_scale must be non-negative, got %f", cfg.Governance.BillingQualityScale))
	}

	for name, provider := range cfg.Providers {
		if provider.Billing == nil {
			continue
		}
		if provider.Billing.Model == "" {
			errs = append(errs, fmt.Sprintf("providers[%q].billing.model: billing model must be set", name))
		}

		validModels := map[string]bool{
			"subscription": true,
			"credit":       true,
			"free":         true,
			"mixed":        true,
		}
		if provider.Billing.Model != "" && !validModels[string(provider.Billing.Model)] {
			errs = append(errs, fmt.Sprintf("providers[%q].billing.model: invalid value %q, must be one of subscription, credit, free, mixed", name, provider.Billing.Model))
		}

		if provider.Billing.FreeOnly && provider.Billing.Model != "mixed" {
			errs = append(errs, fmt.Sprintf("providers[%q].billing.free_only requires billing.model to be \"mixed\"", name))
		}

		validateProviderBillingConfig(name, provider, &errs)
	}

	return errs
}

func validateUpstreamTimeoutSeconds(v *int) []string {
	if v != nil && *v < 0 {
		return []string{fmt.Sprintf("governance.upstream_timeout_seconds must be non-negative (set to 0 for unlimited), got %d", *v)}
	}
	return nil
}

// validateSlowCallThresholdSeconds rejects a negative or over-long slow-call
// threshold (NENYA-153) instead of silently reinterpreting it: 0 disables the
// feature, so a negative value is a configuration mistake, and an unbounded
// threshold would make the breaker unreachable by design.
func validateSlowCallThresholdSeconds(v *int, field string) []string {
	if v == nil {
		return nil
	}
	if *v < 0 {
		return []string{fmt.Sprintf("%s must be non-negative (set to 0 to disable slow-call breaking), got %d", field, *v)}
	}
	if *v > MaxSlowCallThresholdSeconds {
		return []string{fmt.Sprintf("%s exceeds maximum allowed value (%d seconds / 24 hours), got %d", field, MaxSlowCallThresholdSeconds, *v)}
	}
	return nil
}

// MaxBreakerThreshold caps agents.<name>.failure_threshold/success_threshold:
// beyond uint32 range the breaker's counter arithmetic would wrap (CWE-190),
// and anything near it is a configuration mistake rather than an intent.
const MaxBreakerThreshold = 1 << 30

// validateAgentBreakerThresholds rejects negative or over-range per-agent
// breaker thresholds (NENYA-204) instead of silently reinterpreting them:
// 0/omitted inherits the defaults, so a negative value is a configuration
// mistake, and an out-of-range value would wrap the uint32 conversion.
func validateAgentBreakerThresholds(agents map[string]AgentConfig) []string {
	var errs []string
	for name, agent := range agents {
		for field, value := range map[string]int{
			"failure_threshold": agent.FailureThreshold,
			"success_threshold": agent.SuccessThreshold,
		} {
			if value < 0 {
				errs = append(errs, fmt.Sprintf("agents[%q].%s must be non-negative (0 inherits the default), got %d", name, field, value))
			}
			if value > MaxBreakerThreshold {
				errs = append(errs, fmt.Sprintf("agents[%q].%s exceeds the maximum allowed value (%d), got %d", name, field, MaxBreakerThreshold, value))
			}
		}
	}
	return errs
}

// validateAgentSlowCallThresholds applies the slow-call threshold rules to every
// agent override (NENYA-153).
func validateAgentSlowCallThresholds(agents map[string]AgentConfig) []string {
	var errs []string
	for name, agent := range agents {
		errs = append(errs, validateSlowCallThresholdSeconds(
			agent.SlowCallThresholdSeconds,
			fmt.Sprintf("agents[%q].slow_call_threshold_seconds", name),
		)...)
	}
	return errs
}

// validateStreamStallTimeoutSeconds pins the NENYA-155 knob: nil = the 60s
// default, an explicit 0 disables (the server-wide WriteTimeout stays 0),
// 86400 (24h) is the cap.
func validateStreamStallTimeoutSeconds(v *int) []string {
	if v == nil {
		return nil
	}
	if *v < 0 {
		return []string{fmt.Sprintf("server.stream_stall_timeout_seconds must be non-negative (set to 0 to disable the per-write deadline), got %d", *v)}
	}
	if *v > 86400 {
		return []string{fmt.Sprintf("server.stream_stall_timeout_seconds exceeds maximum allowed value (86400 seconds / 24 hours), got %d", *v)}
	}
	return nil
}

func validateStreamIdleTimeoutSeconds(v *int) []string {
	if v == nil {
		return nil
	}
	if *v < 0 {
		return []string{fmt.Sprintf("governance.stream_idle_timeout_seconds must be non-negative (set to 0 to disable stall detection), got %d", *v)}
	}
	if *v > 86400 {
		return []string{fmt.Sprintf("governance.stream_idle_timeout_seconds exceeds maximum allowed value (86400 seconds / 24 hours), got %d", *v)}
	}
	return nil
}

func validateThinkingStreamIdleTimeoutSeconds(v *int) []string {
	if v == nil {
		return nil
	}
	if *v < 0 {
		return []string{fmt.Sprintf("governance.thinking_stream_idle_timeout_seconds must be non-negative (set to 0 to disable thinking-aware extension), got %d", *v)}
	}
	if *v > 86400 {
		return []string{fmt.Sprintf("governance.thinking_stream_idle_timeout_seconds exceeds maximum allowed value (86400 seconds / 24 hours), got %d", *v)}
	}
	return nil
}

// MaxBootstrapBufferBytes caps the stream bootstrap buffering budget so a
// misconfigured value cannot pin unbounded memory per streaming request.
const MaxBootstrapBufferBytes = 1 << 20

func validateBootstrapBufferBytes(v int) []string {
	if v < 0 {
		return []string{fmt.Sprintf("governance.stream_bootstrap_buffer_bytes must be non-negative (0 disables bootstrap buffering), got %d", v)}
	}
	if v > MaxBootstrapBufferBytes {
		return []string{fmt.Sprintf("governance.stream_bootstrap_buffer_bytes exceeds maximum allowed value (%d bytes), got %d", MaxBootstrapBufferBytes, v)}
	}
	return nil
}

func validateProviderBillingConfig(name string, provider ProviderConfig, errs *[]string) {
	if provider.Billing == nil {
		return
	}
	bc := provider.Billing
	if bc.Model == "" {
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.model: billing model must be set", name))
	}

	validModels := map[string]bool{
		"subscription": true,
		"credit":       true,
		"free":         true,
		"mixed":        true,
	}
	if bc.Model != "" && !validModels[string(bc.Model)] {
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.model: invalid value %q, must be one of subscription, credit, free, mixed", name, bc.Model))
	}

	if bc.FreeOnly && bc.Model != "mixed" {
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.free_only requires billing.model to be \"mixed\"", name))
	}

	validateQuotaSource(name, bc, errs)
	validateQuotaExtractionConfig(name, bc, errs)
	validateQuotaTimeoutSeconds(name, bc, errs)
	validateQuotaBackoffMaxSeconds(name, bc, errs)
}

func validateQuotaSource(name string, billingCfg *BillingConfig, errs *[]string) {
	switch billingCfg.QuotaSource {
	case "api":
		if billingCfg.QuotaURL == "" {
			*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_url: required when quota_source is \"api\"", name))
		}
		if billingCfg.QuotaExtraction == nil {
			*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_extraction: required when quota_source is \"api\"", name))
		}
	case "headers":
		if billingCfg.QuotaExtraction == nil {
			*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_extraction: required when quota_source is \"headers\"", name))
		}
	case "none", "":
	default:
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_source: invalid value %q, must be one of none, api, headers", name, billingCfg.QuotaSource))
	}
}

func validateQuotaExtractionConfig(name string, billingCfg *BillingConfig, errs *[]string) {
	if billingCfg.QuotaExtraction == nil {
		return
	}
	validModes := map[string]bool{
		"simple_json":    true,
		"max_from_array": true,
		"headers":        true,
	}
	if !validModes[string(billingCfg.QuotaExtraction.Mode)] {
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_extraction.mode: invalid value %q, must be one of simple_json, max_from_array, headers", name, billingCfg.QuotaExtraction.Mode))
	}
}

func validateQuotaTimeoutSeconds(name string, billingCfg *BillingConfig, errs *[]string) {
	if billingCfg.QuotaTimeoutSeconds < 0 {
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_timeout_seconds must be non-negative, got %d", name, billingCfg.QuotaTimeoutSeconds))
	}
}

func validateQuotaBackoffMaxSeconds(name string, billingCfg *BillingConfig, errs *[]string) {
	if billingCfg.QuotaBackoffMaxSeconds < 0 {
		*errs = append(*errs, fmt.Sprintf("providers[%q].billing.quota_backoff_max_seconds must be non-negative, got %d", name, billingCfg.QuotaBackoffMaxSeconds))
	}
}

// validateExfilGuardConfig checks the egress-guard action value, budget
// floor, and per-agent override surface (only enabled/action are
// per-agent; the host list, query cap, and IP-literal policy are
// global-only).
func validateExfilGuardConfig(cfg *Config) []string {
	var errs []string
	if g := cfg.Governance.ExfilGuard; g != nil {
		switch g.Action {
		case "", ExfilActionLog, ExfilActionStrip, ExfilActionBlock:
		default:
			errs = append(errs, fmt.Sprintf("governance.exfil_guard.action: invalid value %q, must be empty, \"log\", \"strip\", or \"block\"", g.Action))
		}
		if g.MaxQueryChars < 0 {
			errs = append(errs, "governance.exfil_guard.max_query_chars must be >= 0")
		}
		for i, host := range g.AllowedHosts {
			if strings.ContainsAny(host, "/ :") {
				errs = append(errs, fmt.Sprintf("governance.exfil_guard.allowed_hosts[%d]: %q is not a bare hostname (scheme, path, port, or space)", i, host))
			}
		}
	}
	for name, agent := range cfg.Agents {
		if agent.ExfilGuard == nil {
			continue
		}
		if cfg.Governance.ExfilGuard == nil {
			errs = append(errs, fmt.Sprintf("agents.%s.exfil_guard requires governance.exfil_guard to be configured (the per-agent surface is an override)", name))
		}
		switch agent.ExfilGuard.Action {
		case "", ExfilActionLog, ExfilActionStrip, ExfilActionBlock:
		default:
			errs = append(errs, fmt.Sprintf("agents.%s.exfil_guard.action: invalid value %q", name, agent.ExfilGuard.Action))
		}
	}
	return errs
}

// validateMCPGuardConfig checks the tool-argument guard surface: URL
// policy vocabulary and a positive size cap, plus per-server allowlist
// entry shapes (hostname or "*.suffix").
func validateMCPGuardConfig(cfg *Config) []string {
	var errs []string
	if g := cfg.Governance.MCPGuard; g != nil {
		switch g.URLPolicy {
		case "", "deny_private", "log", "off":
		default:
			errs = append(errs, fmt.Sprintf("governance.mcp_guard.url_policy: invalid value %q, must be empty, \"deny_private\", \"log\", or \"off\"", g.URLPolicy))
		}
		if g.MaxArgBytes < 0 {
			errs = append(errs, "governance.mcp_guard.max_arg_bytes: must be >= 0")
		}
	}
	for name, server := range cfg.MCPServers {
		for _, entry := range server.AllowedHosts {
			host := strings.ToLower(strings.TrimSpace(entry))
			if host == "" {
				errs = append(errs, fmt.Sprintf("mcp_servers.%s.allowed_hosts: empty entry", name))
				continue
			}
			wildcard := strings.HasPrefix(host, "*.")
			host = strings.TrimPrefix(host, "*.")
			if host == "" {
				errs = append(errs, fmt.Sprintf("mcp_servers.%s.allowed_hosts: entry %q has an empty wildcard suffix", name, entry))
				continue
			}
			if strings.Contains(host, "*") || strings.Contains(host, "/") || strings.Contains(host, " ") || strings.Contains(host, ":") {
				errs = append(errs, fmt.Sprintf("mcp_servers.%s.allowed_hosts: entry %q must be a hostname or \"*.suffix\"", name, entry))
				continue
			}
			if wildcard && (net.ParseIP(host) != nil || isNumericHost(host)) {
				errs = append(errs, fmt.Sprintf("mcp_servers.%s.allowed_hosts: entry %q must not be an IP or all-numeric suffix", name, entry))
				continue
			}
		}
	}
	return errs
}

// validateCanaryConfig checks the tripwire action value (a typo would
// silently degrade block to log at runtime).
func validateCanaryConfig(cfg *Config) []string {
	if cfg.Governance.Canary == nil {
		return nil
	}
	switch cfg.Governance.Canary.Action {
	case "", CanaryActionLog, CanaryActionBlock:
		return nil
	default:
		return []string{fmt.Sprintf("governance.canary.action: invalid value %q, must be empty, \"log\", or \"block\"", cfg.Governance.Canary.Action)}
	}
}

// isNumericHost reports whether every dot-separated label parses as a
// number in the inet_aton vocabularies (decimal, 0x-hex, 0-octal).
func isNumericHost(host string) bool {
	if host == "" {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !isNumericHostLabel(label) {
			return false
		}
	}
	return true
}

// isNumericHostLabel reports whether one label parses as a number:
// decimal, 0x-hex, or 0-octal. It deliberately rejects MORE broadly
// than internal/mcp's parseNumericIPv4 (which additionally applies
// byte-range normalization): config validation only needs "could this
// suffix spell an IP literal", and over-rejection is the safe
// direction. A shared helper is not possible without an import cycle
// (config is a leaf that internal/util already depends on).
func isNumericHostLabel(label string) bool {
	if label == "" {
		return false
	}
	base, digits := 10, label
	switch {
	case strings.HasPrefix(label, "0x") || strings.HasPrefix(label, "0X"):
		base, digits = 16, label[2:]
	case label[0] == '0' && len(label) > 1:
		base, digits = 8, label[1:]
	}
	if digits == "" {
		return false
	}
	_, err := strconv.ParseUint(digits, base, 32)
	return err == nil
}

// serverHasHeader reports whether headers carries name in any spelling
// (HTTP header names are case-insensitive, RFC 9110 §5.1).
func serverHasHeader(headers map[string]string, name string) bool {
	for k, v := range headers {
		if strings.EqualFold(k, name) && strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// validateAgentNames rejects agent names that cannot round-trip through the
// circuit key `agent:provider:model` (AUD-40 / NENYA-238): a colon in the
// agent name made both positional key parsers misread the key — the
// per-agent breaker threshold lookup (first field) missed the agent and
// silently fell back to the defaults, and the backoff metric labels
// (SplitN 3) scrambled agent/provider/model. Empty names are rejected for
// the same reason: they cannot be distinguished from the unset case.
func validateAgentNames(agents map[string]AgentConfig) []string {
	var errs []string
	for name := range agents {
		if name == "" {
			errs = append(errs, "agent name must not be empty")
			continue
		}
		if strings.Contains(name, ":") {
			errs = append(errs, fmt.Sprintf("agents[%q]: agent name must not contain %q — the circuit key agent:provider:model uses colons as separators, so per-agent breaker thresholds and backoff metrics would silently misattribute (rename the agent)", name, ":"))
		}
	}
	return errs
}
