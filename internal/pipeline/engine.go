package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

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
	defer resp.Body.Close()

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
