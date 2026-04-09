package providers

import (
	"net/url"
	"strings"
)

func deepseekSpec() ProviderSpec {
	return ProviderSpec{
		SupportsStreamOptions:  true,
		SupportsAutoToolChoice: true,
		SupportsContentArrays:  true,
		ValidationEndpoint:     deepseekValidationEndpoint,
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
