package config

import (
	"fmt"
	"nenya/internal/infra"
)

func applyLogLevel(level string) error {
	if level == "" {
		return nil
	}
	return infra.SetLogLevel(level)
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

func ApplyDefaults(cfg *Config) error {
	applyServerDefaults(cfg)
	applyGovernanceDefaults(cfg)
	applySecurityFilterDefaults(cfg)
	applyEngineRefDefaults(&cfg.SecurityFilter.Engine)
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
	applyResolvedEngineDefaults(cfg.SecurityFilter.Engine.ResolvedTargets)
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
}

func applyGovernanceDefaults(cfg *Config) {
	if err := applyLogLevel(cfg.Server.LogLevel); err != nil {
		return
	}
	if !cfg.Governance.TPMSet() && cfg.Governance.RatelimitMaxTPM == 0 {
		cfg.Governance.RatelimitMaxTPM = 250000
	}
	if !cfg.Governance.RPMSet() && cfg.Governance.RatelimitMaxRPM == 0 {
		cfg.Governance.RatelimitMaxRPM = 15
	}
	if cfg.Governance.TruncationStrategy == "" {
		cfg.Governance.TruncationStrategy = "middle-out"
	}
	if cfg.Governance.KeepFirstPercent == 0 {
		cfg.Governance.KeepFirstPercent = 15.0
	}
	if cfg.Governance.KeepLastPercent == 0 {
		cfg.Governance.KeepLastPercent = 25.0
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
}

func applySecurityFilterDefaults(cfg *Config) {
	if cfg.SecurityFilter.Patterns == nil {
		if !cfg.SecurityFilter.EnabledWasSet() {
			cfg.SecurityFilter.Enabled = true
		}
		cfg.SecurityFilter.Patterns = []string{
			`(?i)AKIA[0-9A-Z]{16}`,
			`(?i)gh(p|o|s)_[a-zA-Z0-9]{36,255}`,
			`(?i)ya29\.[0-9A-Za-z\-_]+`,
			`(?i)sk-[a-zA-Z0-9]{48}`,
			`(?i)-----BEGIN\s+(RSA\s+)?(DSA\s+)?(EC\s+)?PRIVATE\s+KEY\s*-----`,
			`(?i)(aws_access_key_id|aws_secret_access_key)\s*=\s*['"][^'"]{10,}['"]`,
			`(?i)(password|passwd|pwd|secret|token)[\s:=]+['"][^'"]{6,}['"]`,
			`[a-f0-9]{32}:`,
			`(?i)SG\.[a-zA-Z0-9\-_]{22}\.[a-zA-Z0-9\-_]{43}`,
		}
	} else if !cfg.SecurityFilter.EnabledWasSet() {
		cfg.SecurityFilter.Enabled = true
	}
	if cfg.SecurityFilter.RedactionLabel == "" {
		cfg.SecurityFilter.RedactionLabel = "[REDACTED]"
	}
	if cfg.SecurityFilter.OutputWindowChars == 0 {
		cfg.SecurityFilter.OutputWindowChars = 4096
	}
	cfg.SecurityFilter.SkipOnEngineFailure = true
	if cfg.SecurityFilter.EntropyThreshold == 0 {
		cfg.SecurityFilter.EntropyThreshold = 4.5
	}
	if cfg.SecurityFilter.EntropyMinToken == 0 {
		cfg.SecurityFilter.EntropyMinToken = 20
	}
}

func applyPrefixCacheDefaults(cfg *Config) {
	if !cfg.PrefixCache.Enabled && (cfg.PrefixCache.PinSystemFirst || cfg.PrefixCache.StableTools || cfg.PrefixCache.SkipRedactionOnSystem) {
		cfg.PrefixCache.Enabled = true
	}
	if !cfg.PrefixCache.PinSystemFirst && !cfg.PrefixCache.PinWasSet() {
		cfg.PrefixCache.PinSystemFirst = true
	}
	if !cfg.PrefixCache.StableTools && !cfg.PrefixCache.StableWasSet() {
		cfg.PrefixCache.StableTools = true
	}
	if !cfg.PrefixCache.SkipRedactionWasSet() {
		cfg.PrefixCache.SkipRedactionOnSystem = false
	}
}

func applyCompactionDefaults(cfg *Config) {
	applyJSONMinifyDefaults(cfg)
	applyCollapseDefaults(cfg)
	applyTrimDefaults(cfg)
	applyNormalizeDefaults(cfg)
	applyPruneToolsDefaults(cfg)
	applyPruneThoughtsDefaults(cfg)
	applyCompactionEnabledDefaults(cfg)
}

func applyJSONMinifyDefaults(cfg *Config) {
	if !cfg.Compaction.JSONMinify && !cfg.Compaction.MinifyWasSet() {
		cfg.Compaction.JSONMinify = true
	}
}

func applyCollapseDefaults(cfg *Config) {
	if !cfg.Compaction.CollapseBlankLines && !cfg.Compaction.CollapseWasSet() {
		cfg.Compaction.CollapseBlankLines = true
	}
}

func applyTrimDefaults(cfg *Config) {
	if !cfg.Compaction.TrimTrailingWhitespace && !cfg.Compaction.TrimWasSet() {
		cfg.Compaction.TrimTrailingWhitespace = true
	}
}

func applyNormalizeDefaults(cfg *Config) {
	if !cfg.Compaction.NormalizeLineEndings && !cfg.Compaction.NormWasSet() {
		cfg.Compaction.NormalizeLineEndings = true
	}
}

func applyPruneToolsDefaults(cfg *Config) {
	if !cfg.Compaction.PruneStaleTools && !cfg.Compaction.PruneWasSet() {
		cfg.Compaction.PruneStaleTools = false
	}
	if cfg.Compaction.ToolProtectionWindow == 0 && !cfg.Compaction.PruneWasSet() {
		cfg.Compaction.ToolProtectionWindow = 4
	}
}

func applyPruneThoughtsDefaults(cfg *Config) {
	if !cfg.Compaction.PruneThoughts && !cfg.Compaction.PruneThoughtsWasSet() {
		cfg.Compaction.PruneThoughts = false
	}
}

func applyCompactionEnabledDefaults(cfg *Config) {
	hasAnyFeature := cfg.Compaction.JSONMinify || cfg.Compaction.CollapseBlankLines || cfg.Compaction.TrimTrailingWhitespace || cfg.Compaction.NormalizeLineEndings
	if !cfg.Compaction.Enabled && !cfg.Compaction.EnabledWasSet() && hasAnyFeature {
		cfg.Compaction.Enabled = true
	}
}

func applyResponseCacheDefaults(cfg *Config) {
	if !cfg.ResponseCache.Enabled && !cfg.ResponseCache.EnabledWasSet() && cfg.ResponseCache.MaxEntries > 0 {
		cfg.ResponseCache.Enabled = true
	}
	if !cfg.ResponseCache.Enabled {
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
		if agent.MCP != nil {
			if agent.MCP.MaxIterations <= 0 {
				agent.MCP.MaxIterations = 10
			}
		}
		for i, sel := range agent.ModelSelectors {
			if err := sel.Compile(); err != nil {
				return fmt.Errorf("agent %q selector %d: %w", name, i, err)
			}
			agent.ModelSelectors[i] = sel
		}
		cfg.Agents[name] = agent
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
}
