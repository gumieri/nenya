package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

type PricingEntry struct {
	InputCostPer1M  float64 `json:"input_cost_per_1m"`
	OutputCostPer1M float64 `json:"output_cost_per_1m"`
	Currency        string  `json:"currency"`
}

func (p PricingEntry) IsZero() bool {
	return p.InputCostPer1M == 0 && p.OutputCostPer1M == 0
}

func (p PricingEntry) CalculateCost(inputTokens, outputTokens int64) float64 {
	inputCost := (float64(inputTokens) / 1_000_000) * p.InputCostPer1M
	outputCost := (float64(outputTokens) / 1_000_000) * p.OutputCostPer1M
	return inputCost + outputCost
}

type openRouterPricing struct {
	Prompt     string `json:"prompt"`
	Completion string `json:"completion"`
}

type openRouterModel struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Pricing openRouterPricing `json:"pricing"`
}

type openRouterModelsResponse struct {
	Data []openRouterModel `json:"data"`
}

type PricingFetcher struct {
	client  *http.Client
	baseURL string
	logger  *slog.Logger
}

func NewPricingFetcher(logger *slog.Logger) *PricingFetcher {
	return &PricingFetcher{
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
		baseURL: "https://openrouter.ai/api/v1",
		logger:  logger,
	}
}

func (pf *PricingFetcher) FetchOpenRouterPricing(ctx context.Context) (map[string]PricingEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pf.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := pf.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	var modelsResp openRouterModelsResponse
	if err := json.Unmarshal(body, &modelsResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	pricing := make(map[string]PricingEntry, len(modelsResp.Data))
	for _, m := range modelsResp.Data {
		var inputCost, outputCost float64
		if _, err := fmt.Sscanf(m.Pricing.Prompt, "%f", &inputCost); err != nil {
			continue
		}
		if _, err := fmt.Sscanf(m.Pricing.Completion, "%f", &outputCost); err != nil {
			continue
		}

		inputPer1M := inputCost * 1_000_000
		outputPer1M := outputCost * 1_000_000

		pricing[m.ID] = PricingEntry{
			InputCostPer1M:  inputPer1M,
			OutputCostPer1M: outputPer1M,
			Currency:        "USD",
		}
	}

	pf.logger.Debug("fetched openrouter pricing", "models", len(pricing))
	return pricing, nil
}

func MergePricing(discovered map[string]PricingEntry, static map[string]PricingEntry) map[string]PricingEntry {
	merged := make(map[string]PricingEntry, len(discovered)+len(static))
	for k, v := range discovered {
		merged[k] = v
	}
	for k, v := range static {
		if existing, ok := merged[k]; !ok || existing.IsZero() {
			merged[k] = v
		}
	}
	return merged
}
