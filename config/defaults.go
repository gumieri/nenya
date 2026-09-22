package config

import (
	"fmt"
	"log/slog"
)

// ParseLogLevel maps a log level string to a slog level, reporting whether
// the value was a recognized level name ("debug", "info", "warn", "error").
// Single source of truth for level parsing, shared by strict (config
// validation) and lenient (logger setup) call sites.
func ParseLogLevel(level string) (slog.Level, bool) {
	switch level {
	case "debug":
		return slog.LevelDebug, true
	case "info":
		return slog.LevelInfo, true
	case "warn":
		return slog.LevelWarn, true
	case "error":
		return slog.LevelError, true
	default:
		return slog.LevelInfo, false
	}
}

func applyEngineRefDefaults(e *EngineRef) {
	if e.AgentName == "" {
		if e.Provider == "" {
			e.Provider = "ollama"
		}
		if e.Model == "" {
			e.Model = "qwen2.5-coder:7b"
		}
		if e.TimeoutSeconds == 0 {
			e.TimeoutSeconds = 60
		}
	}
}

func applyResolvedEngineDefaults(targets []EngineTarget) {
	for i := range targets {
		if targets[i].Engine.Provider == "" {
			targets[i].Engine.Provider = "ollama"
		}
		if targets[i].Engine.Model == "" {
			targets[i].Engine.Model = "qwen2.5-coder:7b"
		}
		if targets[i].Engine.TimeoutSeconds == 0 {
			targets[i].Engine.TimeoutSeconds = 60
		}
	}
}

// ApplyDefaults populates all unset configuration fields with sensible
// defaults, resolves engine references, and applies built-in providers.
func ApplyDefaults(cfg *Config) error {
	applyServerDefaults(cfg)
	applyContextDefaults(cfg)
	applyGovernanceDefaults(cfg)
	applyBouncerDefaults(cfg)
	applyEngineRefDefaults(&cfg.Bouncer.Engine)
	applyEngineRefDefaults(&cfg.Window.Engine)
	if err := applyPrefixCacheDefaults(cfg); err != nil {
		return err
	}
	applyCompactionDefaults(cfg)
	applyResponseCacheDefaults(cfg)
	applyProviderMapDefaults(cfg)
	applyLocalEngineDefaults(cfg)
	if err := applyAgentDefaults(cfg); err != nil {
		return err
	}
	if err := applyProviderModelsDefaults(cfg); err != nil {
		return err
	}
	applyBuiltInProviders(cfg)
	applyWindowDefaults(cfg)
	if err := resolveEngineRefs(cfg); err != nil {
		return err
	}
	applyResolvedEngineDefaults(cfg.Bouncer.Engine.ResolvedTargets)
	applyResolvedEngineDefaults(cfg.Window.Engine.ResolvedTargets)
	return nil
}

func applyServerDefaults(cfg *Config) {
	if cfg.Server.ListenAddr == "" {
		cfg.Server.ListenAddr = ":8080"
	}
	if cfg.Server.MaxBodyBytes == 0 {
		cfg.Server.MaxBodyBytes = 10 << 20
	}
	if cfg.Server.UserAgent == "" {
		cfg.Server.UserAgent = "github.com/nenya/1.0"
	}
	if (cfg.Server.SecureMemoryRequired == nil || !*cfg.Server.SecureMemoryRequired) && !cfg.Server.SecureMemoryRequiredWasSet() {
		cfg.Server.SecureMemoryRequired = PtrTo(true)
	}
}

func applyContextDefaults(cfg *Config) {
	if cfg.Context.TruncationStrategy == "" {
		cfg.Context.TruncationStrategy = "middle-out"
	}
	if cfg.Context.TruncationKeepFirstPct == 0 {
		cfg.Context.TruncationKeepFirstPct = 15.0
	}
	if cfg.Context.TruncationKeepLastPct == 0 {
		cfg.Context.TruncationKeepLastPct = 25.0
	}
	// HardLimitTokens defaults to 0 (backward-compat softLimit*2)
	// If 0, interceptContent uses softLimit * 2 as hard limit.
	if cfg.Context.HardLimitTokens < 0 {
		cfg.Context.HardLimitTokens = 0
	}
}

func applyGovernanceDefaults(cfg *Config) {
	// Note: server.log_level validity is enforced by cmd/nenya via
	// ParseLogLevel; ApplyDefaults stays level-agnostic so an invalid
	// level can never skip governance defaults.
	if !cfg.Governance.TPMSet() && cfg.Governance.RatelimitMaxTPM == nil {
		cfg.Governance.RatelimitMaxTPM = PtrTo(250000)
	}
	if !cfg.Governance.RPMSet() && cfg.Governance.RatelimitMaxRPM == nil {
		cfg.Governance.RatelimitMaxRPM = PtrTo(15)
	}
	if len(cfg.Governance.BlockedExecutionPatterns) == 0 {
		cfg.Governance.BlockedExecutionPatterns = []string{
			`(?i)\brm\s+-[a-zA-Z]*[rR][a-zA-Z]*\s+.*(/|\*)`,
			`(?i)\bchmod\s+(?:-R\s+)?777\b`,
			`(?i)\bmkfs\.`,
			`(?i)\bterraform\s+destroy\b`,
			`(?i)\bterragrunt\s+destroy\b`,
			`(?i)\baws\s+s3\s+rb\s+.*--force`,
			`(?i)\baws\s+ec2\s+terminate-instances\b`,
			`(?i)\bkubectl\s+delete\s+(namespace|ns|pv|pvc|crd)\b`,
			`(?i)\bhelm\s+(uninstall|delete)\b`,
			`(?i)\b(DROP|TRUNCATE)\s+(TABLE|DATABASE|SCHEMA)\b`,
			`(?i)\b(shutdown|reboot|poweroff|halt|init\s+0)\b`,
		}
	}
	applyStreamHeadDefaults(&cfg.Governance)
	if cfg.Governance.CostMode == "" {
		cfg.Governance.CostMode = "balanced"
	}
	if cfg.Governance.BillingEconomyScale == 0 {
		cfg.Governance.BillingEconomyScale = 1.5
	}
	if cfg.Governance.BillingQualityScale == 0 {
		cfg.Governance.BillingQualityScale = 0.0
	}
	if cfg.Governance.StreamContinuation == nil {
		cfg.Governance.StreamContinuation = &StreamContinuationConfig{}
	}
	if cfg.Governance.StreamContinuation.Enabled == nil {
		cfg.Governance.StreamContinuation.Enabled = PtrTo(true)
	}
	if cfg.Governance.StreamContinuation.MaxAttempts <= 0 {
		cfg.Governance.StreamContinuation.MaxAttempts = 2
	}
	if cfg.Governance.StreamContinuation.SameModelOnly == nil {
		cfg.Governance.StreamContinuation.SameModelOnly = PtrTo(true)
	}
}

// applyStreamHeadDefaults ensures the stream-head detection flags used by the
// proxy's pre-header probe (empty-stream detection and early error failover)
// default to enabled.
func applyStreamHeadDefaults(gc *GovernanceConfig) {
	if !gc.EmptyStreamAsErrorSet() {
		gc.EmptyStreamAsError = PtrTo(true)
	}
	if !gc.EarlyStreamErrorFailoverSet() {
		gc.EarlyStreamErrorFailover = PtrTo(true)
	}
}

// Financial-identifier regex sources are exported so the pipeline package
// can attach checksum validators (checksum.go) keyed to exactly these
// sources — re.String() round-trips the pattern source, so any edit to a
// constant here must keep the validator mapping in sync. All patterns are
// anchored at word boundaries: identifiers embedded in adjacent word
// characters (e.g. "x4111111111111111") are not matched — an accepted
// Tier-0 regex redaction limitation.
const (
	// FinancialCardPattern matches 13-19 digit card-shaped numbers with
	// optional space/dash separators; gated by Luhn validation.
	FinancialCardPattern = `\b\d(?:[ -]?\d){12,18}\b`
	// FinancialIBANPattern matches contiguous and single-space-separated
	// IBANs (case-insensitive); gated by ISO 13616 mod-97 validation.
	FinancialIBANPattern = `(?i)\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){2,7}(?: ?[A-Z0-9]{1,4})?\b`
	// FinancialCPFPattern matches formatted Brazilian CPF numbers; gated
	// by check-digit validation.
	FinancialCPFPattern = `\b\d{3}\.\d{3}\.\d{3}-\d{2}\b`
	// FinancialCNPJPattern matches formatted Brazilian CNPJ numbers; gated
	// by check-digit validation.
	FinancialCNPJPattern = `\b\d{2}\.\d{3}\.\d{3}/\d{4}-\d{2}\b`
)

// RedactPresetFinancial returns the financial preset source list. It
// returns a fresh copy so callers cannot mutate the package-level preset
// backing redactPresets.
func RedactPresetFinancial() []string {
	return []string{
		FinancialIBANPattern,
		FinancialCardPattern,
		FinancialCPFPattern,
		FinancialCNPJPattern,
	}
}

var redactPresets = map[string][]string{
	"credentials": {
		`(?i)AKIA[0-9A-Z]{16}`,
		`(?i)gh(p|o|s)_[a-zA-Z0-9]{36,255}`,
		`(?i)ya29\.[0-9A-Za-z\-_]+`,
		`(?i)sk-[a-zA-Z0-9]{48}`,
		`(?i)-----BEGIN\s+(RSA\s+)?(DSA\s+)?(EC\s+)?PRIVATE\s+KEY\s*-----`,
		`(?i)(aws_access_key_id|aws_secret_access_key)\s*=\s*['"][^'"]{10,}['"]`,
		`(?i)(password|passwd|pwd|secret|token)[\s:=]+['"][^'"]{6,}['"]`,
		`[a-f0-9]{32}:`,
		`(?i)SG\.[a-zA-Z0-9\-_]{22}\.[a-zA-Z0-9\-_]{43}`,
		`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]*`,
	},
	"financial": RedactPresetFinancial(),
	"pii": {
		`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b`,
		`\b\d{3}-\d{2}-\d{4}\b`,
		`\b(?:\d{4}[ -]?){3}\d{4}\b`,
		`\+\d{1,3}\s?\(?\d{3}\)?[\s.-]?\d{3}[\s.-]?\d{4}`,
	},
}

func expandRedactPreset(cfg *BouncerConfig) {
	if cfg.RedactPreset != "" && cfg.RedactPatterns == nil {
		patterns, ok := redactPresets[cfg.RedactPreset]
		if ok {
			// Copy so callers mutating their RedactPatterns cannot corrupt
			// the package-level preset backing array.
			cfg.RedactPatterns = append([]string(nil), patterns...)
			return
		}
		slog.Warn("unknown bouncer.redact_preset, falling back to credentials patterns", "preset", cfg.RedactPreset)
	}
}

func applyBouncerDefaults(cfg *Config) {
	expandRedactPreset(&cfg.Bouncer)
	if cfg.Bouncer.RedactPatterns == nil {
		if !cfg.Bouncer.EnabledWasSet() {
			cfg.Bouncer.Enabled = PtrTo(true)
		}
		// Copy so callers mutating their RedactPatterns cannot corrupt
		// the package-level preset backing array.
		cfg.Bouncer.RedactPatterns = append([]string(nil), redactPresets["credentials"]...)
	} else if !cfg.Bouncer.EnabledWasSet() {
		cfg.Bouncer.Enabled = PtrTo(true)
	}
	if cfg.Bouncer.RedactionLabel == "" {
		cfg.Bouncer.RedactionLabel = "[REDACTED]"
	}
	if cfg.Bouncer.RedactOutputWindow == 0 {
		cfg.Bouncer.RedactOutputWindow = 4096
	}
	if !cfg.Bouncer.FailOpenWasSet() {
		cfg.Bouncer.FailOpen = PtrTo(true)
	}
	if cfg.Bouncer.EntropyThreshold == 0 {
		cfg.Bouncer.EntropyThreshold = 4.5
	}
	if cfg.Bouncer.EntropyMinToken == 0 {
		cfg.Bouncer.EntropyMinToken = 20
	}
}

func applyPrefixCacheDefaults(cfg *Config) error {
	if !cfg.PrefixCache.Enabled {
		cfg.PrefixCache.Enabled = anyCacheFeatureSet(cfg)
	}
	if (cfg.PrefixCache.PinSystemFirst == nil || !*cfg.PrefixCache.PinSystemFirst) && !cfg.PrefixCache.PinWasSet() {
		cfg.PrefixCache.PinSystemFirst = PtrTo(true)
	}
	if (cfg.PrefixCache.StableTools == nil || !*cfg.PrefixCache.StableTools) && !cfg.PrefixCache.StableWasSet() {
		cfg.PrefixCache.StableTools = PtrTo(true)
	}
	if !cfg.PrefixCache.SkipRedactionWasSet() {
		cfg.PrefixCache.SkipRedactionOnSystem = PtrTo(false)
	}
	return applyCacheControlDefaults(cfg)
}

func applyCacheControlDefaults(cfg *Config) error {
	if !cfg.PrefixCache.Enabled {
		return nil
	}
	if err := applyCacheModeDefaults(cfg); err != nil {
		return err
	}
	if cfg.PrefixCache.CacheControlTTL == "" {
		cfg.PrefixCache.CacheControlTTL = "ephemeral"
	}
	// Unset per-breakpoint TTLs intentionally alias the global CacheControlTTL
	// field so they always reflect the effective global value (no copy drift).
	if !cfg.PrefixCache.CacheSystemTTLWasSet() {
		cfg.PrefixCache.CacheSystemTTL = &cfg.PrefixCache.CacheControlTTL
	}
	if !cfg.PrefixCache.CacheToolsTTLWasSet() {
		cfg.PrefixCache.CacheToolsTTL = &cfg.PrefixCache.CacheControlTTL
	}
	if !cfg.PrefixCache.CacheMessagesTTLWasSet() {
		cfg.PrefixCache.CacheMessagesTTL = &cfg.PrefixCache.CacheControlTTL
	}

	if err := validateCacheTTLs(cfg); err != nil {
		return err
	}
	if err := validateCacheTTLOrdering(cfg); err != nil {
		return err
	}
	return applyOpenAICacheDefaults(cfg)
}

func applyCacheModeDefaults(cfg *Config) error {
	if !cfg.PrefixCache.CacheModeWasSet() {
		cfg.PrefixCache.CacheMode = PtrTo(CacheModeExplicit)
	}
	if cfg.PrefixCache.CacheMode == nil {
		return fmt.Errorf("cache_mode is nil after WasSet check")
	}
	if *cfg.PrefixCache.CacheMode != CacheModeExplicit && *cfg.PrefixCache.CacheMode != CacheModeAutomatic {
		return fmt.Errorf("invalid cache_mode: %q (must be `%s` or `%s`)", *cfg.PrefixCache.CacheMode, CacheModeExplicit, CacheModeAutomatic)
	}
	if *cfg.PrefixCache.CacheMode == CacheModeAutomatic {
		// Automatic mode uses a single top-level breakpoint, so block-level
		// markers are unused and forced off regardless of explicit config.
		cfg.PrefixCache.CacheSystem = PtrTo(false)
		cfg.PrefixCache.CacheTools = PtrTo(false)
		cfg.PrefixCache.CacheMessages = PtrTo(false)
		return nil
	}
	if !cfg.PrefixCache.CacheSystemWasSet() {
		cfg.PrefixCache.CacheSystem = PtrTo(true)
	}
	if !cfg.PrefixCache.CacheToolsWasSet() {
		cfg.PrefixCache.CacheTools = PtrTo(true)
	}
	if !cfg.PrefixCache.CacheMessagesWasSet() {
		cfg.PrefixCache.CacheMessages = PtrTo(true)
	}
	return nil
}

func applyOpenAICacheDefaults(cfg *Config) error {
	if !cfg.PrefixCache.Enabled {
		return nil
	}
	if !cfg.PrefixCache.OpenAIBreakpointWasSet() {
		cfg.PrefixCache.OpenAIBreakpoint = PtrTo(true)
	}
	if !cfg.PrefixCache.OpenAIModeWasSet() {
		cfg.PrefixCache.OpenAIMode = PtrTo(OpenAIModeImplicit)
	}
	if cfg.PrefixCache.OpenAIMode == nil {
		return nil
	}
	if *cfg.PrefixCache.OpenAIMode != OpenAIModeImplicit && *cfg.PrefixCache.OpenAIMode != OpenAIModeExplicit {
		return fmt.Errorf("invalid openai_mode: %q (must be `%s` or `%s`)", *cfg.PrefixCache.OpenAIMode, OpenAIModeImplicit, OpenAIModeExplicit)
	}
	return nil
}

func validateCacheTTLs(cfg *Config) error {
	validTTLs := map[string]bool{"ephemeral": true, "1h": true}
	all := []struct {
		field string
		val   *string
	}{
		{"cache_control_ttl", &cfg.PrefixCache.CacheControlTTL},
		{"cache_system_ttl", cfg.PrefixCache.CacheSystemTTL},
		{"cache_tools_ttl", cfg.PrefixCache.CacheToolsTTL},
		{"cache_messages_ttl", cfg.PrefixCache.CacheMessagesTTL},
	}
	for _, ttl := range all {
		if ttl.val != nil && *ttl.val != "" && !validTTLs[*ttl.val] {
			return fmt.Errorf("invalid %s: %q (must be 'ephemeral' or '1h')", ttl.field, *ttl.val)
		}
	}
	return nil
}

// validateCacheTTLOrdering ensures TTL values follow a non-increasing pattern:
// longer TTLs ("1h") must precede shorter ones ("ephemeral").
func validateCacheTTLOrdering(cfg *Config) error {
	perBreakpoint := []*string{
		cfg.PrefixCache.CacheSystemTTL,
		cfg.PrefixCache.CacheToolsTTL,
		cfg.PrefixCache.CacheMessagesTTL,
	}
	for i := 1; i < len(perBreakpoint); i++ {
		prev := perBreakpoint[i-1]
		cur := perBreakpoint[i]
		// Skip if either is nil (falls back to global TTL)
		if prev == nil || cur == nil {
			continue
		}
		if *prev == "ephemeral" && *cur == "1h" {
			return fmt.Errorf("invalid TTL ordering: per-breakpoint TTL %q must not follow %q (longer TTLs must precede shorter)", *cur, *prev)
		}
	}
	if cfg.PrefixCache.CacheControlTTL == "1h" && (perBreakpoint[0] != nil && *perBreakpoint[0] == "ephemeral") {
		return fmt.Errorf("invalid TTL ordering: global TTL %q requires all per-breakpoint TTLs to be %q", cfg.PrefixCache.CacheControlTTL, cfg.PrefixCache.CacheControlTTL)
	}
	return nil
}

func anyCacheFeatureSet(cfg *Config) bool {
	if cfg.PrefixCache.PinSystemFirst != nil && *cfg.PrefixCache.PinSystemFirst {
		return true
	}
	if cfg.PrefixCache.StableTools != nil && *cfg.PrefixCache.StableTools {
		return true
	}
	if cfg.PrefixCache.SkipRedactionOnSystem != nil && *cfg.PrefixCache.SkipRedactionOnSystem {
		return true
	}
	if cfg.PrefixCache.CacheSystem != nil && *cfg.PrefixCache.CacheSystem {
		return true
	}
	if cfg.PrefixCache.CacheTools != nil && *cfg.PrefixCache.CacheTools {
		return true
	}
	if cfg.PrefixCache.CacheMessages != nil && *cfg.PrefixCache.CacheMessages {
		return true
	}
	return false
}

var compactionPresets = map[CompactionPreset]struct {
	JSONMinify             bool
	CollapseBlankLines     bool
	TrimTrailingWhitespace bool
	NormalizeLineEndings   bool
	PruneStaleTools        bool
	PruneThoughts          bool
}{
	CompactionPresetAggressive: {
		JSONMinify:             true,
		CollapseBlankLines:     true,
		TrimTrailingWhitespace: true,
		NormalizeLineEndings:   true,
		PruneStaleTools:        true,
		PruneThoughts:          true,
	},
	CompactionPresetBalanced: {
		JSONMinify:             true,
		CollapseBlankLines:     true,
		TrimTrailingWhitespace: true,
		NormalizeLineEndings:   true,
		PruneStaleTools:        false,
		PruneThoughts:          false,
	},
	CompactionPresetMinimal: {
		JSONMinify:             false,
		CollapseBlankLines:     false,
		TrimTrailingWhitespace: false,
		NormalizeLineEndings:   false,
		PruneStaleTools:        false,
		PruneThoughts:          false,
	},
}

func applyCompactionDefaults(cfg *Config) {
	resolveCompactionPreset(cfg)
	applyJSONMinifyDefaults(cfg)
	applyCollapseDefaults(cfg)
	applyTrimDefaults(cfg)
	applyNormalizeDefaults(cfg)
	applyPruneToolsDefaults(cfg)
	applyPruneThoughtsDefaults(cfg)
	applyCompactionEnabledDefaults(cfg)
}

func resolveCompactionPreset(cfg *Config) {
	if cfg.Compaction.Preset == "" {
		return
	}
	preset, ok := compactionPresets[cfg.Compaction.Preset]
	if !ok {
		slog.Warn("unknown compaction_preset, falling back to individual defaults",
			"preset", cfg.Compaction.Preset,
			"valid", []CompactionPreset{CompactionPresetAggressive, CompactionPresetBalanced, CompactionPresetMinimal},
		)
		return
	}
	// Preset values are set only for fields not explicitly configured by the user.
	// The WasSet() check ensures explicit values survive, and the preset acts as a
	// group default. Individual apply*Defaults functions then skip fields already
	// set by the preset (since WasSet() now returns true).
	if !cfg.Compaction.MinifyWasSet() {
		cfg.Compaction.JSONMinify = PtrTo(preset.JSONMinify)
	}
	if !cfg.Compaction.CollapseWasSet() {
		cfg.Compaction.CollapseBlankLines = PtrTo(preset.CollapseBlankLines)
	}
	if !cfg.Compaction.TrimWasSet() {
		cfg.Compaction.TrimTrailingWhitespace = PtrTo(preset.TrimTrailingWhitespace)
	}
	if !cfg.Compaction.NormWasSet() {
		cfg.Compaction.NormalizeLineEndings = PtrTo(preset.NormalizeLineEndings)
	}
	if !cfg.Compaction.PruneWasSet() {
		cfg.Compaction.PruneStaleTools = PtrTo(preset.PruneStaleTools)
	}
	if !cfg.Compaction.PruneThoughtsWasSet() {
		cfg.Compaction.PruneThoughts = PtrTo(preset.PruneThoughts)
	}
}

func applyJSONMinifyDefaults(cfg *Config) {
	if (cfg.Compaction.JSONMinify == nil || !*cfg.Compaction.JSONMinify) && !cfg.Compaction.MinifyWasSet() {
		cfg.Compaction.JSONMinify = PtrTo(true)
	}
}

func applyCollapseDefaults(cfg *Config) {
	if (cfg.Compaction.CollapseBlankLines == nil || !*cfg.Compaction.CollapseBlankLines) && !cfg.Compaction.CollapseWasSet() {
		cfg.Compaction.CollapseBlankLines = PtrTo(true)
	}
}

func applyTrimDefaults(cfg *Config) {
	if (cfg.Compaction.TrimTrailingWhitespace == nil || !*cfg.Compaction.TrimTrailingWhitespace) && !cfg.Compaction.TrimWasSet() {
		cfg.Compaction.TrimTrailingWhitespace = PtrTo(true)
	}
}

func applyNormalizeDefaults(cfg *Config) {
	if (cfg.Compaction.NormalizeLineEndings == nil || !*cfg.Compaction.NormalizeLineEndings) && !cfg.Compaction.NormWasSet() {
		cfg.Compaction.NormalizeLineEndings = PtrTo(true)
	}
}

func applyPruneToolsDefaults(cfg *Config) {
	if (cfg.Compaction.PruneStaleTools == nil || !*cfg.Compaction.PruneStaleTools) && !cfg.Compaction.PruneWasSet() {
		cfg.Compaction.PruneStaleTools = PtrTo(false)
	}
	if cfg.Compaction.ToolProtectionWindow == 0 {
		cfg.Compaction.ToolProtectionWindow = 4
	}
}

func applyPruneThoughtsDefaults(cfg *Config) {
	if (cfg.Compaction.PruneThoughts == nil || !*cfg.Compaction.PruneThoughts) && !cfg.Compaction.PruneThoughtsWasSet() {
		cfg.Compaction.PruneThoughts = PtrTo(false)
	}
}

func applyCompactionEnabledDefaults(cfg *Config) {
	hasAnyFeature := cfg.Compaction.JSONMinify != nil && *cfg.Compaction.JSONMinify || cfg.Compaction.CollapseBlankLines != nil && *cfg.Compaction.CollapseBlankLines || cfg.Compaction.TrimTrailingWhitespace != nil && *cfg.Compaction.TrimTrailingWhitespace || cfg.Compaction.NormalizeLineEndings != nil && *cfg.Compaction.NormalizeLineEndings
	if (cfg.Compaction.Enabled == nil || !*cfg.Compaction.Enabled) && !cfg.Compaction.EnabledWasSet() && hasAnyFeature {
		cfg.Compaction.Enabled = PtrTo(true)
	}
}

func applyResponseCacheDefaults(cfg *Config) {
	if (cfg.ResponseCache.Enabled == nil || !*cfg.ResponseCache.Enabled) && !cfg.ResponseCache.EnabledWasSet() && cfg.ResponseCache.MaxEntries > 0 {
		cfg.ResponseCache.Enabled = PtrTo(true)
	}
	if cfg.ResponseCache.Enabled == nil || !*cfg.ResponseCache.Enabled {
		return
	}
	if cfg.ResponseCache.MaxEntries <= 0 {
		cfg.ResponseCache.MaxEntries = 256
	}
	if cfg.ResponseCache.MaxEntryBytes <= 0 {
		cfg.ResponseCache.MaxEntryBytes = 1 << 20
	}
	if cfg.ResponseCache.TTLSeconds <= 0 {
		cfg.ResponseCache.TTLSeconds = 3600
	}
	if cfg.ResponseCache.EvictEverySeconds <= 0 {
		cfg.ResponseCache.EvictEverySeconds = 300
	}
	if cfg.ResponseCache.ForceRefreshHeader == "" {
		cfg.ResponseCache.ForceRefreshHeader = "x-nenya-cache-force-refresh"
	}
	applyResponseCacheSemanticDefaults(cfg)
}

func applyResponseCacheSemanticDefaults(cfg *Config) {
	if !cfg.ResponseCache.EnableSemantic {
		return
	}
	if cfg.ResponseCache.SimilarityThreshold <= 0 {
		cfg.ResponseCache.SimilarityThreshold = 0.9
	}
	if cfg.ResponseCache.EmbeddingModel == "" {
		cfg.ResponseCache.EmbeddingModel = "mxbai-embed-large"
	}
	if cfg.ResponseCache.EmbeddingURL == "" {
		cfg.ResponseCache.EmbeddingURL = "http://localhost:11434"
	}
}

func applyProviderMapDefaults(cfg *Config) {
	if cfg.Providers == nil {
		cfg.Providers = make(map[string]ProviderConfig)
	}
}

func applyAgentDefaults(cfg *Config) error {
	for name, agent := range cfg.Agents {
		applyAgentMCPDefaults(&agent)
		for i, m := range agent.Models {
			if err := applyAgentModelRegexDefaults(cfg, name, i, &m); err != nil {
				return err
			}
			agent.Models[i] = m
		}
		cfg.Agents[name] = agent
	}
	return nil
}

func applyAgentMCPDefaults(agent *AgentConfig) {
	if agent.MCP != nil && agent.MCP.MaxIterations <= 0 {
		agent.MCP.MaxIterations = 10
	}
}

func applyAgentModelRegexDefaults(cfg *Config, name string, i int, m *AgentModel) error {
	if m.ProviderRgx == "" && m.ModelRgx == "" {
		if looksLikeRegex(m.Model) {
			slog.Warn("agent model looks like a regex pattern but uses the 'model' field (literal); did you mean to use 'model_rgx' for regex matching?", "agent", name, "model_index", i, "model", m.Model)
		}
		return nil
	}

	if m.Provider != "" && m.ProviderRgx != "" {
		slog.Warn("agent model has both provider and provider_rgx set; provider_rgx takes precedence", "agent", name, "model_index", i)
	}
	if m.Model != "" && m.ModelRgx != "" {
		slog.Warn("agent model has both model and model_rgx set; model_rgx takes precedence", "agent", name, "model_index", i)
	}
	if cfg.Discovery.Enabled == nil || !*cfg.Discovery.Enabled {
		slog.Warn("agent model_rgx requires discovery to expand into concrete models; only static registry entries will match", "agent", name, "model_index", i)
	}
	if err := m.CompileRegex(); err != nil {
		return fmt.Errorf("agent %q model %d: %w", name, i, err)
	}
	return nil
}

func applyProviderModelsDefaults(cfg *Config) error {
	for name, pc := range cfg.Providers {
		if len(pc.AllowedModels) == 0 {
			continue
		}
		if _, err := CompileAllowedModels(pc.AllowedModels); err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
	}
	return nil
}

func applyBuiltInProviders(cfg *Config) {
	for name, builtIn := range BuiltInProviders() {
		if userCfg, exists := cfg.Providers[name]; exists {
			cfg.Providers[name] = mergeProviderConfig(userCfg, builtIn)
		} else {
			cfg.Providers[name] = builtIn
		}
	}
}

// mergeProviderConfig merges user config over built-in defaults.
// User values take precedence; missing fields are filled from builtIn.
// MUST only be called once during initialization.
func mergeProviderConfig(user, builtIn ProviderConfig) ProviderConfig {
	merged := user
	mergeString(&merged.URL, builtIn.URL)
	mergeString(&merged.AuthStyle, builtIn.AuthStyle)
	mergeString(&merged.ApiFormat, builtIn.ApiFormat)
	if merged.TimeoutSeconds == 0 && builtIn.TimeoutSeconds != 0 {
		merged.TimeoutSeconds = builtIn.TimeoutSeconds
	}
	if merged.MaxRetryAttempts == 0 && builtIn.MaxRetryAttempts != 0 {
		merged.MaxRetryAttempts = builtIn.MaxRetryAttempts
	}
	if len(merged.RetryableStatusCodes) == 0 && len(builtIn.RetryableStatusCodes) > 0 {
		merged.RetryableStatusCodes = builtIn.RetryableStatusCodes
	}
	if merged.FormatURLs == nil && builtIn.FormatURLs != nil {
		merged.FormatURLs = make(map[string]string)
		for k, v := range builtIn.FormatURLs {
			merged.FormatURLs[k] = v
		}
	}
	if merged.Thinking == nil && builtIn.Thinking != nil {
		merged.Thinking = builtIn.Thinking
	}
	// Rate-limit defaults are tri-state pointers: nil inherits, 0 disables.
	if merged.RatelimitMaxRPM == nil {
		merged.RatelimitMaxRPM = builtIn.RatelimitMaxRPM
	}
	if merged.RatelimitMaxTPM == nil {
		merged.RatelimitMaxTPM = builtIn.RatelimitMaxTPM
	}
	return merged
}

// mergeString sets *dst to src if dst is non-nil and empty, and src is non-empty.
func mergeString(dst *string, src string) {
	if dst != nil && *dst == "" && src != "" {
		*dst = src
	}
}

func applyWindowDefaults(cfg *Config) {
	if !cfg.Window.Enabled && (cfg.Window.Mode != "" || cfg.Window.ActiveMessages != 0 || cfg.Window.TriggerRatio != 0 || cfg.Window.SummaryMaxRunes != 0 || cfg.Window.MaxContext != 0) {
		cfg.Window.Enabled = true
	}
	if cfg.Window.Mode == "" {
		cfg.Window.Mode = "summarize"
	}
	if cfg.Window.ActiveMessages == 0 {
		cfg.Window.ActiveMessages = 6
	}
	if cfg.Window.TriggerRatio == 0 {
		cfg.Window.TriggerRatio = 0.8
	}
	if cfg.Window.SummaryMaxRunes == 0 {
		cfg.Window.SummaryMaxRunes = 4000
	}
	if cfg.Window.MaxContext < 0 {
		cfg.Window.MaxContext = 0
	}
	if cfg.Window.MaxContext == 0 {
		cfg.Window.MaxContext = 128000
	}
	if cfg.Window.KeepFirstPct == 0 {
		cfg.Window.KeepFirstPct = 25.0
	}
	if cfg.Window.KeepLastPct == 0 {
		cfg.Window.KeepLastPct = 30.0
	}
}

func applyLocalEngineDefaults(cfg *Config) {
	if cfg.LocalEngine == nil {
		return
	}
	if cfg.LocalEngine.BaseURL == "" {
		cfg.LocalEngine.BaseURL = "http://127.0.0.1:11434"
	}
	if cfg.LocalEngine.TimeoutSeconds == 0 {
		cfg.LocalEngine.TimeoutSeconds = 120
	}
	if cfg.LocalEngine.MaxSessions == 0 {
		cfg.LocalEngine.MaxSessions = 3
	}
}

func looksLikeRegex(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		switch s[i] {
		case '.', '*', '+', '^', '$', '(', ')', '[', ']', '{', '}', '|', '\\', '?':
			return true
		}
	}
	return false
}
