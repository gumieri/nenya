package providers

import (
	"net/url"
	"strings"
)

func togetherSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
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
		ValidationEndpoint:     openrouterValidationEndpoint,
	}
}

func openrouterValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func sambanovaSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
		ValidationEndpoint:     sambanovaValidationEndpoint,
	}
}

func sambanovaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func cerebrasSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  true,
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
		SupportsContentArrays:  false,
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
		SupportsContentArrays:  false,
		ValidationEndpoint:     nvidiaValidationEndpoint,
	}
}

func qwenFreeSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  false,
		ValidationEndpoint:     qwenFreeValidationEndpoint,
	}
}

func qwenFreeValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}

func minimaxFreeSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  false,
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
		ValidationEndpoint:     zaiValidationEndpoint,
	}
}

func ollamaSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  false,
		ValidationEndpoint:     ollamaValidationEndpoint,
	}
}

func ollamaValidationEndpoint(providerURL string) string {
	return defaultValidationEndpoint(providerURL, "")
}
