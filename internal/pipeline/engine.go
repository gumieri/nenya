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

	"nenya/config"
)

// MaxOllamaResponseBytes is the maximum response size accepted from the
// local engine (Ollama) to prevent memory exhaustion.
const MaxOllamaResponseBytes = 512 * 1024

// MaxErrorBodyBytes is the maximum number of bytes read from upstream
// error response bodies for logging/classification.
const MaxErrorBodyBytes = 8 * 1024

// CallEngine sends a prompt to the local engine (e.g. Ollama) for
// summarization or redaction. It handles both OpenAI and Ollama API formats.
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
	if decodeErr := json.NewDecoder(io.LimitReader(resp.Body, MaxOllamaResponseBytes)).Decode(&response); decodeErr != nil {
		return "", fmt.Errorf("failed to decode engine response: %v", decodeErr)
	}

	output, err := extractEngineOutput(response, apiFormat)
	if err != nil {
		return "", err
	}
	return output, nil
}

func extractEngineOutput(response map[string]interface{}, apiFormat string) (string, error) {
	switch apiFormat {
	case "ollama":
		return extractOllamaOutput(response)
	default:
		return extractOpenAIOutput(response)
	}
}

func extractTextFromParts(parts []interface{}) (string, bool) {
	for _, part := range parts {
		p, ok := part.(map[string]interface{})
		if !ok || p["type"] != "text" {
			continue
		}
		if text, ok := p["text"].(string); ok {
			return text, true
		}
	}
	return "", false
}

func extractOllamaOutput(response map[string]interface{}) (string, error) {
	output, ok := response["response"].(string)
	if !ok {
		return "", fmt.Errorf("engine response missing 'response' field")
	}
	return output, nil
}

func extractOpenAIOutput(response map[string]interface{}) (string, error) {
	choices, ok := response["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		return "", fmt.Errorf("openai response missing choices")
	}

	choice, ok := choices[0].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("openai choice is not an object")
	}

	msg, ok := choice["message"].(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("openai choice missing message")
	}

	// Handle string content (standard OpenAI format).
	if content, ok := msg["content"].(string); ok {
		return content, nil
	}

	// Handle array content (Anthropic-style or multimodal).
	if parts, ok := msg["content"].([]interface{}); ok {
		if text, found := extractTextFromParts(parts); found {
			return text, nil
		}
	}

	return "", fmt.Errorf("openai message missing content")
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
