package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadPromptFile_DirectPrompt(t *testing.T) {
	prompt, err := LoadPromptFile("", "test prompt", "default prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "test prompt" {
		t.Errorf("expected 'test prompt', got %s", prompt)
	}
}

func TestLoadPromptFile_DefaultPrompt(t *testing.T) {
	prompt, err := LoadPromptFile("", "", "default prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "default prompt" {
		t.Errorf("expected 'default prompt', got %s", prompt)
	}
}

func TestLoadPromptFile_FromFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(promptPath, []byte("file prompt content"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_DIR", dir)

	prompt, err := LoadPromptFile(promptPath, "", "default prompt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prompt != "file prompt content" {
		t.Errorf("expected 'file prompt content', got %s", prompt)
	}
}

func TestLoadPromptFile_PathTraversal(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CONFIG_DIR", dir)

	_, err := LoadPromptFile("../etc/passwd", "", "default prompt")
	if err == nil {
		t.Error("expected error for path traversal")
	}
}

func TestLoadPromptFile_AbsolutePathOutsideConfig(t *testing.T) {
	t.Setenv("CONFIG_DIR", "/tmp")

	_, err := LoadPromptFile("/etc/passwd", "", "default prompt")
	if err == nil {
		t.Error("expected error for absolute path outside config")
	}
}

func TestValidatePromptPath_RelativeTraversal(t *testing.T) {
	tests := []string{
		"../secret",
		"../../etc/passwd",
		"./../../secret",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			err := validatePromptPath(path)
			if err == nil {
				t.Errorf("expected error for path %s", path)
			}
		})
	}
}

func TestValidatePromptPath_ValidRelativePath(t *testing.T) {
	tests := []string{
		"prompt.txt",
		"subdir/prompt.txt",
		"prompts/system.txt",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			err := validatePromptPath(path)
			if err != nil {
				t.Errorf("unexpected error for %s: %v", path, err)
			}
		})
	}
}

func TestTryLoadCredFile_NoEnv(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	secrets, err := tryLoadCredFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets != nil {
		t.Error("expected nil when CREDENTIALS_DIRECTORY not set")
	}
}

func TestTryLoadCredFile_NotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	secrets, err := tryLoadCredFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets != nil {
		t.Error("expected nil when file not found")
	}
}

func TestTryLoadCredFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secrets"), []byte("{invalid json"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	_, err := tryLoadCredFile()
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestTryLoadCredFile_Valid(t *testing.T) {
	dir := t.TempDir()
	secretContent := `{"client_token": "test-token-1234567890", "provider_keys": {"openai": "sk-key"}}`
	if err := os.WriteFile(filepath.Join(dir, "secrets"), []byte(secretContent), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)

	secrets, err := tryLoadCredFile()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets == nil {
		t.Fatal("expected secrets to be loaded")
	}
	if secrets.ClientToken != "test-token-1234567890" {
		t.Errorf("expected client token 'test-token-1234567890', got %s", secrets.ClientToken)
	}
	if secrets.ProviderKeys == nil || secrets.ProviderKeys["openai"] != "sk-key" {
		t.Error("expected provider key 'sk-key'")
	}
}

func TestLoadSecretsFromPath_Directory(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets.d")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}
	secretContent := `{"client_token": "test-token-1234567890"}`
	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(secretContent), 0644); err != nil {
		t.Fatal(err)
	}

	secrets, err := loadSecretsFromPath(secretsDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets.ClientToken != "test-token-1234567890" {
		t.Errorf("expected 'test-token-1234567890', got %s", secrets.ClientToken)
	}
}

func TestLoadSecretsFromPath_SingleFile(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secrets.json")
	secretContent := `{"client_token": "test-token-1234567890", "provider_keys": {"openai": "sk-key"}}`
	if err := os.WriteFile(secretFile, []byte(secretContent), 0644); err != nil {
		t.Fatal(err)
	}

	secrets, err := loadSecretsFromPath(secretFile)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets.ClientToken != "test-token-1234567890" {
		t.Errorf("expected 'test-token-1234567890', got %s", secrets.ClientToken)
	}
}

func TestLoadSecretsFromPath_NotFound(t *testing.T) {
	secrets, err := loadSecretsFromPath("/nonexistent/secrets")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets != nil {
		t.Error("expected nil for nonexistent path")
	}
}

func TestLoadSecretsFromDir_MultipleFiles(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets.d")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(secretsDir, "01-client.json"), []byte(`{"client_token": "token-1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "02-provider.json"), []byte(`{"provider_keys": {"openai": "sk-key"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	secrets, err := loadSecretsFromDir(secretsDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets.ClientToken != "token-1" {
		t.Errorf("expected 'token-1', got %s", secrets.ClientToken)
	}
	if secrets.ProviderKeys["openai"] != "sk-key" {
		t.Error("expected provider key to be merged")
	}
}

func TestLoadSecretsFromDir_IgnoresNonJSON(t *testing.T) {
	dir := t.TempDir()
	secretsDir := filepath.Join(dir, "secrets.d")
	if err := os.MkdirAll(secretsDir, 0755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(secretsDir, "secrets.json"), []byte(`{"client_token": "token-1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "README.txt"), []byte("readme"), 0644); err != nil {
		t.Fatal(err)
	}

	secrets, err := loadSecretsFromDir(secretsDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if secrets.ClientToken != "token-1" {
		t.Error("should have loaded valid JSON file")
	}
}

func TestMergeSecrets_BothNil(t *testing.T) {
	result := mergeSecrets(nil, nil)
	if result != nil {
		t.Error("expected nil for both nil")
	}
}

func TestMergeSecrets_FirstNil(t *testing.T) {
	second := &SecretsConfig{ClientToken: "token-2"}
	result := mergeSecrets(nil, second)
	if result != second {
		t.Error("expected second to be returned when first is nil")
	}
}

func TestMergeSecrets_SecondNil(t *testing.T) {
	first := &SecretsConfig{ClientToken: "token-1"}
	result := mergeSecrets(first, nil)
	if result != first {
		t.Error("expected first to be returned when second is nil")
	}
}

func TestMergeSecrets_Merge(t *testing.T) {
	first := &SecretsConfig{
		ClientToken: "token-1",
		ProviderKeys: map[string]string{
			"openai": "sk-key-1",
		},
		ApiKeys: map[string]ApiKey{
			"key-1": {Name: "key-1", Token: "token-1", Roles: []string{"admin"}, Enabled: true},
		},
	}
	second := &SecretsConfig{
		ClientToken: "token-2",
		ProviderKeys: map[string]string{
			"anthropic": "sk-key-2",
		},
		ApiKeys: map[string]ApiKey{
			"key-2": {Name: "key-2", Token: "token-2", Roles: []string{"user"}, Enabled: true},
		},
	}

	result := mergeSecrets(first, second)
	if result.ClientToken != "token-2" {
		t.Errorf("expected 'token-2', got %s", result.ClientToken)
	}
	if result.ProviderKeys["openai"] != "sk-key-1" {
		t.Error("expected first provider key to be preserved")
	}
	if result.ProviderKeys["anthropic"] != "sk-key-2" {
		t.Error("expected second provider key to be added")
	}
	if result.ApiKeys["key-1"].Token != "token-1" {
		t.Error("expected first API key to be preserved")
	}
	if result.ApiKeys["key-2"].Token != "token-2" {
		t.Error("expected second API key to be added")
	}
}

func TestMergeSecrets_DisabledKeysNotMerged(t *testing.T) {
	first := &SecretsConfig{
		ApiKeys: map[string]ApiKey{
			"key-1": {Name: "key-1", Token: "token-1", Roles: []string{"admin"}, Enabled: true},
		},
	}
	second := &SecretsConfig{
		ApiKeys: map[string]ApiKey{
			"key-2": {Name: "key-2", Token: "token-2", Roles: []string{"user"}, Enabled: false},
		},
	}

	result := mergeSecrets(first, second)
	if _, ok := result.ApiKeys["key-2"]; ok {
		t.Error("disabled API key should not be merged")
	}
	if _, ok := result.ApiKeys["key-1"]; !ok {
		t.Error("enabled API key should be preserved")
	}
}

func TestValidateSecretsPath_Valid(t *testing.T) {
	tests := []string{
		"/tmp/config",
		"./config",
		"config",
	}
	for _, path := range tests {
		t.Run(path, func(t *testing.T) {
			err := validateSecretsPath(path)
			if err != nil {
				t.Errorf("unexpected error for %s: %v", path, err)
			}
		})
	}
}

func TestResolveProviders(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"openai": {
				URL:         "https://api.openai.com/v1/chat/completions",
				AuthStyle:   "bearer",
				ApiFormat:   "openai",
				TimeoutSeconds: 30,
			},
			"anthropic": {
				URL:         "https://api.anthropic.com/v1/messages",
				AuthStyle:   "bearer",
				ApiFormat:   "anthropic",
				TimeoutSeconds: 60,
			},
		},
	}
	secrets := &SecretsConfig{
		ProviderKeys: map[string]string{
			"openai":    "sk-openai-key",
			"anthropic": "sk-ant-key",
		},
	}

	providers := ResolveProviders(cfg, secrets)
	if len(providers) != 2 {
		t.Errorf("expected 2 providers, got %d", len(providers))
	}

	openai, ok := providers["openai"]
	if !ok {
		t.Fatal("expected openai provider")
	}
	if openai.APIKey != "sk-openai-key" {
		t.Errorf("expected 'sk-openai-key', got %s", openai.APIKey)
	}
	if openai.BaseURL != "https://api.openai.com" {
		t.Errorf("expected 'https://api.openai.com', got %s", openai.BaseURL)
	}

	anthropic, ok := providers["anthropic"]
	if !ok {
		t.Fatal("expected anthropic provider")
	}
	if anthropic.APIKey != "sk-ant-key" {
		t.Errorf("expected 'sk-ant-key', got %s", anthropic.APIKey)
	}
}

func TestResolveProviders_NoSecrets(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"none-auth": {
				URL:       "https://api.example.com/v1/chat/completions",
				AuthStyle: "none",
			},
		},
	}

	providers := ResolveProviders(cfg, nil)
	if len(providers) != 1 {
		t.Errorf("expected 1 provider, got %d", len(providers))
	}

	p, ok := providers["none-auth"]
	if !ok {
		t.Fatal("expected none-auth provider")
	}
	if p.APIKey != "" {
		t.Errorf("expected empty API key, got %s", p.APIKey)
	}
}

func TestDeriveBaseURL_ValidURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://api.openai.com/v1/chat/completions", "https://api.openai.com"},
		{"https://api.anthropic.com/v1/messages", "https://api.anthropic.com"},
		{"http://localhost:11434/api/generate", "http://localhost:11434"},
		{"https://example.com", "https://example.com"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := deriveBaseURL(tt.input)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestDeriveBaseURL_InvalidURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"://invalid", "://invalid"},
		{"not a url", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := deriveBaseURL(tt.input)
			if result != tt.expected {
				t.Errorf("expected %s, got %s", tt.expected, result)
			}
		})
	}
}

func TestLoad_DirectoryPath(t *testing.T) {
	dir := t.TempDir()
	_, err := Load(dir)
	if err == nil {
		t.Fatal("expected error for directory path")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("expected 'is a directory' error, got %v", err)
	}
}



func TestLoadFromDir_NoConfig(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadFromDir(dir)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestLoadFromDir_BothConfigAndConfigD(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"server": {"listen_addr": ":9090"}}`), 0644); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dir, "config.d")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "01-server.json"), []byte(`{"server": {"listen_addr": ":9090", "log_level": "debug"}}`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadFromDir(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.ListenAddr != ":9090" {
		t.Errorf("expected :9090 from config.d, got %s", cfg.Server.ListenAddr)
	}
	if cfg.Server.LogLevel != "debug" {
		t.Errorf("expected debug from config.d, got %s", cfg.Server.LogLevel)
	}
}
