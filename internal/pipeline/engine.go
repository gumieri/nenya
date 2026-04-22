package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"nenya/internal/config"
)

const (
	MaxOllamaResponseBytes = 512 * 1024
	MaxErrorBodyBytes      = 8 * 1024
)

func CallEngine(ctx context.Context, httpClient *http.Client, provider *config.Provider, engine config.EngineConfig, injectAPIKey func(providerName string, headers http.Header) error, systemPrompt, prompt string) (string, error) {
	apiFormat := provider.ApiFormat
	if apiFormat == "" {
		apiFormat = "openai"
	}

	var payload map[string]interface{}
	switch apiFormat {
	case "ollama":
		payload = map[string]interface{}{
			"model":  engine.Model,
			"system": systemPrompt,
			"prompt": prompt,
			"stream": false,
		}
	default:
		payload = map[string]interface{}{
			"model": engine.Model,
			"messages": []map[string]string{
				{"role": "system", "content": systemPrompt},
				{"role": "user", "content": prompt},
			},
			"stream": false,
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("failed to marshal engine payload: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.URL, bytes.NewBuffer(encoded))
	if err != nil {
		return "", fmt.Errorf("failed to create engine request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	if err = injectAPIKey(engine.Provider, req.Header); err != nil {
		return "", fmt.Errorf("engine auth failed: %v", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("engine unreachable: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, MaxErrorBodyBytes))
		return "", fmt.Errorf("engine returned status %d: %s", resp.StatusCode, string(body))
	}

	var response map[string]interface{}
	if err := json.NewDecoder(io.LimitReader(resp.Body, MaxOllamaResponseBytes)).Decode(&response); err != nil {
		return "", fmt.Errorf("failed to decode engine response: %v", err)
	}

	var output string
	switch apiFormat {
	case "ollama":
		var ok bool
		output, ok = response["response"].(string)
		if !ok {
			return "", fmt.Errorf("engine response missing 'response' field")
		}
	default:
		if choices, ok := response["choices"].([]interface{}); ok && len(choices) > 0 {
			if choice, ok := choices[0].(map[string]interface{}); ok {
				if msg, ok := choice["message"].(map[string]interface{}); ok {
					if content, ok := msg["content"].(string); ok {
						output = content
					}
				}
			}
		}
		if output == "" {
			return "", fmt.Errorf("openai response missing content")
		}
	}
	return output, nil
}

func CallEngineChain(ctx context.Context, httpClient, ollamaClient *http.Client,
	targets []config.EngineTarget, logger *slog.Logger,
	injectAPIKey func(providerName string, headers http.Header) error,
	caller, agentName, systemPrompt, prompt string) (string, error) {
	if len(targets) == 0 {
		return "", fmt.Errorf("engine chain: no targets available")
	}

	var lastErr error
	for i, target := range targets {
		attempt := i + 1
		total := len(targets)

		logger.Info("engine call attempt",
			"caller", caller,
			"agent", agentName,
			"provider", target.Provider.Name,
			"model", target.Engine.Model,
			"attempt", attempt,
			"total", total)

		client := httpClient
		if target.Provider.ApiFormat == "ollama" {
			client = ollamaClient
		}

		timeout := target.Engine.TimeoutSeconds
		if timeout <= 0 {
			timeout = 60
		}
		engineCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		result, err := CallEngine(engineCtx, client, target.Provider, target.Engine, injectAPIKey, systemPrompt, prompt)
		cancel()

		if err != nil {
			lastErr = err
			logger.Warn("engine call failed",
				"caller", caller,
				"agent", agentName,
				"provider", target.Provider.Name,
				"model", target.Engine.Model,
				"attempt", attempt,
				"total", total,
				"err", err)
			continue
		}

		logger.Info("engine call success",
			"caller", caller,
			"agent", agentName,
			"provider", target.Provider.Name,
			"model", target.Engine.Model,
			"attempt", attempt,
			"total", total)
		return result, nil
	}

	return "", fmt.Errorf("engine chain failed after %d attempts: last error: %w", len(targets), lastErr)
}
