package discovery

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"unicode"
)

type Parser interface {
	Parse(body []byte, provider string) ([]DiscoveredModel, error)
}

type OpenAIModelsResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

type AnthropicModelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"data"`
}

type GeminiModelsResponse struct {
	Models []struct {
		Name             string `json:"name"`
		InputTokenLimit  int    `json:"inputTokenLimit"`
		OutputTokenLimit int    `json:"outputTokenLimit"`
	} `json:"models"`
}

type OllamaModelsResponse struct {
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
}

type OpenAIParser struct{}

func (p *OpenAIParser) Parse(body []byte, provider string) ([]DiscoveredModel, error) {
	var resp OpenAIModelsResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&resp); err != nil {
		models, fallbackErr := parsePlainArrayModels(body, provider)
		if fallbackErr == nil {
			return models, nil
		}
		models, fallbackErr = parseObjectArrayModels(body, provider)
		if fallbackErr == nil {
			return models, nil
		}
		return nil, fmt.Errorf("failed to parse OpenAI models response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID == "" {
			continue
		}
		if !isValidModelID(m.ID) {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:       m.ID,
			Provider: provider,
		})
	}
	return models, nil
}

func parsePlainArrayModels(body []byte, provider string) ([]DiscoveredModel, error) {
	var plainArray []string
	if err := json.Unmarshal(body, &plainArray); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(plainArray))
	for _, id := range plainArray {
		if id == "" {
			continue
		}
		if !isValidModelID(id) {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:       id,
			Provider: provider,
		})
	}
	return models, nil
}

func parseObjectArrayModels(body []byte, provider string) ([]DiscoveredModel, error) {
	var objectArray []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &objectArray); err != nil {
		return nil, err
	}
	models := make([]DiscoveredModel, 0, len(objectArray))
	for _, m := range objectArray {
		if m.ID == "" {
			continue
		}
		if !isValidModelID(m.ID) {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:       m.ID,
			Provider: provider,
		})
	}
	return models, nil
}

type AnthropicParser struct{}

func (p *AnthropicParser) Parse(body []byte, provider string) ([]DiscoveredModel, error) {
	var resp AnthropicModelsResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to parse Anthropic models response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(resp.Data))
	for _, m := range resp.Data {
		if m.ID == "" {
			continue
		}
		if !isValidModelID(m.ID) {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:       m.ID,
			Provider: provider,
			OwnedBy:  "anthropic",
		})
	}
	return models, nil
}

type GeminiParser struct{}

func (p *GeminiParser) Parse(body []byte, provider string) ([]DiscoveredModel, error) {
	var resp GeminiModelsResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to parse Gemini models response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(resp.Models))
	for _, m := range resp.Models {
		if m.Name == "" {
			continue
		}
		modelID := strings.TrimPrefix(m.Name, "models/")
		if modelID == "" {
			continue
		}
		if !isValidModelID(modelID) {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:         modelID,
			Provider:   provider,
			MaxContext: m.InputTokenLimit,
			MaxOutput:  m.OutputTokenLimit,
			OwnedBy:    "google",
		})
	}
	return models, nil
}

type OllamaParser struct{}

func (p *OllamaParser) Parse(body []byte, provider string) ([]DiscoveredModel, error) {
	var resp OllamaModelsResponse
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&resp); err != nil {
		return nil, fmt.Errorf("failed to parse Ollama models response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(resp.Models))
	for _, m := range resp.Models {
		if m.Name == "" {
			continue
		}
		if !isValidModelID(m.Name) {
			continue
		}
		models = append(models, DiscoveredModel{
			ID:       m.Name,
			Provider: provider,
			OwnedBy:  "ollama",
		})
	}
	return models, nil
}

func GetParser(provider string) Parser {
	switch strings.ToLower(provider) {
	case "anthropic":
		return &AnthropicParser{}
	case "gemini":
		return &GeminiParser{}
	case "ollama":
		return &OllamaParser{}
	default:
		return &OpenAIParser{}
	}
}

func GetModelsEndpoint(providerURL, provider string) string {
	if providerURL == "" {
		return ""
	}
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}

	switch strings.ToLower(provider) {
	case "anthropic":
		return "https://api.anthropic.com/v1/models"
	case "gemini":
		if strings.Contains(strings.ToLower(u.Host), "generativelanguage.googleapis.com") {
			if idx := strings.Index(u.Path, "/openai/chat/completions"); idx != -1 {
				return strings.TrimSuffix(providerURL, "/openai/chat/completions") + "/models"
			}
			return "https://generativelanguage.googleapis.com/v1beta/models"
		}
	case "ollama":
		return u.Scheme + "://" + u.Host + "/api/tags"
	}

	if strings.HasSuffix(u.Path, "/chat/completions") {
		return providerURL[:len(providerURL)-len("/chat/completions")] + "/models"
	}
	return ""
}

func ParseModelsResponse(body []byte, provider string, logger *slog.Logger) ([]DiscoveredModel, error) {
	parser := GetParser(provider)
	models, err := parser.Parse(body, provider)
	if err != nil {
		logger.Debug("failed to parse models response", "provider", provider, "err", err)
		return nil, err
	}
	for i := range models {
		if models[i].Metadata == nil {
			if caps := InferCapabilities(models[i].ID); caps != nil {
				models[i].Metadata = caps
			}
		}
	}
	logger.Debug("parsed models from provider", "provider", provider, "count", len(models))
	return models, nil
}

func isValidModelID(id string) bool {
	if id == "" {
		return false
	}
	if len(id) > 256 {
		return false
	}
	for _, r := range id {
		if !unicode.IsPrint(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}
