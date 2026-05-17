package providers

import (
	"net/url"
	"strings"
)

func togetherSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: togetherValidationEndpoint,
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
		ServiceKinds: []ServiceKind{
			ServiceKindLLM,
			ServiceKindEmbedding,
			ServiceKindTTS,
			ServiceKindSTT,
			ServiceKindImage,
			ServiceKindImageToText,
		},
		ValidationEndpoint: openaiValidationEndpoint,
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
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: githubValidationEndpoint,
	}
}

func githubValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func openrouterSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: openrouterValidationEndpoint,
	}
}

func openrouterValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func sambanovaSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: sambanovaValidationEndpoint,
	}
}

func sambanovaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func cerebrasSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: cerebrasValidationEndpoint,
	}
}

func cerebrasValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func nvidiaSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: nvidiaValidationEndpoint,
	}
}

func nvidiaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func nvidiaFreeSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: nvidiaValidationEndpoint,
	}
}

func qwenFreeSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: qwenFreeValidationEndpoint,
	}
}

func qwenFreeValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func minimaxFreeSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: minimaxFreeValidationEndpoint,
	}
}

func minimaxFreeValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func zaiCodingPlanSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: zaiValidationEndpoint,
	}
}

func ollamaSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:           []ServiceKind{ServiceKindLLM},
		NewResponseTransformer: newOllamaTransformer,
		ValidationEndpoint:     ollamaValidationEndpoint,
	}
}

func ollamaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func anthropicSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: anthropicValidationEndpoint,
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
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: mistralValidationEndpoint,
	}
}

func mistralValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func xaiSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: xaiValidationEndpoint,
	}
}

func xaiValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func azureSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: azureValidationEndpoint,
	}
}

func azureValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func perplexitySpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM, ServiceKindWebSearch},
		ValidationEndpoint: perplexityValidationEndpoint,
	}
}

func perplexityValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func cohereSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM, ServiceKindRerank},
		ValidationEndpoint: cohereValidationEndpoint,
	}
}

func cohereValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func deepinfraSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: deepinfraValidationEndpoint,
	}
}

func deepinfraValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func zenSpec() ProviderSpec {
	return ProviderSpec{
		ServiceKinds:       []ServiceKind{ServiceKindLLM},
		ValidationEndpoint: zenValidationEndpoint,
	}
}

func zenValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}
