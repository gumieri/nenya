package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Load reads and parses a single JSON config file from path. Returns
// the parsed Config with defaults applied, or an error if the file
// cannot be read, contains invalid JSON, or defaults cannot be applied.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("failed to access config path %s: %v", path, err)
	}

	if info.IsDir() {
		return nil, fmt.Errorf("config path %s is a directory, use LoadFromDir() instead", path)
	}

	cfg, err := decodeConfigFile(path)
	if err != nil {
		return nil, err
	}
	if err := ApplyDefaults(cfg); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return cfg, nil
}

// decodeConfigFile reads, comment-strips, and decodes a single config file
// without applying defaults. Callers apply defaults once after merging.
func decodeConfigFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", path, err)
	}
	data = StripComments(data)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", path, err)
	}
	warnUnknownFields(data, path)
	return &cfg, nil
}

// warnUnknownFields performs a strict secondary decode and logs a warning
// naming the first unknown field it hits. Lenient decoding always wins —
// unknown fields never fail load or SIGHUP reload (NENYA-19) — but the
// warning surfaces likely typos that lenient parsing would silently drop.
func warnUnknownFields(data []byte, path string) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var probe Config
	if err := dec.Decode(&probe); err != nil {
		if name, ok := unknownFieldName(err); ok {
			slog.Warn("config contains unknown field, ignored (possible typo)",
				"field", name, "path", path)
		}
	}
}

// unknownFieldName extracts the field name from encoding/json's
// "json: unknown field \"x\"" error.
func unknownFieldName(err error) (string, bool) {
	const marker = "json: unknown field "
	msg := err.Error()
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return "", false
	}
	rest := msg[idx+len(marker):]
	if name, uerr := strconv.Unquote(rest); uerr == nil {
		return name, true
	}
	return rest, true
}

// LoadFromDir loads configuration from a directory. `config.json`, when
// present, is the base; `config.d/*.json` (excluding `secrets.json`) are then
// merged over it in ascending filename order. With no `config.d/` the result is
// just `config.json`. Returns an error if neither source exists.
//
// This is the conf.d model: a drop-in augments the base instead of replacing
// it. Previously a non-empty `config.d/` caused `config.json` to be ignored
// entirely, which silently discarded the base when a consumer wrote a single
// drop-in.
func LoadFromDir(dir string) (*Config, error) {
	configFilePath := filepath.Join(dir, "config.json")
	configDirPath := filepath.Join(dir, "config.d")

	dropIns, err := configDropInFiles(configDirPath)
	if err != nil {
		return nil, err
	}

	merged := &Config{}
	found := false

	if info, statErr := os.Stat(configFilePath); statErr == nil && !info.IsDir() {
		base, loadErr := decodeConfigFile(configFilePath)
		if loadErr != nil {
			return nil, loadErr
		}
		merged = base
		found = true
	}

	for _, filePath := range dropIns {
		partial, loadErr := decodeConfigFile(filePath)
		if loadErr != nil {
			return nil, loadErr
		}
		mergeConfig(merged, partial)
		found = true
	}

	if !found {
		return nil, fmt.Errorf("no config found in %s (tried %s and %s/*.json)", dir, configFilePath, configDirPath)
	}

	if err := ApplyDefaults(merged); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return merged, nil
}

// configDropInFiles returns the sorted `*.json` files in dir, excluding
// `secrets.json`. A missing directory yields an empty slice.
func configDropInFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read config directory %s: %v", dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || name == "secrets.json" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	slices.Sort(files)
	return files, nil
}

// mergeConfig applies overlay onto base with the directory-mode merge rules:
// exported struct fields recurse; pointers to structs deep-merge; pointers to
// scalars, non-empty slices, and non-nil maps are applied (maps merge per key);
// plain scalars apply when non-zero. The rules cover every field of Config
// structurally, so a newly added field merges without touching this function.
// The previous hand-written per-section mergers silently dropped any field they
// did not enumerate (for example governance.injection/canary/exfil_guard).
func mergeConfig(base, overlay *Config) {
	mergeValue(reflect.ValueOf(base).Elem(), reflect.ValueOf(overlay).Elem())
}

var timeType = reflect.TypeOf(time.Time{})

// mergeValue merges src into dst in place following the rules documented on
// mergeConfig. Unexported fields are skipped.
func mergeValue(dst, src reflect.Value) {
	switch src.Kind() {
	case reflect.Pointer:
		mergePointer(dst, src)
	case reflect.Struct:
		mergeStruct(dst, src)
	case reflect.Map:
		mergeMapValue(dst, src)
	case reflect.Slice:
		if src.Len() > 0 {
			dst.Set(src)
		}
	case reflect.Bool:
		if src.Bool() {
			dst.SetBool(true)
		}
	default:
		if !src.IsZero() {
			dst.Set(src)
		}
	}
}

// mergePointer keeps whole-pointer semantics for scalars (so an explicit
// false/0 override survives) and deep-merges pointers to structs.
func mergePointer(dst, src reflect.Value) {
	if src.IsNil() {
		return
	}
	if src.Elem().Kind() != reflect.Struct || src.Elem().Type() == timeType {
		dst.Set(src)
		return
	}
	if dst.IsNil() {
		dst.Set(reflect.New(src.Elem().Type()))
	}
	mergeValue(dst.Elem(), src.Elem())
}

// mergeStruct merges each exported field; unexported fields are left as-is.
func mergeStruct(dst, src reflect.Value) {
	if src.Type() == timeType {
		if !src.IsZero() {
			dst.Set(src)
		}
		return
	}
	for i := 0; i < src.NumField(); i++ {
		if src.Type().Field(i).PkgPath != "" {
			continue // unexported
		}
		mergeValue(dst.Field(i), src.Field(i))
	}
}

// mergeMapValue merges maps per key (replacing each present key's value).
func mergeMapValue(dst, src reflect.Value) {
	if src.Len() == 0 {
		return
	}
	if dst.IsNil() {
		dst.Set(reflect.MakeMap(src.Type()))
	}
	iter := src.MapRange()
	for iter.Next() {
		dst.SetMapIndex(iter.Key(), iter.Value())
	}
}

// StripComments removes C-style (// and /* */) comments from JSON data,
// preserving strings that contain comment-like sequences. Returns the
// comment-free byte slice.
func StripComments(data []byte) []byte {
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

// LoadPromptFile loads a prompt from a file, returning directPrompt if
// non-empty, defaultPrompt if filePath is empty, or reading filePath
// with path traversal validation. Returns the prompt string or an error.
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
		return fmt.Errorf("cannot resolve absolute path for prompt file: %w", err)
	}

	configDir := os.Getenv("CONFIG_DIR")
	if configDir == "" {
		return nil
	}

	absConfigDir, err := filepath.Abs(configDir)
	if err != nil {
		return fmt.Errorf("cannot resolve absolute path for config directory: %w", err)
	}

	relPath, err := filepath.Rel(absConfigDir, absPath)
	if err != nil {
		return fmt.Errorf("cannot compute relative path for prompt file: %w", err)
	}

	if strings.HasPrefix(relPath, ".."+string(filepath.Separator)) || relPath == ".." {
		return fmt.Errorf("prompt file path escapes config directory: %s", filePath)
	}

	return nil
}

// LoadSecrets loads the secrets configuration from systemd credential
// files or standard secrets paths. It checks in order:
//  1. $CREDENTIALS_DIRECTORY/secrets
//  2. $CREDENTIALS_DIRECTORY/secrets.d/ (directory)
//  3. $NENYA_SECRETS_DIR/ (or /run/secrets/nenya/ as default)
//
// Returns an error if no secrets are found or validation fails.
func LoadSecrets() (*SecretsConfig, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	secretsDir := os.Getenv("NENYA_SECRETS_DIR")

	secrets, err := tryLoadCredFile()
	if err != nil {
		return nil, err
	}
	if secrets != nil {
		return validateSecretsResult(secrets)
	}

	if credDir != "" {
		secrets, err = loadSecretsFromPath(credDir + "/secrets.d")
		if err != nil {
			return nil, err
		}
		if secrets != nil {
			return validateSecretsResult(secrets)
		}
	}

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

// ResolveProviders creates the runtime Provider map from Config and
// Secrets. Each ProviderConfig is merged with the matching API key from
// secrets and derives additional fields (BaseURL, etc.).
func ResolveProviders(cfg *Config, secrets *SecretsConfig) map[string]*Provider {
	providers := make(map[string]*Provider, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		apiKey := ""
		if secrets != nil {
			apiKey = secrets.ProviderKeys[name]
		}
		compiledRE, err := CompileAllowedModels(pc.AllowedModels)
		if err != nil {
			fmt.Printf("[ERROR] failed to compile allowed_models for provider %q: %v (skipping provider)\n", name, err)
			continue
		}
		compiledNonChat, err := CompileNonChatModels(pc.NonChatModels)
		if err != nil {
			fmt.Printf("[ERROR] failed to compile non_chat_models for provider %q: %v (skipping provider)\n", name, err)
			continue
		}
		providers[name] = &Provider{
			Name:                         name,
			URL:                          pc.URL,
			BaseURL:                      deriveBaseURL(pc.URL),
			FormatURLs:                   pc.FormatURLs,
			APIKey:                       apiKey,
			AuthStyle:                    pc.AuthStyle,
			ApiFormat:                    pc.ApiFormat,
			TimeoutSeconds:               pc.TimeoutSeconds,
			IdleConnTimeoutSeconds:       pc.IdleConnTimeoutSeconds,
			ResponseHeaderTimeoutSeconds: pc.ResponseHeaderTimeoutSeconds,
			StreamIdleTimeoutSeconds:     pc.StreamIdleTimeoutSeconds,
			RetryableStatusCodes:         pc.RetryableStatusCodes,
			MaxRetryAttempts:             pc.MaxRetryAttempts,
			RetryablePhrases:             pc.RetryablePhrases,
			RequestScopedErrors:          pc.RequestScopedErrors,
			Thinking:                     pc.Thinking,
			Billing:                      pc.Billing,
			AllowedModels:                pc.AllowedModels,
			allowedRE:                    compiledRE,
			NonChatModels:                pc.NonChatModels,
			nonChatRE:                    compiledNonChat,
			MaxConcurrentRequests:        pc.MaxConcurrentRequests,
			ModelConcurrency:             pc.ModelConcurrency,
			ModelAliases:                 pc.ModelAliases,
			TokenBudgetDaily:             pc.TokenBudgetDaily,
			StreamBootstrapBufferBytes:   pc.StreamBootstrapBufferBytes,
			ThoughtSignaturePolicy:       derefString(pc.ThoughtSignaturePolicy),
			SessionStickyKeys:            pc.SessionStickyKeys,
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

// BuiltInProviders returns the default provider configurations from the
// ProviderRegistry as ProviderConfig values, ready for merging into the
// user's config via applyBuiltInProviders.
func BuiltInProviders() map[string]ProviderConfig {
	providers := make(map[string]ProviderConfig, len(ProviderRegistry))
	for name, entry := range ProviderRegistry {
		providers[name] = entry.ToProviderConfig()
	}
	return providers
}

// derefString returns the pointed-to string, or "" for nil.
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
