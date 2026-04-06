package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

func validateConfiguration(cfg *Config, secrets *SecretsConfig, logger *slog.Logger) error {
	logger.Info("starting configuration validation")

	// Validate each provider with API key
	providers := resolveProviders(cfg, secrets)

	// Validate engine provider health if configured and enabled (moved here after resolveProviders)
	if cfg.SecurityFilter.Enabled && cfg.SecurityFilter.Engine.Provider == "ollama" {
		if p, ok := providers[cfg.SecurityFilter.Engine.Provider]; ok && p.URL != "" {
			logger.Info("checking Ollama engine health", "provider", cfg.SecurityFilter.Engine.Provider, "url", p.URL)
			if !validateOllamaHealth(p.URL) {
				return fmt.Errorf("Ollama engine provider %q at %s is not reachable", cfg.SecurityFilter.Engine.Provider, p.URL)
			}
			logger.Info("Ollama engine health check passed")
		}
	}

	errors := []string{}

	for name, provider := range providers {
		if provider.APIKey == "" {
			logger.Warn("provider has no API key configured", "provider", name)
			continue
		}

		logger.Info("validating provider", "provider", name, "url", provider.URL)
		if err := validateProvider(provider, logger); err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", name, err))
			logger.Error("provider validation failed", "provider", name, "err", err)
		} else {
			logger.Info("provider validation passed", "provider", name)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("provider validation failed:\n  - %s", strings.Join(errors, "\n  - "))
	}

	logger.Info("configuration validation completed successfully")
	return nil
}

func validateOllamaHealth(ollamaURL string) bool {
	healthURL := ollamaHealthURL(ollamaURL)
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest(http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func validateProvider(provider *Provider, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Determine validation endpoint based on provider URL
	validationURL := getValidationEndpoint(provider.URL)
	if validationURL == "" {
		// Fallback to trying a minimal chat completion with streaming disabled
		return validateWithMinimalRequest(provider, ctx, logger)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, validationURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	// Apply authentication based on auth style
	if err := applyAuthHeader(req, provider); err != nil {
		return fmt.Errorf("failed to apply authentication: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		// 200 OK means valid, 401 means key is invalid (but endpoint exists)
		// We accept 401 because it means the endpoint is reachable and key format is wrong,
		// which is still a configuration issue the user needs to fix
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("API key rejected (HTTP 401)")
		}
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

func getValidationEndpoint(url string) string {
	// Map provider URLs to their model listing endpoints
	// Most OpenAI-compatible APIs support /v1/models
	if strings.Contains(url, "generativelanguage.googleapis.com") {
		// Gemini: https://generativelanguage.googleapis.com/v1beta/openai/chat/completions
		// Models endpoint: https://generativelanguage.googleapis.com/v1beta/models
		if idx := strings.Index(url, "/openai/chat/completions"); idx != -1 {
			return url[:idx] + "/models"
		}
	}
	if strings.Contains(url, "api.deepseek.com") {
		return "https://api.deepseek.com/models"
	}
	if strings.Contains(url, "api.z.ai") {
		return "https://api.z.ai/v1/models"
	}
	if strings.Contains(url, "api.groq.com") {
		return "https://api.groq.com/openai/v1/models"
	}
	if strings.Contains(url, "api.together.xyz") {
		return "https://api.together.xyz/v1/models"
	}
	if strings.Contains(url, "api.openai.com") {
		return "https://api.openai.com/v1/models"
	}
	// Ollama doesn't need validation beyond health check
	if strings.Contains(url, "127.0.0.1:11434") || strings.Contains(url, "localhost:11434") {
		return ""
	}
	// Default: try /v1/models relative to the chat completions endpoint
	if strings.HasSuffix(url, "/chat/completions") {
		return strings.TrimSuffix(url, "/chat/completions") + "/models"
	}
	return ""
}

func validateWithMinimalRequest(provider *Provider, ctx context.Context, logger *slog.Logger) error {
	// Create a minimal chat completion request with streaming disabled
	payload := `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":false,"max_tokens":1}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err := applyAuthHeader(req, provider); err != nil {
		return fmt.Errorf("failed to apply authentication: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		// 200 OK means success, 400 Bad Request might mean invalid model name but API is reachable
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API key rejected (HTTP 401)")
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

func applyAuthHeader(req *http.Request, provider *Provider) error {
	switch provider.AuthStyle {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
	case "bearer+x-goog":
		// Gemini style: Bearer token with x-goog-api-key header
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		req.Header.Set("x-goog-api-key", provider.APIKey)
	case "none":
		// No authentication required (e.g., Ollama)
	default:
		return fmt.Errorf("unsupported auth style: %s", provider.AuthStyle)
	}
	return nil
}
