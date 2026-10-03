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

// PricingEntry is the effective pricing for a model in the discovery catalog.
//
// InputCostPer1M/OutputCostPer1M are the standard (off-peak) baseline rates.
// PeakInputCostPer1M/PeakOutputCostPer1M are the peak-window rates; zero means
// no peak window. CachedInputCostPer1M is the discounted cache-read input rate;
// zero means cache reads bill at the window input rate. See docs/COST_MODEL.md.
type PricingEntry struct {
	InputCostPer1M       float64 `json:"input_cost_per_1m"`
	OutputCostPer1M      float64 `json:"output_cost_per_1m"`
	PeakInputCostPer1M   float64 `json:"peak_input_cost_per_1m,omitempty"`
	PeakOutputCostPer1M  float64 `json:"peak_output_cost_per_1m,omitempty"`
	CachedInputCostPer1M float64 `json:"cached_input_cost_per_1m,omitempty"`
	Currency             string  `json:"currency"`
}

// IsZero reports whether no rate is set (a peak-only/cached-only entry is not
// zero).
func (p PricingEntry) IsZero() bool {
	return p.InputCostPer1M == 0 && p.OutputCostPer1M == 0 &&
		p.PeakInputCostPer1M == 0 && p.PeakOutputCostPer1M == 0 &&
		p.CachedInputCostPer1M == 0
}

// HasPeak reports whether the entry declares a peak-window rate.
func (p PricingEntry) HasPeak() bool {
	return p.PeakInputCostPer1M != 0 || p.PeakOutputCostPer1M != 0
}

// HasStandardRate reports whether a usable standard (off-peak) baseline rate is
// set. Peak-only entries are non-zero but have no standard rate, so callers
// that price with the standard pair (Peak unset) must gate on this —
// otherwise a peak-only entry would silently price at $0.
func (p PricingEntry) HasStandardRate() bool {
	return p.InputCostPer1M != 0 || p.OutputCostPer1M != 0
}

// PricingUsage describes the token counts and rate window for one cost
// computation. Peak selects the peak pair when the entry declares one;
// CachedInput is the subset of Input billed at the cached-input rate.
type PricingUsage struct {
	Input       int64
	CachedInput int64
	Output      int64
	Peak        bool
}

// CalculateCost estimates the USD cost of a request. The window (usage.Peak)
// selects the peak pair when the entry declares one — per dimension, so a
// half-declared peak pair prices the missing side at the standard rate rather
// than $0. CachedInput is billed at CachedInputCostPer1M when set, else at the
// window input rate; the remaining Input is billed at the window input rate;
// Output at the window output rate. Token counts are clamped non-negative and
// CachedInput is clamped to Input; rates are clamped non-negative (a NaN rate
// from an unvalidated feed contributes nothing). See docs/COST_MODEL.md.
func (p PricingEntry) CalculateCost(usage PricingUsage) float64 {
	inRate, outRate := p.InputCostPer1M, p.OutputCostPer1M
	if usage.Peak && p.HasPeak() {
		if p.PeakInputCostPer1M > 0 {
			inRate = p.PeakInputCostPer1M
		}
		if p.PeakOutputCostPer1M > 0 {
			outRate = p.PeakOutputCostPer1M
		}
	}
	cachedRate := p.CachedInputCostPer1M
	if cachedRate == 0 {
		cachedRate = inRate
	}
	// Non-positive → 0 (NaN fails the > test, so it clamps too).
	if !(inRate > 0) {
		inRate = 0
	}
	if !(outRate > 0) {
		outRate = 0
	}
	if !(cachedRate > 0) {
		cachedRate = 0
	}
	input := usage.Input
	if input < 0 {
		input = 0
	}
	cached := usage.CachedInput
	if cached < 0 {
		cached = 0
	}
	if cached > input {
		cached = input
	}
	output := usage.Output
	if output < 0 {
		output = 0
	}
	uncached := input - cached
	return (float64(cached)*cachedRate +
		float64(uncached)*inRate +
		float64(output)*outRate) / 1_000_000
}

// PricingSource is an interface for pricing data sources. It is reserved for future
// use in implementing a pricing chain (e.g., StaticPricing → FallbackPricing) for
// more flexible pricing resolution. Currently not integrated into the main discovery
// flow.
type PricingSource interface {
	GetPricing(ctx context.Context, modelID string) (PricingEntry, bool)
}

// StaticPricing implements PricingSource by looking up pricing from a static map.
// Reserved for future use in pricing chain implementation.
type StaticPricing struct {
	pricing map[string]PricingEntry
}

func NewStaticPricing(pricing map[string]PricingEntry) *StaticPricing {
	return &StaticPricing{pricing: pricing}
}

func (sp *StaticPricing) GetPricing(ctx context.Context, modelID string) (PricingEntry, bool) {
	if sp.pricing == nil {
		return PricingEntry{}, false
	}
	p, ok := sp.pricing[modelID]
	return p, ok
}

// FallbackPricing implements PricingSource by returning a fixed high-cost estimate.
// Reserved for future use in pricing chain implementation when no other pricing source
// is available.
type FallbackPricing struct {
	InputCostPer1M  float64
	OutputCostPer1M float64
}

func NewFallbackPricing(inputCost, outputCost float64) *FallbackPricing {
	return &FallbackPricing{
		InputCostPer1M:  inputCost,
		OutputCostPer1M: outputCost,
	}
}

func (fp *FallbackPricing) GetPricing(ctx context.Context, modelID string) (PricingEntry, bool) {
	return PricingEntry{
		InputCostPer1M:  fp.InputCostPer1M,
		OutputCostPer1M: fp.OutputCostPer1M,
		Currency:        "USD",
	}, true
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
	var resp *http.Response
	var fetchErr error

	maxAttempts := 3
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, pf.baseURL+"/models", nil)
		if err != nil {
			return nil, fmt.Errorf("creating request: %w", err)
		}

		resp, fetchErr = pf.client.Do(req)
		if fetchErr == nil {
			break
		}

		if attempt < maxAttempts {
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return nil, fmt.Errorf("fetch canceled: %w", ctx.Err())
			}
		}
	}

	if fetchErr != nil {
		return nil, fmt.Errorf("fetching models: %w", fetchErr)
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

// MergePricing overlays discovered pricing onto the static map: a static entry
// is used only when the model is absent from the discovered set or the
// discovered entry is entirely zero. IsZero covers peak/cached fields, so a
// discovered entry carrying only those still wins over the static baseline.
//
// Reserved for a future pricing chain (see PricingSource); not wired into the
// discovery flow today.
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
