package providers

import (
	"net/url"
	"strings"
)

// deepseekSpec returns the provider specification for DeepSeek.
//
// DeepSeek v4 models (deepseek-v4-pro, deepseek-v4-flash) support thinking
// mode, where the model outputs chain-of-thought reasoning via the
// reasoning_content field alongside the final content.
//
// Key behaviors:
//   - deepseek-v4-pro and deepseek-v4-flash: thinking mode is ON by default
//   - To disable thinking, clients must send {"thinking": {"type": "disabled"}}
//   - When thinking mode is active, temperature/top_p/presence_penalty/frequency_penalty
//     are silently ignored by the API
//   - reasoning_content from assistant messages MUST be passed back verbatim in
//     multi-turn conversations when tool calls were performed
//   - reasoning_content from non-tool-call turns is optional (API ignores it)
func deepseekSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: deepseekValidationEndpoint,
	}
}

func deepseekValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)

	if strings.Contains(host, "api.deepseek.com") {
		return "https://api.deepseek.com/models"
	}
	return defaultValidationEndpoint(providerURL, u.Path)
}
