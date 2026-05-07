package config

import (
	"fmt"
	"log/slog"
	"os"
	"syscall"
)

var configLogLevel slog.LevelVar

func applyLogLevel(level string) error {
	if level == "" {
		return nil
	}
	return setConfigLogLevel(level)
}

func setConfigLogLevel(level string) error {
	var slogLevel slog.Level
	switch level {
	case "debug":
		slogLevel = slog.LevelDebug
	case "info":
		slogLevel = slog.LevelInfo
	case "warn":
		slogLevel = slog.LevelWarn
	case "error":
		slogLevel = slog.LevelError
	default:
		return fmt.Errorf("invalid log level: %s (must be debug, info, warn, or error)", level)
	}
	configLogLevel.Set(slogLevel)
	return nil
}

// SetupLogger creates a slog.Logger with auto-detected text or JSON
// output format (text for TTY, JSON for non-TTY such as systemd). The
// verbosity flag controls debug vs info level.
func SetupLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	configLogLevel.Set(level)

	var handler slog.Handler
	if isatty(os.Stderr.Fd()) {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: &configLogLevel})
	} else {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &configLogLevel})
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}

func isatty(fd uintptr) bool {
	var st syscall.Stat_t
	if err := syscall.Fstat(int(fd), &st); err != nil {
		return false
	}
	return st.Mode&syscall.S_IFMT == syscall.S_IFCHR
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
	applyPrefixCacheDefaults(cfg)
	applyCompactionDefaults(cfg)
	applyResponseCacheDefaults(cfg)
	applyProviderMapDefaults(cfg)
	if err := applyAgentDefaults(cfg); err != nil {
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
		cfg.Server.UserAgent = "nenya/1.0"
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
}

func applyGovernanceDefaults(cfg *Config) {
	if err := applyLogLevel(cfg.Server.LogLevel); err != nil {
		return
	}
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
	if !cfg.Governance.EmptyStreamAsErrorSet() {
		cfg.Governance.EmptyStreamAsError = PtrTo(true)
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
	},
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
			cfg.RedactPatterns = patterns
		}
	}
}

func applyBouncerDefaults(cfg *Config) {
	expandRedactPreset(&cfg.Bouncer)
	if cfg.Bouncer.RedactPatterns == nil {
		if !cfg.Bouncer.EnabledWasSet() {
			cfg.Bouncer.Enabled = PtrTo(true)
		}
		cfg.Bouncer.RedactPatterns = redactPresets["credentials"]
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

func applyPrefixCacheDefaults(cfg *Config) {
	if !cfg.PrefixCache.Enabled && (cfg.PrefixCache.PinSystemFirst != nil && *cfg.PrefixCache.PinSystemFirst || cfg.PrefixCache.StableTools != nil && *cfg.PrefixCache.StableTools || cfg.PrefixCache.SkipRedactionOnSystem != nil && *cfg.PrefixCache.SkipRedactionOnSystem) {
		cfg.PrefixCache.Enabled = true
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
		cfg.ResponseCache.MaxEntries = 512
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
			fmt.Printf("[WARN] agent %q model %d: model %q looks like a regex pattern but uses the 'model' field (literal). Did you mean to use 'model_rgx' for regex matching?\n", name, i, m.Model)
		}
		return nil
	}

	if m.Provider != "" && m.ProviderRgx != "" {
		fmt.Printf("[WARN] agent %q model %d: both provider and provider_rgx set; provider_rgx takes precedence\n", name, i)
	}
	if m.Model != "" && m.ModelRgx != "" {
		fmt.Printf("[WARN] agent %q model %d: both model and model_rgx set; model_rgx takes precedence\n", name, i)
	}
	if cfg.Discovery.Enabled == nil || !*cfg.Discovery.Enabled {
		fmt.Printf("[WARN] agent %q model %d: model_rgx requires discovery to expand into concrete models; only static registry entries will match\n", name, i)
	}
	if err := m.CompileRegex(); err != nil {
		return fmt.Errorf("agent %q model %d: %w", name, i, err)
	}
	return nil
}

func applyBuiltInProviders(cfg *Config) {
	for name, builtIn := range BuiltInProviders() {
		if _, exists := cfg.Providers[name]; !exists {
			cfg.Providers[name] = builtIn
		}
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

func looksLikeRegex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '.', '*', '+', '^', '$', '(', ')', '[', ']', '{', '}', '|', '\\', '?':
			return true
		}
	}
	return false
}
