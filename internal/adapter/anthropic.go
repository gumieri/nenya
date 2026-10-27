package adapter

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

const (
	maxPreFlightBodyBytes     = 1 * 1024 * 1024
	thinkingTypeEnabled       = "enabled"
	thinkingDisplaySummarized = "summarized"
)

// CacheOpts groups cache control options for Anthropic adapter methods.
type CacheOpts struct {
	System      bool   // Enable system breakpoint caching
	Tools       bool   // Enable tools breakpoint caching
	Messages    bool   // Enable messages breakpoint caching
	Automatic   bool   // Enable top-level automatic caching mode (Anthropic automatic caching). When true, a single top-level cache_control is emitted instead of block-level markers.
	SystemTTL   string // TTL for system breakpoint ("ephemeral" or "1h")
	ToolsTTL    string // TTL for tools breakpoint
	MessagesTTL string // TTL for messages breakpoint
	GlobalTTL   string // Fallback TTL for all breakpoints (from CacheControlTTL)
	// MidConversationSystem overrides the model-capability probe for
	// keeping role=system messages in the messages array (Anthropic
	// 4.8+/5-family). nil lets the adapter decide via
	// discovery.SupportsMidConversationSystem; a non-nil value wins so
	// catalog/config-driven metadata can steer legacy hoisting either
	// way.
	MidConversationSystem *bool
	// TrailingContextMarkers lists content substrings identifying
	// gateway-injected, REQUEST-VARYING trailer messages (the egress
	// canary marker): they are never hoisted into the cache-marked system
	// block (that would change the cached prefix every request and miss
	// every breakpoint) and stay at the tail, unmarked, so the tripwire
	// still reaches the upstream (NENYA-171).
	TrailingContextMarkers []string
}

// getSystemTTL returns the effective TTL for the system breakpoint.
func (o *CacheOpts) getSystemTTL() string {
	if o.SystemTTL != "" {
		return o.SystemTTL
	}
	return o.GlobalTTL
}

// getToolsTTL returns the effective TTL for the tools breakpoint.
func (o *CacheOpts) getToolsTTL() string {
	if o.ToolsTTL != "" {
		return o.ToolsTTL
	}
	return o.GlobalTTL
}

// getMessagesTTL returns the effective TTL for the messages breakpoint.
func (o *CacheOpts) getMessagesTTL() string {
	if o.MessagesTTL != "" {
		return o.MessagesTTL
	}
	return o.GlobalTTL
}

// buildCacheControl constructs the cache_control map for Anthropic API.
// Returns {"type": "ephemeral"} for ephemeral TTL, or {"type": "ephemeral", "ttl": "1h"} for 1h TTL.
func buildCacheControl(ttl string) map[string]interface{} {
	cc := map[string]interface{}{"type": "ephemeral"}
	if ttl == "1h" {
		cc["ttl"] = "1h"
	}
	return cc
}

type AnthropicAdapter struct {
	version string
}

var (
	anthropicOnce    sync.Once
	anthropicAdapter *AnthropicAdapter
)

// GetAnthropicAdapter returns a singleton AnthropicAdapter instance.
func GetAnthropicAdapter() *AnthropicAdapter {
	anthropicOnce.Do(func() {
		anthropicAdapter = NewAnthropicAdapter()
	})
	return anthropicAdapter
}

// ConvertOpenAIToAnthropicBody converts an OpenAI-format request body
// (as a parsed map) to the Anthropic Messages API format. CacheOpts
// controls cache_control marker injection, per-breakpoint TTLs, and the
// request-varying trailer exclusions.
func (a *AnthropicAdapter) ConvertOpenAIToAnthropicBody(openai map[string]interface{}, model string, stream bool, opts CacheOpts) map[string]interface{} {
	return a.convertOpenAIToAnthropic(openai, model, stream, opts)
}

// ConvertAnthropicToOpenAIBody converts an Anthropic-format response
// (as a parsed map) to the OpenAI chat completions format.
func (a *AnthropicAdapter) ConvertAnthropicToOpenAIBody(anthropic map[string]interface{}, stream bool) map[string]interface{} {
	return a.convertAnthropicToOpenAI(anthropic, stream, nil)
}

// ConvertAnthropicToOpenAIBodyReported is ConvertAnthropicToOpenAIBody with a
// DropReport: content the OpenAI shape cannot represent (thinking-block
// signatures, redacted thinking, unknown block types) is recorded by kind on
// the report instead of being dropped silently (NENYA-176). Nil report
// behaves exactly like the unreported variant.
func (a *AnthropicAdapter) ConvertAnthropicToOpenAIBodyReported(anthropic map[string]interface{}, stream bool, report *DropReport) map[string]interface{} {
	return a.convertAnthropicToOpenAI(anthropic, stream, report)
}

// ConvertAnthropicRequestToOpenAIBody converts an Anthropic-format request body
// (as a parsed map) to the OpenAI chat completions format. This is the reverse
// of ConvertOpenAIToAnthropicBody and is used when clients send Anthropic-format
// requests to the gateway.
func (a *AnthropicAdapter) ConvertAnthropicRequestToOpenAIBody(anthropic map[string]interface{}) map[string]interface{} {
	return a.convertAnthropicToOpenAIRequest(anthropic, nil)
}

// ConvertAnthropicRequestToOpenAIBodyReported is ConvertAnthropicRequestToOpenAIBody
// with a DropReport for content the OpenAI shape cannot represent (NENYA-176).
func (a *AnthropicAdapter) ConvertAnthropicRequestToOpenAIBodyReported(anthropic map[string]interface{}, report *DropReport) map[string]interface{} {
	return a.convertAnthropicToOpenAIRequest(anthropic, report)
}

// ConvertOpenAIResponseToAnthropicBody converts an OpenAI-format response
// (as a parsed map) to the Anthropic Messages API format. This is the reverse
// of ConvertAnthropicToOpenAIBody and is used when the gateway needs to send
// Anthropic-format responses to clients that sent Anthropic-format requests.
func (a *AnthropicAdapter) ConvertOpenAIResponseToAnthropicBody(openai map[string]interface{}) map[string]interface{} {
	return a.convertOpenAIToAnthropicResponse(openai, nil)
}

// ConvertOpenAIResponseToAnthropicBodyReported is ConvertOpenAIResponseToAnthropicBody
// with fidelity reporting: kinds of content the translation could not
// represent (the "[unsendable content: <type>]" markers) are recorded in the
// DropReport so the gateway can surface them on the X-Nenya-Dropped-Content
// header and the unsendable-content metric (NENYA-176; the report plumbing
// is AUD-33 / NENYA-252).
func (a *AnthropicAdapter) ConvertOpenAIResponseToAnthropicBodyReported(openai map[string]interface{}, report *DropReport) map[string]interface{} {
	return a.convertOpenAIToAnthropicResponse(openai, report)
}

// NewAnthropicAdapter creates a new AnthropicAdapter instance.
func NewAnthropicAdapter() *AnthropicAdapter {
	return &AnthropicAdapter{
		version: "2023-06-01",
	}
}

// InjectAuth adds the Anthropic-specific authentication headers (x-api-key and anthropic-version).
func (a *AnthropicAdapter) InjectAuth(req *http.Request, apiKey string) error {
	if err := verifyAPIKey(apiKey, "anthropic"); err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Anthropic-Version", a.version)
	return nil
}

// MutateRequest converts an OpenAI-format request body to Anthropic format.
func (a *AnthropicAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	if len(body) > maxPreFlightBodyBytes {
		return nil, fmt.Errorf("anthropic: request body too large (%d bytes, max %d)", len(body), maxPreFlightBodyBytes)
	}

	var openai map[string]interface{}
	if err := json.Unmarshal(body, &openai); err != nil {
		slog.Debug("anthropic: request body is not valid JSON, passing through", "error", err)
		return body, nil
	}

	opts := CacheOpts{
		GlobalTTL: "ephemeral",
	}
	anthropic := a.convertOpenAIToAnthropic(openai, model, stream, opts)

	out, err := json.Marshal(anthropic)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted request: %w", err)
	}
	return out, nil
}

// MutateResponse converts an Anthropic-format response to OpenAI format.
func (a *AnthropicAdapter) MutateResponse(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var anthropic map[string]interface{}
	if err := json.Unmarshal(body, &anthropic); err != nil {
		slog.Debug("anthropic: response body is not valid JSON, passing through", "error", err)
		return body, nil
	}

	openai := a.convertAnthropicToOpenAI(anthropic, true, nil)

	out, err := json.Marshal(openai)
	if err != nil {
		return body, fmt.Errorf("anthropic: failed to marshal converted response: %w", err)
	}
	return out, nil
}

// NormalizeError classifies Anthropic HTTP errors into retryable, rate-limited, quota-exhausted, or permanent.
func (a *AnthropicAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	switch statusCode {
	case 429:
		return ErrorRateLimited
	case 500, 502, 503, 504:
		return ErrorRetryable
	case 400:
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "overloaded") || strings.Contains(lower, "rate") {
			return ErrorRetryable
		}
		return ErrorPermanent
	case 529:
		return ErrorRateLimited
	default:
		return ErrorPermanent
	}
}
