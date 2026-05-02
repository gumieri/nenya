package config

import (
	"encoding/json"
	"errors"
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
		return nil, fmt.Errorf("config path %s is a directory, use LoadFromDir() instead", path)
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

func LoadFromDir(dir string) (*Config, error) {
	// 1. Try config.d/*.json (merge all) - only if it exists and is a directory
	configDirPath := dir + "/config.d"
	if info, err := os.Stat(configDirPath); err == nil && info.IsDir() {
		dirCfg, dirErr := loadConfigDirectory(configDirPath)
		if dirErr != nil {
			return nil, dirErr
		}
		if dirCfg != nil {
			return dirCfg, nil
		}
	}

	// 2. Try config.json
	configFilePath := dir + "/config.json"
	if info, err := os.Stat(configFilePath); err == nil && !info.IsDir() {
		fileCfg, fileErr := Load(configFilePath)
		if fileErr != nil {
			return nil, fileErr
		}
		if fileCfg != nil {
			return fileCfg, nil
		}
	}

	return nil, fmt.Errorf("no config found in %s (tried %s/config.d/*.json and %s/config.json)", dir, dir, dir)
}

// loadConfigDirectory loads and merges all *.json files in the given directory alphabetically.
func loadConfigDirectory(dir string) (*Config, error) {
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
		// Skip secrets.json to avoid accidentally loading secrets as config
		if name == "secrets.json" {
			continue
		}
		jsonFiles = append(jsonFiles, name)
	}
	slices.Sort(jsonFiles)

	if len(jsonFiles) == 0 {
		return nil, nil
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
	mergeGovernanceScalars(base, overlay)
	mergeGovernanceBools(base, overlay)
}

func mergeGovernanceScalars(base, overlay *Config) {
	og := &overlay.Governance
	bg := &base.Governance
	if og.TruncationStrategy != "" {
		bg.TruncationStrategy = og.TruncationStrategy
	}
	if og.KeepFirstPercent != 0 {
		bg.KeepFirstPercent = og.KeepFirstPercent
	}
	if og.KeepLastPercent != 0 {
		bg.KeepLastPercent = og.KeepLastPercent
	}
	if og.TFIDFQuerySource != "" {
		bg.TFIDFQuerySource = og.TFIDFQuerySource
	}
	if len(og.BlockedExecutionPatterns) > 0 {
		bg.BlockedExecutionPatterns = og.BlockedExecutionPatterns
	}
	if len(og.RetryableStatusCodes) > 0 {
		bg.RetryableStatusCodes = og.RetryableStatusCodes
	}
	if og.RPMSet() {
		bg.RatelimitMaxRPM = og.RatelimitMaxRPM
	}
	if og.TPMSet() {
		bg.RatelimitMaxTPM = og.RatelimitMaxTPM
	}
	if og.MaxRetryAttempts != 0 {
		bg.MaxRetryAttempts = og.MaxRetryAttempts
	}
	if og.RoutingStrategy != "" {
		bg.RoutingStrategy = og.RoutingStrategy
	}
	if og.RoutingLatencyWeight != 0 {
		bg.RoutingLatencyWeight = og.RoutingLatencyWeight
	}
	if og.RoutingCostWeight != 0 {
		bg.RoutingCostWeight = og.RoutingCostWeight
	}
	if og.MaxCostPerRequest != 0 {
		bg.MaxCostPerRequest = og.MaxCostPerRequest
	}
}

func mergeGovernanceBools(base, overlay *Config) {
	og := &overlay.Governance
	bg := &base.Governance
	if og.EmptyStreamAsErrorSet() {
		bg.EmptyStreamAsError = og.EmptyStreamAsError
		bg.emptyStreamAsErrorSet = true
	}
	if og.AutoContextSkipSet() {
		bg.AutoContextSkip = og.AutoContextSkip
		bg.autoContextSkipSet = true
	}
	if og.AutoReorderByLatencySet() {
		bg.AutoReorderByLatency = og.AutoReorderByLatency
		bg.autoReorderByLatencySet = true
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
	if overlay.SecurityFilter.EntropyEnabled {
		base.SecurityFilter.EntropyEnabled = true
	}
	if overlay.SecurityFilter.EntropyThreshold != 0 {
		base.SecurityFilter.EntropyThreshold = overlay.SecurityFilter.EntropyThreshold
	}
	if overlay.SecurityFilter.EntropyMinToken != 0 {
		base.SecurityFilter.EntropyMinToken = overlay.SecurityFilter.EntropyMinToken
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
	if overlay.Discovery.EnabledWasSet() {
		base.Discovery.Enabled = overlay.Discovery.Enabled
	}
	if overlay.Discovery.AutoAgentsWasSet() {
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
	cleaned := filepath.Clean(filePath)
	if strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) || cleaned == ".." {
		return fmt.Errorf("prompt file path escapes working directory: %s", filePath)
	}

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

	if strings.HasPrefix(relPath, ".."+string(filepath.Separator)) || relPath == ".." {
		return fmt.Errorf("prompt file path escapes config directory: %s", filePath)
	}

	return nil
}

func LoadSecrets() (*SecretsConfig, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	secretsDir := os.Getenv("NENYA_SECRETS_DIR")

	// 1. systemd single file
	secrets, err := tryLoadCredFile()
	if err != nil {
		return nil, err
	}
	if secrets != nil {
		return validateSecretsResult(secrets)
	}

	// 2. CREDENTIALS_DIRECTORY/secrets.d/*.json
	if credDir != "" {
		secrets, err = loadSecretsFromPath(credDir + "/secrets.d")
		if err != nil {
			return nil, err
		}
		if secrets != nil {
			return validateSecretsResult(secrets)
		}
	}

	// 3. NENYA_SECRETS_DIR or /run/secrets/nenya
	if secretsDir == "" {
		secretsDir = "/run/secrets/nenya"
	}
	secrets, err = loadSecretsFromPath(secretsDir)
	if err != nil {
		return nil, err
	}
	if secrets != nil {
		return validateSecretsResult(secrets)
	}

	return nil, errors.New("secrets not found: checked " +
		"$CREDENTIALS_DIRECTORY/secrets, $CREDENTIALS_DIRECTORY/secrets.d/, " +
		"$NENYA_SECRETS_DIR/, /run/secrets/nenya/")
}

func validateSecretsResult(secrets *SecretsConfig) (*SecretsConfig, error) {
	if err := validateSecrets(secrets); err != nil {
		return nil, err
	}
	return secrets, nil
}

func validateSecrets(secrets *SecretsConfig) error {
	if secrets.ClientToken == "" {
		return errors.New("client_token is required")
	}
	for name, key := range secrets.ApiKeys {
		if err := key.Validate(); err != nil {
			return fmt.Errorf("invalid api_key %q: %w", name, err)
		}
	}
	return nil
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
		return nil, fmt.Errorf("failed to parse secrets: %v", err)
	}
	if secrets.ProviderKeys == nil {
		secrets.ProviderKeys = make(map[string]string)
	}
	if secrets.ApiKeys == nil {
		secrets.ApiKeys = make(map[string]ApiKey)
	}
	return &secrets, nil
}

func loadSecretsFromPath(path string) (*SecretsConfig, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to stat secrets path %q: %w", path, err)
	}

	if fi.IsDir() {
		return loadSecretsFromDir(path)
	}
	return loadSecretsSingleFile(path)
}

func loadSecretsFromDir(dir string) (*SecretsConfig, error) {
	if err := validateSecretsPath(dir); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read secrets directory: %w", err)
	}

	var result *SecretsConfig
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		secrets, err := loadSecretsSingleFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("failed to load secret %q: %w", entry.Name(), err)
		}
		result = mergeSecrets(result, secrets)
	}
	return result, nil
}

func loadSecretsSingleFile(path string) (*SecretsConfig, error) {
	if err := validateSecretsPath(path); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read secrets file: %w", err)
	}

	var secrets SecretsConfig
	if err := json.Unmarshal(data, &secrets); err != nil {
		return nil, fmt.Errorf("failed to parse secrets JSON: %w", err)
	}
	return &secrets, nil
}

func mergeSecrets(a, b *SecretsConfig) *SecretsConfig {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}

	if b.ClientToken != "" {
		a.ClientToken = b.ClientToken
	}

	if b.ProviderKeys != nil {
		if a.ProviderKeys == nil {
			a.ProviderKeys = make(map[string]string)
		}
		for k, v := range b.ProviderKeys {
			a.ProviderKeys[k] = v
		}
	}

	if b.ApiKeys != nil {
		if a.ApiKeys == nil {
			a.ApiKeys = make(map[string]ApiKey)
		}
		for k, v := range b.ApiKeys {
			if v.Enabled {
				a.ApiKeys[k] = v
			}
		}
	}

	return a
}

func validateSecretsPath(path string) error {
	_, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("invalid secrets path: %w", err)
	}
	return nil
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
			MaxRetryAttempts:     pc.MaxRetryAttempts,
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
