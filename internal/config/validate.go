package config

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	providerpkg "nenya/internal/providers"
)

func closeBody(resp *http.Response) {
	if resp != nil {
		_ = resp.Body.Close()
	}
}

func ValidateConfiguration(ctx context.Context, cfg *Config, secrets *SecretsConfig, logger *slog.Logger) error {
	return ValidateConfigurationWithPing(ctx, cfg, secrets, logger, true)
}

func ValidateConfigurationNoPing(ctx context.Context, cfg *Config, secrets *SecretsConfig, logger *slog.Logger) error {
	return ValidateConfigurationWithPing(ctx, cfg, secrets, logger, false)
}

func ValidateConfigurationWithPing(ctx context.Context, cfg *Config, secrets *SecretsConfig, logger *slog.Logger, pingProviders bool) error {
	logger.Info("starting configuration validation")

	providers := ResolveProviders(cfg, secrets)

	if pingProviders && cfg.SecurityFilter.Enabled && cfg.SecurityFilter.Engine.Provider == "ollama" {
		if p, ok := providers[cfg.SecurityFilter.Engine.Provider]; ok && p.URL != "" {
			logger.Info("checking Ollama engine health", "provider", cfg.SecurityFilter.Engine.Provider, "url", p.URL)
			if !validateOllamaHealth(ctx, p.URL) {
				return fmt.Errorf("ollama engine provider %q at %s is not reachable", cfg.SecurityFilter.Engine.Provider, p.URL)
			}
			logger.Info("Ollama engine health check passed")
		}
	}

	errors := []string{}

	switch cfg.Governance.TFIDFQuerySource {
	case "", "prior_messages", "self":
	default:
		errors = append(errors, fmt.Sprintf("governance.tfidf_query_source: invalid value %q, must be empty, \"prior_messages\", or \"self\"", cfg.Governance.TFIDFQuerySource))
	}

	if err := ValidatePatterns("security_filter.patterns", cfg.SecurityFilter.Patterns, logger); err != nil {
		errors = append(errors, err.Error())
	}
	if err := ValidatePatterns("governance.blocked_execution_patterns", cfg.Governance.BlockedExecutionPatterns, logger); err != nil {
		errors = append(errors, err.Error())
	}

	if cfg.SecurityFilter.EntropyEnabled {
		if cfg.SecurityFilter.EntropyThreshold <= 0 || cfg.SecurityFilter.EntropyThreshold > 8.0 {
			errors = append(errors, "security_filter.entropy_threshold must be between 0 and 8.0")
		}
		if cfg.SecurityFilter.EntropyMinToken < 8 {
			errors = append(errors, "security_filter.entropy_min_token must be >= 8")
		}
	}

	if pingProviders {
		for name, provider := range providers {
			if provider.APIKey == "" {
				logger.Warn("provider has no API key configured", "provider", name)
				continue
			}

			logger.Info("validating provider", "provider", name, "url", provider.URL)
			if err := validateProvider(ctx, name, provider, logger); err != nil {
				errors = append(errors, fmt.Sprintf("%s: %v", name, err))
				logger.Error("provider validation failed", "provider", name, "err", err)
			} else {
				logger.Info("provider validation passed", "provider", name)
			}
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("provider validation failed:\n  - %s", strings.Join(errors, "\n  - "))
	}

	logger.Info("configuration validation completed successfully")
	return nil
}

func validateOllamaHealth(ctx context.Context, ollamaURL string) bool {
	healthURL := OllamaHealthURL(ollamaURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return false
	}
	resp, err := validationClient.Do(req)
	if err != nil {
		return false
	}
	defer closeBody(resp)
	return resp.StatusCode == http.StatusOK
}

var validationClient = &http.Client{Timeout: 30 * time.Second}

func validateProvider(ctx context.Context, name string, provider *Provider, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var validationURL string
	if spec, ok := providerpkg.Get(name); ok && spec.ValidationEndpoint != nil {
		validationURL = spec.ValidationEndpoint(provider.URL)
	}

	if validationURL == "" {
		return validateWithMinimalRequest(provider, ctx, logger)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, validationURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}

	if err = ApplyAuthHeader(req, provider); err != nil {
		return fmt.Errorf("failed to apply authentication: %v", err)
	}

	resp, err := validationClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusUnauthorized {
		if resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("API key rejected (HTTP 401)")
		}
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

func validateWithMinimalRequest(provider *Provider, ctx context.Context, logger *slog.Logger) error {
	payload := `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":false,"max_tokens":1}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err = ApplyAuthHeader(req, provider); err != nil {
		return fmt.Errorf("failed to apply authentication: %v", err)
	}

	resp, err := validationClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %v", err)
	}
	defer closeBody(resp)

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusBadRequest {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return fmt.Errorf("API key rejected (HTTP 401)")
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
}

func ApplyAuthHeader(req *http.Request, provider *Provider) error {
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

func ValidatePatterns(label string, patterns []string, logger *slog.Logger) error {
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

func OllamaHealthURL(engineURL string) string {
	const nativeSuffix = "/api/generate"
	const openaiSuffix = "/v1/chat/completions"
	if strings.HasSuffix(engineURL, nativeSuffix) {
		return engineURL[:len(engineURL)-len(nativeSuffix)] + "/api/tags"
	}
	if strings.HasSuffix(engineURL, openaiSuffix) {
		return engineURL[:len(engineURL)-len(openaiSuffix)] + "/api/tags"
	}
	return engineURL
}
