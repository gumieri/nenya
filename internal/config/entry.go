package config

type ModelEntry struct {
	Provider   string
	MaxContext int
	MaxOutput  int
}

type ProviderEntry struct {
	URL           string
	RoutePrefixes []string
	AuthStyle     string
	ApiFormat     string
}

func (e ProviderEntry) ToProviderConfig() ProviderConfig {
	return ProviderConfig{
		URL:           e.URL,
		RoutePrefixes: e.RoutePrefixes,
		AuthStyle:     e.AuthStyle,
		ApiFormat:     e.ApiFormat,
	}
}
