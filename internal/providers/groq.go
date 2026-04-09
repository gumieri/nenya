package providers

import (
	"net/url"
	"strings"
)

func groqSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  false,
		SupportsAutoToolChoice: false,
		SupportsContentArrays:  false,
		ValidationEndpoint:     groqValidationEndpoint,
	}
}

func groqValidationEndpoint(providerURL string) string {
	u, err := url.Parse(providerURL)
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Host)

	if strings.Contains(host, "api.groq.com") {
		return "https://api.groq.com/openai/v1/models"
	}
	return defaultValidationEndpoint(providerURL, u.Path)
}
