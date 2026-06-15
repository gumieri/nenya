package providers

import (
	"log/slog"
	"net/url"
	"strings"

	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
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
	ServiceKinds           []ServiceKind
	ModelMap               map[string]string
	SanitizeRequest        func(deps *SanitizeDeps, payload map[string]interface{})
	NewResponseTransformer func(cache *infra.ThoughtSignatureCache) stream.ResponseTransformer
	ValidationEndpoint     func(providerURL string) string
}

// Registry maps provider format names to their ProviderSpec definitions.
// Built-in specs are registered at init time.
var Registry = map[string]ProviderSpec{}

func init() {
	Registry["anthropic"] = anthropicSpec()
	Registry["azure"] = azureSpec()
	Registry["cerebras"] = cerebrasSpec()
	Registry["cohere"] = cohereSpec()
	Registry["deepinfra"] = deepinfraSpec()
	Registry["deepseek"] = deepseekSpec()
	Registry["github"] = githubSpec()
	Registry["gemini"] = geminiSpec()
	Registry["groq"] = groqSpec()
	Registry["minimax_free"] = minimaxFreeSpec()
	Registry["mistral"] = mistralSpec()
	Registry["moonshot"] = moonshotSpec()
	Registry["nvidia"] = nvidiaSpec()
	Registry["nvidia_free"] = nvidiaFreeSpec()
	Registry["ollama"] = ollamaSpec()
	Registry["openai"] = openaiSpec()
	Registry["openrouter"] = openrouterSpec()
	Registry["perplexity"] = perplexitySpec()
	Registry["qwen_free"] = qwenFreeSpec()
	Registry["sambanova"] = sambanovaSpec()
	Registry["together"] = togetherSpec()
	Registry["xai"] = xaiSpec()
	Registry["zen"] = zenSpec()
	Registry["zai"] = zaiSpec()
	Registry["zai-coding-plan"] = zaiCodingPlanSpec()
}

func Get(name string) (ProviderSpec, bool) {
	spec, ok := Registry[strings.ToLower(name)]
	return spec, ok
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
