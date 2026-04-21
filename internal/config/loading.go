package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to access config path %s: %v", path, err)
	}

	if info.IsDir() {
		return loadFromDirectory(path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", path, err)
	}
	data = stripComments(data)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", path, err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return &cfg, nil
}

func loadFromDirectory(dir string) (*Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read config directory %s: %v", dir, err)
	}

	var jsonFiles []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		if name == "secrets.json" {
			continue
		}
		jsonFiles = append(jsonFiles, name)
	}
	slices.Sort(jsonFiles)

	if len(jsonFiles) == 0 {
		return nil, fmt.Errorf("no JSON config files found in %s", dir)
	}

	merged := &Config{}

	for _, name := range jsonFiles {
		filePath := filepath.Join(dir, name)
		data, err := os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read config file %s: %v", filePath, err)
		}
		data = stripComments(data)

		var partial Config
		if err := json.Unmarshal(data, &partial); err != nil {
			return nil, fmt.Errorf("failed to parse config file %s: %v", filePath, err)
		}

		mergeConfig(merged, &partial)
	}

	if err := ApplyDefaults(merged); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return merged, nil
}

func mergeConfig(base, overlay *Config) {
	if overlay.Server.ListenAddr != "" {
		base.Server.ListenAddr = overlay.Server.ListenAddr
	}
	if overlay.Server.MaxBodyBytes != 0 {
		base.Server.MaxBodyBytes = overlay.Server.MaxBodyBytes
	}
if overlay.Server.UserAgent != "" {
		base.Server.UserAgent = overlay.Server.UserAgent
	}

	if overlay.Governance.TruncationStrategy != "" {
		base.Governance.TruncationStrategy = overlay.Governance.TruncationStrategy
	}
	if overlay.Governance.KeepFirstPercent != 0 {
		base.Governance.KeepFirstPercent = overlay.Governance.KeepFirstPercent
	}
	if overlay.Governance.KeepLastPercent != 0 {
		base.Governance.KeepLastPercent = overlay.Governance.KeepLastPercent
	}
	if overlay.Governance.TFIDFQuerySource != "" {
		base.Governance.TFIDFQuerySource = overlay.Governance.TFIDFQuerySource
	}
	if len(overlay.Governance.BlockedExecutionPatterns) > 0 {
		base.Governance.BlockedExecutionPatterns = overlay.Governance.BlockedExecutionPatterns
	}
	if len(overlay.Governance.RetryableStatusCodes) > 0 {
		base.Governance.RetryableStatusCodes = overlay.Governance.RetryableStatusCodes
	}
	if overlay.Governance.RPMSet() {
		base.Governance.RatelimitMaxRPM = overlay.Governance.RatelimitMaxRPM
	}
	if overlay.Governance.TPMSet() {
		base.Governance.RatelimitMaxTPM = overlay.Governance.RatelimitMaxTPM
	}

	if overlay.Governance.TruncationStrategy != "" {
		base.Governance.TruncationStrategy = overlay.Governance.TruncationStrategy
	}
	if overlay.Governance.TPMSet() {
		base.Governance.RatelimitMaxTPM = overlay.Governance.RatelimitMaxTPM
	}

	if overlay.SecurityFilter.EnabledWasSet() {
		base.SecurityFilter.Enabled = overlay.SecurityFilter.Enabled
		base.SecurityFilter.enabledSet = true
	}
	if overlay.SecurityFilter.RedactionLabel != "" {
		base.SecurityFilter.RedactionLabel = overlay.SecurityFilter.RedactionLabel
	}
	if len(overlay.SecurityFilter.Patterns) > 0 {
		base.SecurityFilter.Patterns = overlay.SecurityFilter.Patterns
	}
	if overlay.SecurityFilter.OutputEnabled {
		base.SecurityFilter.OutputEnabled = true
	}
	if overlay.SecurityFilter.OutputWindowChars != 0 {
		base.SecurityFilter.OutputWindowChars = overlay.SecurityFilter.OutputWindowChars
	}
	if overlay.SecurityFilter.SkipOnEngineFailure {
		base.SecurityFilter.SkipOnEngineFailure = true
	}
	if overlay.SecurityFilter.Engine.AgentName != "" || overlay.SecurityFilter.Engine.Provider != "" {
		base.SecurityFilter.Engine = overlay.SecurityFilter.Engine
	}

	if overlay.PrefixCache.PinWasSet() {
		base.PrefixCache.PinSystemFirst = overlay.PrefixCache.PinSystemFirst
		base.PrefixCache.pinSet = true
	}
	if overlay.PrefixCache.StableWasSet() {
		base.PrefixCache.StableTools = overlay.PrefixCache.StableTools
		base.PrefixCache.stableSet = true
	}
	if overlay.PrefixCache.SkipRedactionWasSet() {
		base.PrefixCache.SkipRedactionOnSystem = overlay.PrefixCache.SkipRedactionOnSystem
		base.PrefixCache.skipRedactionSet = true
	}

	if overlay.Compaction.EnabledWasSet() {
		base.Compaction.Enabled = overlay.Compaction.Enabled
		base.Compaction.enabledSet = true
	}
	if overlay.Compaction.MinifyWasSet() {
		base.Compaction.JSONMinify = overlay.Compaction.JSONMinify
		base.Compaction.minifySet = true
	}
	if overlay.Compaction.CollapseWasSet() {
		base.Compaction.CollapseBlankLines = overlay.Compaction.CollapseBlankLines
		base.Compaction.collapseSet = true
	}
	if overlay.Compaction.TrimWasSet() {
		base.Compaction.TrimTrailingWhitespace = overlay.Compaction.TrimTrailingWhitespace
		base.Compaction.trimSet = true
	}
	if overlay.Compaction.NormWasSet() {
		base.Compaction.NormalizeLineEndings = overlay.Compaction.NormalizeLineEndings
		base.Compaction.normalizeSet = true
	}
	if overlay.Compaction.PruneWasSet() {
		base.Compaction.PruneStaleTools = overlay.Compaction.PruneStaleTools
		base.Compaction.pruneSet = true
	}
	if overlay.Compaction.PruneThoughtsWasSet() {
		base.Compaction.PruneThoughts = overlay.Compaction.PruneThoughts
		base.Compaction.pruneThoughtsSet = true
	}
	if overlay.Compaction.ToolProtectionWindow != 0 {
		base.Compaction.ToolProtectionWindow = overlay.Compaction.ToolProtectionWindow
	}

	if overlay.Window.Enabled {
		base.Window.Enabled = true
	}
	if overlay.Window.Mode != "" {
		base.Window.Mode = overlay.Window.Mode
	}
	if overlay.Window.ActiveMessages != 0 {
		base.Window.ActiveMessages = overlay.Window.ActiveMessages
	}
	if overlay.Window.TriggerRatio != 0 {
		base.Window.TriggerRatio = overlay.Window.TriggerRatio
	}
	if overlay.Window.SummaryMaxRunes != 0 {
		base.Window.SummaryMaxRunes = overlay.Window.SummaryMaxRunes
	}
	if overlay.Window.MaxContext != 0 {
		base.Window.MaxContext = overlay.Window.MaxContext
	}
	if overlay.Window.Engine.AgentName != "" || overlay.Window.Engine.Provider != "" {
		base.Window.Engine = overlay.Window.Engine
	}

	if overlay.ResponseCache.EnabledWasSet() {
		base.ResponseCache.Enabled = overlay.ResponseCache.Enabled
		base.ResponseCache.enabledSet = true
	}
	if overlay.ResponseCache.MaxEntries != 0 {
		base.ResponseCache.MaxEntries = overlay.ResponseCache.MaxEntries
	}
	if overlay.ResponseCache.MaxEntryBytes != 0 {
		base.ResponseCache.MaxEntryBytes = overlay.ResponseCache.MaxEntryBytes
	}
	if overlay.ResponseCache.TTLSeconds != 0 {
		base.ResponseCache.TTLSeconds = overlay.ResponseCache.TTLSeconds
	}
	if overlay.ResponseCache.EvictEverySeconds != 0 {
		base.ResponseCache.EvictEverySeconds = overlay.ResponseCache.EvictEverySeconds
	}
	if overlay.ResponseCache.ForceRefreshHeader != "" {
		base.ResponseCache.ForceRefreshHeader = overlay.ResponseCache.ForceRefreshHeader
	}

	mergeMap(base, overlay, &base.Agents, &overlay.Agents)
	mergeMap(base, overlay, &base.Providers, &overlay.Providers)
	mergeMap(base, overlay, &base.MCPServers, &overlay.MCPServers)
}

func mergeMap[T any](base, overlay *Config, baseField *map[string]T, overlayField *map[string]T) {
	if len(*overlayField) == 0 {
		return
	}
	if *baseField == nil {
		*baseField = make(map[string]T, len(*overlayField))
	}
	for k, v := range *overlayField {
		(*baseField)[k] = v
	}
}

func stripComments(data []byte) []byte {
	var result []byte
	i := 0
	n := len(data)
	inString := false
	for i < n {
		if !inString && i+1 < n && data[i] == '/' && data[i+1] == '/' {
			for i < n && data[i] != '\n' {
				i++
			}
			continue
		}
		if !inString && i+1 < n && data[i] == '/' && data[i+1] == '*' {
			for i < n && !(data[i] == '*' && i+1 < n && data[i+1] == '/') {
				i++
			}
			if i+1 < n {
				i += 2
			}
			continue
		}
		if data[i] == '"' {
			backslashCount := 0
			for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
				backslashCount++
			}
			if backslashCount%2 == 0 {
				inString = !inString
			}
		}
		result = append(result, data[i])
		i++
	}
	return result
}

func LoadPromptFile(filePath string, directPrompt string, defaultPrompt string) (string, error) {
	if directPrompt != "" {
		return directPrompt, nil
	}
	if filePath == "" {
		return defaultPrompt, nil
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read prompt file %s: %v", filePath, err)
	}
	return string(data), nil
}

func LoadSecrets() (*SecretsConfig, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir != "" {
		secretsPath := credDir + "/secrets"
		data, err := os.ReadFile(secretsPath)
		if err == nil {
			var secrets SecretsConfig
			if err := json.Unmarshal(data, &secrets); err != nil {
				return nil, fmt.Errorf("failed to parse secrets JSON: %v", err)
			}
			if secrets.ClientToken == "" {
				return nil, fmt.Errorf("client_token missing in secrets")
			}
			if secrets.ProviderKeys == nil {
				secrets.ProviderKeys = make(map[string]string)
			}
			return &secrets, nil
		}
	}

	clientToken := os.Getenv("NENYA_CLIENT_TOKEN")
	if clientToken == "" {
		return nil, fmt.Errorf("CREDENTIALS_DIRECTORY not set and NENYA_CLIENT_TOKEN not set")
	}

	secrets := &SecretsConfig{
		ClientToken:  clientToken,
		ProviderKeys: make(map[string]string),
	}

	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "NENYA_PROVIDER_KEY_") {
			parts := strings.SplitN(env, "=", 2)
			if len(parts) != 2 {
				continue
			}
			providerName := strings.ToLower(strings.TrimPrefix(parts[0], "NENYA_PROVIDER_KEY_"))
			secrets.ProviderKeys[providerName] = parts[1]
		}
	}

	return secrets, nil
}

func ResolveProviders(cfg *Config, secrets *SecretsConfig) map[string]*Provider {
	providers := make(map[string]*Provider, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		apiKey := ""
		if secrets != nil {
			apiKey = secrets.ProviderKeys[name]
		}
		providers[name] = &Provider{
			Name:                 name,
			URL:                  pc.URL,
			APIKey:               apiKey,
			RoutePrefixes:        pc.RoutePrefixes,
			AuthStyle:            pc.AuthStyle,
			ApiFormat:            pc.ApiFormat,
			TimeoutSeconds:       pc.TimeoutSeconds,
			RetryableStatusCodes: pc.RetryableStatusCodes,
		}
	}
	return providers
}

func BuiltInProviders() map[string]ProviderConfig {
	providers := make(map[string]ProviderConfig, len(ProviderRegistry))
	for name, entry := range ProviderRegistry {
		providers[name] = entry.ToProviderConfig()
	}
	return providers
}
