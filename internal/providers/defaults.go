package providers

import (
	"net/url"
	"strings"
)

func togetherSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      false,
		SupportsVision:         true,
		ValidationEndpoint:     togetherValidationEndpoint,
	}
}

func togetherValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)

	if strings.Contains(host, "api.together.xyz") {
		return "https://api.together.xyz/v1/models"
	}
	return defaultValidationEndpoint(providerURL, u.Path)
}

func openaiSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     openaiValidationEndpoint,
	}
}

func openaiValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)

	if strings.Contains(host, "api.openai.com") {
		return "https://api.openai.com/v1/models"
	}
	return defaultValidationEndpoint(providerURL, u.Path)
}

func githubSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     githubValidationEndpoint,
	}
}

func githubValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func openrouterSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     openrouterValidationEndpoint,
	}
}

func openrouterValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func sambanovaSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     sambanovaValidationEndpoint,
	}
}

func sambanovaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func cerebrasSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         false,
		ValidationEndpoint:     cerebrasValidationEndpoint,
	}
}

func cerebrasValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func nvidiaSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      false,
		SupportsVision:         true,
		ValidationEndpoint:     nvidiaValidationEndpoint,
	}
}

func nvidiaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func nvidiaFreeSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsVision:         false,
		ValidationEndpoint:     nvidiaValidationEndpoint,
	}
}

func qwenFreeSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsVision:         false,
		ValidationEndpoint:     qwenFreeValidationEndpoint,
	}
}

func qwenFreeValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func minimaxFreeSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         false,
		ValidationEndpoint:     minimaxFreeValidationEndpoint,
	}
}

func minimaxFreeValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func zaiCodingPlanSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         false,
		ValidationEndpoint:     zaiValidationEndpoint,
	}
}

func ollamaSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  false,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         false,
		NewResponseTransformer: newOllamaTransformer,
		ValidationEndpoint:     ollamaValidationEndpoint,
	}
}

func ollamaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func anthropicSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     anthropicValidationEndpoint,
	}
}

func anthropicValidationEndpoint(providerURL string) string {
	if strings.HasSuffix(providerURL, "/messages") {
		return strings.TrimSuffix(providerURL, "/messages")
	}
	return defaultValidationEndpoint(providerURL, "")
}

func mistralSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     mistralValidationEndpoint,
	}
}

func mistralValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func xaiSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     xaiValidationEndpoint,
	}
}

func xaiValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func azureSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     azureValidationEndpoint,
	}
}

func azureValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func perplexitySpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
		SupportsToolCalls:      false,
		SupportsReasoning:      false,
		SupportsVision:         true,
		ValidationEndpoint:     perplexityValidationEndpoint,
	}
}

func perplexityValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func cohereSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      false,
		SupportsVision:         true,
		ValidationEndpoint:     cohereValidationEndpoint,
	}
}

func cohereValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func deepinfraSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      false,
		SupportsVision:         true,
		ValidationEndpoint:     deepinfraValidationEndpoint,
	}
}

func deepinfraValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func zenSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		SupportsToolCalls:      true,
		SupportsReasoning:      true,
		SupportsVision:         true,
		ValidationEndpoint:     zenValidationEndpoint,
	}
}

func zenValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}
