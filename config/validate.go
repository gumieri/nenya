package config

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"regexp"
	"strings"
	"time"
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

	if err := validateOllamaEngine(ctx, cfg, providers, pingProviders, logger); err != nil {
		return err
	}

	errors := collectValidationErrors(ctx, cfg, providers, pingProviders, logger)
	if len(errors) > 0 {
		return fmt.Errorf("provider validation failed:\n  - %s", strings.Join(errors, "\n  - "))
	}

	logger.Info("configuration validation completed successfully")
	return nil
}

func validateOllamaEngine(ctx context.Context, cfg *Config, providers map[string]*Provider, pingProviders bool, logger *slog.Logger) error {
	if !pingProviders {
		return nil
	}
	if cfg.Bouncer.Enabled == nil || !*cfg.Bouncer.Enabled {
		return nil
	}
	if cfg.Bouncer.Engine.Provider != "ollama" {
		return nil
	}

	p, ok := providers[cfg.Bouncer.Engine.Provider]
	if !ok || p.URL == "" {
		return nil
	}

	logger.Info("checking Ollama engine health", "provider", cfg.Bouncer.Engine.Provider, "url", p.URL)
	if !validateOllamaHealth(ctx, p.URL) {
		return fmt.Errorf("ollama engine provider %q at %s is not reachable", cfg.Bouncer.Engine.Provider, p.URL)
	}
	logger.Info("Ollama engine health check passed")
	return nil
}

func collectValidationErrors(ctx context.Context, cfg *Config, providers map[string]*Provider, pingProviders bool, logger *slog.Logger) []string {
	errors := []string{}

	errors = append(errors, validateTFIDFQuerySource(cfg.Context.TFIDFQuerySource)...)
	errors = append(errors, validatePatternsToList("bouncer.patterns", cfg.Bouncer.RedactPatterns, logger)...)
	errors = append(errors, validatePatternsToList("governance.blocked_execution_patterns", cfg.Governance.BlockedExecutionPatterns, logger)...)
	errors = append(errors, validateModelRegistryErrors(logger)...)
	errors = append(errors, validateEntropyConfig(cfg.Bouncer)...)

	if pingProviders {
		errors = append(errors, validateProviders(ctx, providers, logger)...)
	}

	return errors
}

func validateTFIDFQuerySource(source string) []string {
	switch source {
	case "", "prior_messages", "self":
		return nil
	default:
		return []string{fmt.Sprintf("governance.tfidf_query_source: invalid value %q, must be empty, \"prior_messages\", or \"self\"", source)}
	}
}

func validateModelRegistryErrors(logger *slog.Logger) []string {
	errors := []string{}
	for modelID, entry := range ModelRegistry {
		if err := entry.Validate(); err != nil {
			errors = append(errors, fmt.Sprintf("model_registry[%q]: %v", modelID, err))
		}
	}
	return errors
}

func validateEntropyConfig(sf BouncerConfig) []string {
	if !sf.EntropyEnabled {
		return nil
	}

	errors := []string{}
	if sf.EntropyThreshold <= 0 || sf.EntropyThreshold > 8.0 {
		errors = append(errors, "bouncer.entropy_threshold must be between 0 and 8.0")
	}
	if sf.EntropyMinToken < 8 {
		errors = append(errors, "bouncer.entropy_min_token must be >= 8")
	}
	return errors
}

func validateProviders(ctx context.Context, providers map[string]*Provider, logger *slog.Logger) []string {
	errors := []string{}
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
	return errors
}

var validationClient = &http.Client{Timeout: 30 * time.Second}

func validateProvider(ctx context.Context, name string, provider *Provider, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return validateWithMinimalRequest(provider, ctx, logger)
}

func validateWithMinimalRequest(provider *Provider, ctx context.Context, logger *slog.Logger) error {
	payload := `{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":false,"max_tokens":1}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if authErr := applyAuthHeader(req, provider); authErr != nil {
		return fmt.Errorf("failed to apply authentication: %w", authErr)
	}

	var resp *http.Response
	err = doWithRetry(ctx, 3, func() error {
		var doErr error
		resp, doErr = validationClient.Do(req)
		if doErr != nil {
			return doErr
		}
		if resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			return fmt.Errorf("upstream error: %d", resp.StatusCode)
		}
		return nil
	})
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

func validatePatternsToList(label string, patterns []string, logger *slog.Logger) []string {
	var errs []string
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			msg := fmt.Sprintf("%s[%d]: %v", label, i, err)
			errs = append(errs, msg)
			logger.Error("pattern compile failed", "label", label, "index", i, "pattern", p, "err", err)
		}
	}
	return errs
}

func ValidatePatterns(label string, patterns []string, logger *slog.Logger) error {
	errs := validatePatternsToList(label, patterns, logger)
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

func doWithRetry(ctx context.Context, maxAttempts int, fn func() error) error {
	if maxAttempts <= 1 {
		return fn()
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := fn(); err != nil {
			lastErr = err
			if attempt == maxAttempts-1 {
				return lastErr
			}
			backoff := calculateBackoff(attempt)
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
			continue
		}
		return nil
	}
	return lastErr
}

func calculateBackoff(attempt int) time.Duration {
	base := 500 * time.Millisecond
	max := 8 * time.Second
	delay := base
	for i := 0; i < attempt; i++ {
		delay *= 2
		if delay >= max {
			return max
		}
	}
	jitter := time.Duration(rand.Int63n(int64(delay / 10))) // 10% jitter
	return delay + jitter
}
