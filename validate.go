package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
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

	if err := validatePatterns("security_filter.patterns", cfg.SecurityFilter.Patterns, logger); err != nil {
		errors = append(errors, err.Error())
	}
	if err := validatePatterns("governance.blocked_execution_patterns", cfg.Governance.BlockedExecutionPatterns, logger); err != nil {
		errors = append(errors, err.Error())
	}

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
	req, err := http.NewRequest(http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := validationClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

var validationClient = &http.Client{Timeout: 30 * time.Second}

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
	if err = applyAuthHeader(req, provider); err != nil {
		return fmt.Errorf("failed to apply authentication: %v", err)
	}

	resp, err := validationClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("API key rejected (HTTP 401)")
		}
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

func getValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)
	path := u.Path

	switch {
	case strings.Contains(host, "generativelanguage.googleapis.com"):
		if idx := strings.Index(path, "/openai/chat/completions"); idx != -1 {
			return strings.TrimSuffix(providerURL, "/openai/chat/completions") + "/models"
		}
	case strings.Contains(host, "api.deepseek.com"):
		return "https://api.deepseek.com/models"
	case strings.Contains(host, "api.z.ai"):
		return "https://api.z.ai/v1/models"
	case strings.Contains(host, "api.groq.com"):
		return "https://api.groq.com/openai/v1/models"
	case strings.Contains(host, "api.together.xyz"):
		return "https://api.together.xyz/v1/models"
	case strings.Contains(host, "api.openai.com"):
		return "https://api.openai.com/v1/models"
	case strings.Contains(host, "127.0.0.1:11434") || strings.Contains(host, "localhost:11434"):
		return ""
	}

	if strings.HasSuffix(path, "/chat/completions") {
		return providerURL[:len(providerURL)-len("/chat/completions")] + "/models"
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

	if err = applyAuthHeader(req, provider); err != nil {
		return fmt.Errorf("failed to apply authentication: %v", err)
	}

	resp, err := validationClient.Do(req)
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
		req.Header.Set("Authorization", "Bearer "+provider.APIKey)
		req.Header.Set("x-goog-api-key", provider.APIKey)
	case "none":
	default:
		return fmt.Errorf("unsupported auth style: %s", provider.AuthStyle)
	}
	return nil
}

func validatePatterns(label string, patterns []string, logger *slog.Logger) error {
	var errs []string
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			msg := fmt.Sprintf("%s[%d]: %v", label, i, err)
			errs = append(errs, msg)
			logger.Error("pattern compile failed", "label", label, "index", i, "pattern", p, "err", err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid patterns in %s:\n  - %s", label, strings.Join(errs, "\n  - "))
	}
	return nil
}
