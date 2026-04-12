package config

import (
	"encoding/json"
	"fmt"
	"os"
)

func Load(filename string) (*Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file %s: %v", filename, err)
	}
	data = stripComments(data)
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %v", filename, err)
	}
	if err := ApplyDefaults(&cfg); err != nil {
		return nil, fmt.Errorf("failed to apply defaults: %v", err)
	}
	return &cfg, nil
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
	if credDir == "" {
		return nil, fmt.Errorf("CREDENTIALS_DIRECTORY not set")
	}
	secretsPath := credDir + "/secrets"
	data, err := os.ReadFile(secretsPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read secrets file %s: %v", secretsPath, err)
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
	if secrets.MemoryProviderKeys == nil {
		secrets.MemoryProviderKeys = make(map[string]string)
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
			APIKey:               apiKey,
			RoutePrefixes:        pc.RoutePrefixes,
			AuthStyle:            pc.AuthStyle,
			ApiFormat:            pc.ApiFormat,
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
