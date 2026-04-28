package config

import (
	"encoding/json"
	"fmt"
	"net/url"
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
	mergeServerConfig(base, overlay)
	mergeGovernanceConfig(base, overlay)
	mergeSecurityFilterConfig(base, overlay)
	mergePrefixCacheConfig(base, overlay)
	mergeCompactionConfig(base, overlay)
	mergeWindowConfig(base, overlay)
	mergeResponseCacheConfig(base, overlay)
	mergeDiscoveryConfig(base, overlay)
	mergeMap(base, overlay, &base.Agents, &overlay.Agents)
	mergeMap(base, overlay, &base.Providers, &overlay.Providers)
	mergeMap(base, overlay, &base.MCPServers, &overlay.MCPServers)
}

func mergeServerConfig(base, overlay *Config) {
	if overlay.Server.ListenAddr != "" {
		base.Server.ListenAddr = overlay.Server.ListenAddr
	}
	if overlay.Server.MaxBodyBytes != 0 {
		base.Server.MaxBodyBytes = overlay.Server.MaxBodyBytes
	}
	if overlay.Server.UserAgent != "" {
		base.Server.UserAgent = overlay.Server.UserAgent
	}
	if overlay.Server.LogLevel != "" {
		base.Server.LogLevel = overlay.Server.LogLevel
	}
}

func mergeGovernanceConfig(base, overlay *Config) {
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
}

func mergeSecurityFilterConfig(base, overlay *Config) {
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
}

func mergePrefixCacheConfig(base, overlay *Config) {
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
}

func mergeCompactionConfig(base, overlay *Config) {
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
}

func mergeWindowConfig(base, overlay *Config) {
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
}

func mergeResponseCacheConfig(base, overlay *Config) {
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
}

func mergeDiscoveryConfig(base, overlay *Config) {
	if overlay.Discovery.Enabled {
		base.Discovery.Enabled = overlay.Discovery.Enabled
	}
	if overlay.Discovery.AutoAgents {
		base.Discovery.AutoAgents = overlay.Discovery.AutoAgents
	}
	if overlay.Discovery.AutoAgentsConfig != nil {
		base.Discovery.AutoAgentsConfig = overlay.Discovery.AutoAgentsConfig
	}
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
	inString := false
	for i < len(data) {
		if !inString && i+1 < len(data) && data[i] == '/' {
			if data[i+1] == '/' {
				i = skipLineComment(data, i)
				continue
			}
			if data[i+1] == '*' {
				i = skipBlockComment(data, i)
				continue
			}
		}
		if data[i] == '"' && isUnescapedQuote(data, i) {
			inString = !inString
		}
		result = append(result, data[i])
		i++
	}
	return result
}

func skipLineComment(data []byte, i int) int {
	for i < len(data) && data[i] != '\n' && data[i] != '\r' {
		i++
	}
	return i
}

func skipBlockComment(data []byte, i int) int {
	for i < len(data) && (data[i] != '*' || i+1 >= len(data) || data[i+1] != '/') {
		i++
	}
	if i+1 < len(data) {
		i += 2
	}
	return i
}

func isUnescapedQuote(data []byte, i int) bool {
	backslashCount := 0
	for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
		backslashCount++
	}
	return backslashCount%2 == 0
}

func LoadPromptFile(filePath string, directPrompt string, defaultPrompt string) (string, error) {
	if directPrompt != "" {
		return directPrompt, nil
	}
	if filePath == "" {
		return defaultPrompt, nil
	}

	if err := validatePromptPath(filePath); err != nil {
		return "", err
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to read prompt file %s: %v", filePath, err)
	}
	return string(data), nil
}

func validatePromptPath(filePath string) error {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil
	}

	configDir := os.Getenv("CONFIG_DIR")
	if configDir == "" {
		return nil
	}

	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return nil
	}

	relPath, err := filepath.Rel(absConfigDir, absPath)
	if err != nil {
		return nil
	}

	if strings.Contains(relPath, "..") {
		return fmt.Errorf("prompt file path escapes config directory: %s", filePath)
	}

	return nil
}

func LoadSecrets() (*SecretsConfig, error) {
	if secrets, err := tryLoadCredFile(); err != nil {
		return nil, err
	} else if secrets != nil {
		return secrets, nil
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

func tryLoadCredFile() (*SecretsConfig, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	if credDir == "" {
		return nil, nil
	}

	data, err := os.ReadFile(credDir + "/secrets")
	if err != nil {
		return nil, nil
	}

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
			BaseURL:              deriveBaseURL(pc.URL),
			APIKey:               apiKey,
			AuthStyle:            pc.AuthStyle,
			ApiFormat:            pc.ApiFormat,
			TimeoutSeconds:       pc.TimeoutSeconds,
			RetryableStatusCodes: pc.RetryableStatusCodes,
			Thinking:             pc.Thinking,
		}
	}
	return providers
}

func deriveBaseURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	u.Path = ""
	u.RawPath = ""
	return u.String()
}

func BuiltInProviders() map[string]ProviderConfig {
	providers := make(map[string]ProviderConfig, len(ProviderRegistry))
	for name, entry := range ProviderRegistry {
		providers[name] = entry.ToProviderConfig()
	}
	return providers
}
