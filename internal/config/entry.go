package config

import (
	"errors"
	"fmt"
)

type ModelEntry struct {
	Provider   string
	MaxContext int
	MaxOutput  int
}

func (e ModelEntry) Validate() error {
	if e.Provider == "" {
		return errors.New("ModelEntry.Provider is required")
	}
	if e.MaxContext < 0 {
		return fmt.Errorf("ModelEntry.MaxContext must be non-negative, got %d", e.MaxContext)
	}
	if e.MaxOutput < 0 {
		return fmt.Errorf("ModelEntry.MaxOutput must be non-negative, got %d", e.MaxOutput)
	}
	return nil
}

type ModelRef struct {
	ID         string
	MaxContext int
	MaxOutput  int
}

type ProviderEntry struct {
	URL           string
	RoutePrefixes []string
	AuthStyle     string
	ApiFormat     string
	Models        []ModelRef
}

func (e ProviderEntry) ToProviderConfig() ProviderConfig {
	return ProviderConfig{
		URL:           e.URL,
		RoutePrefixes: e.RoutePrefixes,
		AuthStyle:     e.AuthStyle,
		ApiFormat:     e.ApiFormat,
	}
}
