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

// ErrConfigNotFound marks a load that failed because no config source exists.
// It is non-fatal for diagnostic surfaces such as `nenya describe`
// (CONTRACT.md §4.3), which report it as a config_not_found diagnostic and
// fall back to defaults; callers that require a config treat it as fatal.
// Detect it with errors.Is; use ConfigNotFoundError for the human detail.
var ErrConfigNotFound = errors.New("config not found")

// ConfigNotFoundError is the error Load/LoadFromDir return when no config
// source exists. It matches ErrConfigNotFound under errors.Is and carries the
// human-readable detail (paths tried) in Detail, so callers do not have to
// parse the message.
type ConfigNotFoundError struct {
	Detail string
}

func (e *ConfigNotFoundError) Error() string { return "config not found: " + e.Detail }

// Is reports ConfigNotFoundError as ErrConfigNotFound.
func (e *ConfigNotFoundError) Is(target error) bool { return target == ErrConfigNotFound }

// Load reads and parses a single JSON config file from path. Returns
// the parsed Config with defaults applied, or an error if the file
// cannot be read, contains invalid JSON, or defaults cannot be applied.
func Load(path string) (*Config, error) {
	cfg, _, err := LoadWithDiagnostics(path)
	return cfg, err
}

// LoadWithDiagnostics is Load plus the non-fatal diagnostics gathered while
// decoding (currently unknown-field warnings), for `nenya describe`.
func LoadWithDiagnostics(path string) (*Config, []Diagnostic, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, &ConfigNotFoundError{Detail: fmt.Sprintf("config file %s does not exist", path)}
		}
		return nil, nil, fmt.Errorf("failed to access config path %s: %w", path, err)
	}

	if info.IsDir() {
		return nil, nil, fmt.Errorf("config path %s is a directory, use LoadFromDir() instead", path)
	}

	cfg, diags, err := decodeConfigFileWithDiagnostics(path)
	if err != nil {
		return nil, nil, err
	}
	if err := ApplyDefaults(cfg); err != nil {
		return nil, nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return cfg, diags, nil
}

// decodeConfigFileWithDiagnostics reads, comment-strips, and decodes a single
// config file without applying defaults, returning the diagnostics gathered
// while decoding. Callers apply defaults once after merging.
func decodeConfigFileWithDiagnostics(path string) (*Config, []Diagnostic, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read config file %s: %v", path, err)
	}
	data = StripComments(data)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("failed to parse config file %s: %v", path, err)
	}
	var diags []Diagnostic
	if d := unknownFieldDiagnostic(data, path); d != nil {
		diags = append(diags, *d)
	}
	return &cfg, diags, nil
}

// unknownFieldDiagnostic performs a strict secondary decode and, when it hits
// an unknown field, logs a warning naming it and returns the diagnostic.
// Lenient decoding always wins — unknown fields never fail load or SIGHUP
// reload (NENYA-19) — but the warning surfaces likely typos that lenient
// parsing would silently drop.
func unknownFieldDiagnostic(data []byte, path string) *Diagnostic {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var probe Config
	if err := dec.Decode(&probe); err != nil {
		if name, ok := unknownFieldName(err); ok {
			slog.Warn("config contains unknown field, ignored (possible typo)",
				"field", name, "path", path)
			return &Diagnostic{
				Level:   "warn",
				Code:    "unknown_field",
				Message: fmt.Sprintf("unknown field %q ignored", name),
				Source:  path,
			}
		}
	}
	return nil
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
	cfg, _, err := LoadFromDirWithDiagnostics(dir)
	return cfg, err
}

// LoadFromDirWithDiagnostics is LoadFromDir plus the non-fatal diagnostics
// gathered while decoding each merged file, for `nenya describe`.
func LoadFromDirWithDiagnostics(dir string) (*Config, []Diagnostic, error) {
	configFilePath := filepath.Join(dir, "config.json")
	configDirPath := filepath.Join(dir, "config.d")

	dropIns, err := configDropInFiles(configDirPath)
	if err != nil {
		return nil, nil, err
	}

	merged := &Config{}
	var diags []Diagnostic
	found := false

	info, statErr := os.Stat(configFilePath)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		// A stat failure (for example EACCES) is not "no config": surface it
		// instead of silently reporting config_not_found.
		return nil, nil, fmt.Errorf("failed to access config %s: %w", configFilePath, statErr)
	}
	if statErr == nil && !info.IsDir() {
		base, baseDiags, loadErr := decodeConfigFileWithDiagnostics(configFilePath)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		merged = base
		diags = append(diags, baseDiags...)
		found = true
	}

	for _, filePath := range dropIns {
		partial, partialDiags, loadErr := decodeConfigFileWithDiagnostics(filePath)
		if loadErr != nil {
			return nil, nil, loadErr
		}
		diags = append(diags, partialDiags...)
		mergeConfig(merged, partial)
		found = true
	}

	if !found {
		return nil, nil, &ConfigNotFoundError{Detail: fmt.Sprintf("no config found in %s (tried %s and %s/*.json)", dir, configFilePath, configDirPath)}
	}

	if err := ApplyDefaults(merged); err != nil {
		return nil, nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return merged, diags, nil
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

// DefaultSecretsDir is the default directory secrets are merged from when
// NENYA_SECRETS_DIR is unset (CONTRACT.md §6.1 source 4).
const DefaultSecretsDir = "/run/secrets/nenya"

// SecretsResolution reports where secrets were looked for and which source won
// (CONTRACT.md §6.1). Secrets is nil when none was found or when validation
// failed; ActiveSource is set as soon as a source yields a document;
// FailedSource names the candidate whose read/validation failed, when one did.
type SecretsResolution struct {
	ActiveSource string
	FailedSource string
	Searched     []string
	Secrets      *SecretsConfig
}

// ResolveSecrets locates and validates secrets without failing when none is
// found, so managers can inspect the search order (`nenya describe`). It checks
// in order (CONTRACT.md §6.1):
//  1. $CREDENTIALS_DIRECTORY/secrets
//  2. $CREDENTIALS_DIRECTORY/secrets.d/ (directory)
//     3/4. $NENYA_SECRETS_DIR (a file or a merged directory) or, when unset, the
//     /run/secrets/nenya directory — one probe, the env var replacing the
//     default
//  5. <configRoot>/secrets.json (single file, directory mode only)
//
// Source 5 is the deployment's conventional secrets file: the shipped systemd
// unit wires it via LoadCredential (which wins as source 1 under the unit), and
// it is `secret set`'s default target. Searching it last lets an interactive
// `nenya -config-dir <root>` resolve the same secrets the unit would load,
// without changing precedence for sources 1-4.
//
// A non-nil error reports a read or validation failure; LoadSecrets treats a
// missing document as fatal.
func ResolveSecrets(configRoot string) (SecretsResolution, error) {
	credDir := os.Getenv("CREDENTIALS_DIRECTORY")
	secretsDir := os.Getenv("NENYA_SECRETS_DIR")
	if secretsDir == "" {
		secretsDir = DefaultSecretsDir
	}
	secretsDir = CleanAbs(secretsDir)

	res := SecretsResolution{}
	if credDir != "" {
		credDir = CleanAbs(credDir)
		res.Searched = append(res.Searched, filepath.Join(credDir, "secrets"), filepath.Join(credDir, "secrets.d"))
	} else {
		res.Searched = append(res.Searched, "<CREDENTIALS_DIRECTORY>/secrets", "<CREDENTIALS_DIRECTORY>/secrets.d")
	}
	res.Searched = append(res.Searched, secretsDir)

	configSecrets := ""
	if configRoot != "" {
		configSecrets = filepath.Join(CleanAbs(configRoot), "secrets.json")
		res.Searched = append(res.Searched, configSecrets)
	}

	if found, err := resolveCredentialDirectory(&res, credDir); err != nil {
		return res, err
	} else if found {
		return res, nil
	}
	if found, err := resolveSecretsPath(&res, secretsDir); err != nil {
		return res, err
	} else if found {
		return res, nil
	}
	if configSecrets != "" {
		if found, err := resolveSecretsFile(&res, configSecrets); err != nil {
			return res, err
		} else if found {
			return res, nil
		}
	}
	return res, nil
}

// CleanAbs returns p as an absolute, cleaned path, or p unchanged when it
// cannot be resolved, so sources recorded in SecretsResolution match the paths
// other surfaces (paths --json, secret set) report.
func CleanAbs(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// resolveSecretsFile tries a single-file secrets source (CONTRACT.md §6.1
// source 5). A missing path yields false; a directory at path is not this
// source and is skipped rather than merged. It records the winner.
func resolveSecretsFile(res *SecretsResolution, path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		res.FailedSource = path
		return false, fmt.Errorf("failed to stat secrets path %q: %w", path, err)
	}
	if info.IsDir() {
		return false, nil
	}
	secrets, err := loadSecretsSingleFile(path)
	if err != nil {
		res.FailedSource = path
		return false, fmt.Errorf("failed to load secrets file %q: %w", path, err)
	}
	if secrets == nil {
		return false, nil
	}
	return recordSecrets(res, path, secrets)
}

// resolveCredentialDirectory tries the systemd credential sources
// ($CREDENTIALS_DIRECTORY/secrets then $CREDENTIALS_DIRECTORY/secrets.d) and
// records the winner. It reports whether a valid document was found.
func resolveCredentialDirectory(res *SecretsResolution, credDir string) (bool, error) {
	if credDir == "" {
		return false, nil
	}
	secrets, err := tryLoadCredFile(credDir)
	if err != nil {
		res.FailedSource = filepath.Join(credDir, "secrets")
		return false, err
	}
	if secrets != nil {
		return recordSecrets(res, filepath.Join(credDir, "secrets"), secrets)
	}
	secrets, err = loadSecretsFromPath(filepath.Join(credDir, "secrets.d"))
	if err != nil {
		res.FailedSource = filepath.Join(credDir, "secrets.d")
		return false, err
	}
	if secrets == nil {
		return false, nil
	}
	return recordSecrets(res, filepath.Join(credDir, "secrets.d"), secrets)
}

// resolveSecretsPath tries a single secrets path and records the winner.
func resolveSecretsPath(res *SecretsResolution, path string) (bool, error) {
	secrets, err := loadSecretsFromPath(path)
	if err != nil {
		res.FailedSource = path
		return false, err
	}
	if secrets == nil {
		return false, nil
	}
	return recordSecrets(res, path, secrets)
}

// recordSecrets validates secrets and records the source that produced them.
func recordSecrets(res *SecretsResolution, source string, secrets *SecretsConfig) (bool, error) {
	res.ActiveSource = source
	validated, vErr := validateSecretsResult(secrets)
	if vErr != nil {
		return false, vErr
	}
	res.Secrets = validated
	return true, nil
}

// LoadSecrets loads and validates the secrets configuration (see
// ResolveSecrets for the search order; configRoot is the directory-mode config
// root, empty in file mode). Returns an error if no secrets are found or
// validation fails.
func LoadSecrets(configRoot string) (*SecretsConfig, error) {
	res, err := ResolveSecrets(configRoot)
	if err != nil {
		return nil, err
	}
	if res.Secrets == nil {
		return nil, errors.New("secrets not found: checked " + strings.Join(res.Searched, ", "))
	}
	return res.Secrets, nil
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

func tryLoadCredFile(credDir string) (*SecretsConfig, error) {
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
		// A secrets directory pointed at a config root (the launchd unit sets
		// NENYA_SECRETS_DIR=/etc/nenya) contains config.json, which is JSONC
		// and not a secrets document; skip it rather than fail the load.
		if entry.Name() == "config.json" {
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
