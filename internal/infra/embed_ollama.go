package infra

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"nenya/internal/util"
)

// OllamaEmbedder implements EmbeddingProvider via the Ollama /api/embed endpoint.
// Uses configurable model and URL with retry logic for transient failures.
type OllamaEmbedder struct {
	client *http.Client
	model  string
	url    string
}

// ollamaEmbedRequest is sent to the Ollama embedding API.
type ollamaEmbedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

// ollamaEmbedResponse is received from the Ollama embedding API.
type ollamaEmbedResponse struct {
	Embedding []float32 `json:"embedding"`
}

// NewOllamaEmbedder creates a new embedder with the given HTTP client, model name, and base URL.
func NewOllamaEmbedder(client *http.Client, model, url string) *OllamaEmbedder {
	return &OllamaEmbedder{
		client: client,
		model:  model,
		url:    url,
	}
}

// Embed sends text to the Ollama embedding endpoint and returns the embedding vector.
// Returns an error if the text exceeds 10000 characters, or if the upstream request fails.
// Uses DoWithRetry for transient network failures.
func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if len(text) > 10000 {
		return nil, fmt.Errorf("text too long for embedding: %d characters", len(text))
	}

	var result []float32
	err := util.DoWithRetry(ctx, 2, func() error {
		reqBody := ollamaEmbedRequest{
			Model:  o.model,
			Prompt: text,
		}
		payload, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("failed to marshal embed request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.url+"/api/embed", bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("failed to create embed request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := o.client.Do(req)
		if err != nil {
			return fmt.Errorf("failed to send embed request: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("embed request failed with status %d: %s", resp.StatusCode, string(body))
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return fmt.Errorf("failed to read embed response body: %w", err)
		}

		var ollamaResp ollamaEmbedResponse
		if err := json.Unmarshal(body, &ollamaResp); err != nil {
			return fmt.Errorf("failed to decode embed response: %w", err)
		}

		if len(ollamaResp.Embedding) == 0 {
			return fmt.Errorf("empty embedding returned")
		}

		result = ollamaResp.Embedding
		return nil
	})

	return result, err
}
