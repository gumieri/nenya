package providers

import (
	"log/slog"
	"net/url"
	"strings"

	"nenya/internal/infra"
	"nenya/internal/stream"
)

// SanitizeDeps provides the dependencies needed by provider-specific
// request sanitization functions.
type SanitizeDeps struct {
	Logger             *slog.Logger
	ThoughtSigCache    *infra.ThoughtSignatureCache
	ExtractContentText func(msg map[string]interface{}) string
	SupportsReasoning  func(model string) bool
	ProviderThinking   func(name string) (enabled bool, clearThinking bool, ok bool)
}

// ProviderSpec describes a provider's capabilities and optional hooks
// for request sanitization and response transformation.
type ProviderSpec struct {
	SupportsStreamOptions  bool
	SupportsAutoToolChoice bool
	SupportsContentArrays  bool
	SupportsToolCalls      bool
	SupportsReasoning      bool
	SupportsVision         bool
	ModelMap               map[string]string
	SanitizeRequest        func(deps *SanitizeDeps, payload map[string]interface{})
	NewResponseTransformer func(cache *infra.ThoughtSignatureCache) stream.ResponseTransformer
	ValidationEndpoint     func(providerURL string) string
}

// SupportsToolCalls reports whether the named provider supports tool calls.
func SupportsToolCalls(name string) bool {
	if spec, ok := Get(name); ok {
		return spec.SupportsToolCalls
	}
	return true
}

// SupportsReasoning reports whether the named provider supports reasoning/thinking.
func SupportsReasoning(name string) bool {
	if spec, ok := Get(name); ok {
		return spec.SupportsReasoning
	}
	return false
}

// SupportsVision reports whether the named provider supports vision/image inputs.
func SupportsVision(name string) bool {
	if spec, ok := Get(name); ok {
		return spec.SupportsVision
	}
	return false
}

// Registry maps provider format names to their ProviderSpec definitions.
// Built-in specs are registered at init time.
var Registry = map[string]ProviderSpec{}

func init() {
	Registry["gemini"] = geminiSpec()
	Registry["zai"] = zaiSpec()
	Registry["groq"] = groqSpec()
	Registry["deepseek"] = deepseekSpec()
	Registry["together"] = togetherSpec()
	Registry["openai"] = openaiSpec()
	Registry["github"] = githubSpec()
	Registry["openrouter"] = openrouterSpec()
	Registry["sambanova"] = sambanovaSpec()
	Registry["cerebras"] = cerebrasSpec()
	Registry["nvidia"] = nvidiaSpec()
	Registry["nvidia_free"] = nvidiaFreeSpec()
	Registry["qwen_free"] = qwenFreeSpec()
	Registry["minimax_free"] = minimaxFreeSpec()
	Registry["zai-coding-plan"] = zaiCodingPlanSpec()
	Registry["ollama"] = ollamaSpec()
	Registry["anthropic"] = anthropicSpec()
	Registry["mistral"] = mistralSpec()
	Registry["xai"] = xaiSpec()
	Registry["azure"] = azureSpec()
	Registry["perplexity"] = perplexitySpec()
	Registry["cohere"] = cohereSpec()
	Registry["deepinfra"] = deepinfraSpec()
	Registry["zen"] = zenSpec()
}

func Get(name string) (ProviderSpec, bool) {
	spec, ok := Registry[strings.ToLower(name)]
	return spec, ok
}

func SupportsStreamOptions(name string) bool {
	if spec, ok := Get(name); ok {
		return spec.SupportsStreamOptions
	}
	return false
}

func SupportsAutoToolChoice(name string) bool {
	if spec, ok := Get(name); ok {
		return spec.SupportsAutoToolChoice
	}
	return false
}

func SupportsContentArrays(name string) bool {
	if spec, ok := Get(name); ok {
		return spec.SupportsContentArrays
	}
	return false
}

func defaultValidationEndpoint(host string, path string) string {
	u, err := url.Parse(host)
	if err != nil {
		return ""
	}
	lowerHost := strings.ToLower(u.Host)
	lowerPath := u.Path

	switch {
	case strings.Contains(lowerHost, "127.0.0.1:11434") || strings.Contains(lowerHost, "localhost:11434"):
		return ""
	}

	if strings.HasSuffix(lowerPath, "/chat/completions") {
		return host[:len(host)-len("/chat/completions")] + "/models"
	}
	return ""
}
