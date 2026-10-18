package util

import (
	"math"
	"strings"

	"github.com/nenya/config"
)

// SafeMicroUSD converts a USD amount to signed micro-USD, reporting ok=false
// for NaN, Inf, or magnitudes that would overflow the int64 micro-USD range
// (|usd| beyond MaxInt64/1e6). Use at every cost-recording boundary so a
// hostile or buggy upstream usage payload cannot saturate the conversion.
func SafeMicroUSD(usd float64) (micro int64, ok bool) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd >= math.MaxInt64/1e6 || usd <= -math.MaxInt64/1e6 {
		return 0, false
	}
	return int64(math.Round(usd * 1e6)), true
}

// SafeUSDCents converts a USD amount to integer cents, reporting ok=false for
// NaN, Inf, or magnitudes that would overflow the int64 cents range
// (|usd| beyond MaxInt64/100). Same contract as SafeMicroUSD at cent
// resolution.
func SafeUSDCents(usd float64) (cents int64, ok bool) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd >= math.MaxInt64/100 || usd <= -math.MaxInt64/100 {
		return 0, false
	}
	return int64(math.Round(usd * 100)), true
}

// AddCap returns a+b, clamped to math.MaxInt on overflow.
// Use this for slice capacity calculations where a+b could exceed
// maximum int value. Panics on negative inputs (invalid capacity).
func AddCap(a, b int) int {
	if b < 0 || a < 0 {
		return a + b
	}
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// DeriveInputTokenBudget returns the input-conversation token budget for a
// model whose context window is maxContext, reserving room for up to maxOutput
// generated tokens. It is min(maxContext/4*3, maxContext-maxOutput) when
// maxOutput is positive and smaller than maxContext, and maxContext/4*3
// otherwise (unknown, non-positive, or misconfigured output cap). maxContext/4*3
// cannot overflow for any positive int because the division precedes the
// multiply; the subtraction is guarded by maxOutput < maxContext so the result
// is never negative. Degenerate windows under 4 tokens yield 0, disabling the
// trim. Returns 0 when maxContext is not positive.
func DeriveInputTokenBudget(maxContext, maxOutput int) int {
	if maxContext <= 0 {
		return 0
	}
	budget := maxContext / 4 * 3
	if maxOutput > 0 && maxOutput < maxContext {
		if reserved := maxContext - maxOutput; reserved < budget {
			budget = reserved
		}
	}
	return budget
}

// usableCap reports whether a token-cap field carries a usable number.
func usableCap(raw interface{}) bool {
	switch v := raw.(type) {
	case float64:
		return !math.IsNaN(v) && v >= 0
	case int:
		return v >= 0
	case int64:
		return v >= 0
	default:
		return false
	}
}

// EffectiveOutputTokens returns the output-token reservation to use for a
// request: the smaller of the client-supplied token cap and the model's
// declared output cap, so a client that asks for little output does not make
// the input budget over-reserve. declared is the model/output cap (0 when
// unknown). The declared cap is returned when the payload carries no usable
// cap. Both spellings of the cap are read (NENYA-179): a body carrying only
// the newer max_completion_tokens must still reduce the reservation, or a
// target-aware rename would silently change how much input the request keeps.
func EffectiveOutputTokens(payload map[string]interface{}, declared int) int {
	raw := payload["max_tokens"]
	if !usableCap(raw) {
		// A null or non-numeric canonical field falls through to the newer
		// spelling rather than discarding a usable cap (NENYA-179 review).
		raw = payload["max_completion_tokens"]
	}
	if !usableCap(raw) {
		return declared
	}
	var requested int
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || v < 0 || v >= float64(math.MaxInt) {
			return declared
		}
		requested = int(v)
	case int:
		requested = v
	case int64:
		if v > int64(math.MaxInt) {
			return declared
		}
		requested = int(v)
	default:
		return declared
	}
	if requested < 0 {
		return declared
	}
	if declared > 0 && declared < requested {
		return declared
	}
	return requested
}

// JoinBackticks formats a slice of names as a comma-separated list
// wrapped in backticks. For example, ["foo", "bar"] becomes "`foo`, `bar`".
func JoinBackticks(names []string) string {
	var sb strings.Builder
	for i, name := range names {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteByte('`')
		sb.WriteString(name)
		sb.WriteByte('`')
	}
	return sb.String()
}

// ErrNoProvider is the error message returned when no provider can be
// resolved for a given model name.
const ErrNoProvider = "No provider configured for this model"

// ErrNonChatModel is the error message returned when a model is configured
// as a non-chat model (e.g. a TypeSafe Jev System One decision model) and a
// chat-completions request names it. Such models are reached through the
// /proxy/ passthrough or a dedicated decision endpoint, not chat routing.
const ErrNonChatModel = "Model is not a chat model; use the decision endpoint or /proxy/ passthrough"

// ProviderCanServe returns true if the provider is configured with either
// an API key or auth_style "none" (i.e. can actually make upstream requests).
func ProviderCanServe(p *config.Provider) bool {
	return p != nil && (p.APIKey != "" || p.AuthStyle == config.AuthStyleNone)
}

// FindRegistryModels returns models from ModelRegistry matching the given pattern.
// The pattern can be a literal model string, a regex pattern (via ModelRgx/ProviderRgx),
// or a provider-only entry. Only models whose providers are configured and
// able to serve (have API key or auth_style "none") are returned.
func FindRegistryModels(pattern config.AgentModel, providers map[string]*config.Provider) []config.AgentModel {
	var models []config.AgentModel
	for modelID, entry := range config.ModelRegistry {
		if !pattern.MatchesCatalog(entry.Provider, modelID) {
			continue
		}
		if providers != nil {
			if p, ok := providers[entry.Provider]; !ok || !ProviderCanServe(p) || !p.AllowsModel(modelID) {
				continue
			}
			models = append(models, config.AgentModel{
				Provider:        entry.Provider,
				Model:           modelID,
				MaxContext:      entry.MaxContext,
				MaxOutput:       entry.MaxOutput,
				ReasoningEffort: pattern.ReasoningEffort,
			})
		}
	}
	return models
}
