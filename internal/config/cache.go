package config

import (
	"encoding/json"
)

type ResponseCacheConfig struct {
	Enabled            bool   `json:"enabled"`
	MaxEntries         int    `json:"max_entries"`
	MaxEntryBytes      int64  `json:"max_entry_bytes"`
	TTLSeconds         int    `json:"ttl_seconds"`
	EvictEverySeconds  int    `json:"evict_every_seconds"`
	ForceRefreshHeader string `json:"force_refresh_header"`
	enabledSet         bool   `json:"-"`
}

func (c *ResponseCacheConfig) EnabledWasSet() bool { return c.enabledSet }

func (c *ResponseCacheConfig) UnmarshalJSON(data []byte) error {
	type alias ResponseCacheConfig
	aux := struct {
		Enabled *bool `json:"enabled"`
		*alias
	}{
		alias: (*alias)(c),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Enabled != nil {
		c.Enabled = *aux.Enabled
		c.enabledSet = true
	}
	return nil
}

type DiscoveryConfig struct {
	Enabled          bool              `json:"enabled"`
	AutoAgents       bool              `json:"auto_agents"`
	AutoAgentsConfig *AutoAgentsConfig `json:"auto_agents_config,omitempty"`
	enabledSet       bool              `json:"-"`
	autoAgentsSet    bool              `json:"-"`
}

func (d *DiscoveryConfig) EnabledWasSet() bool    { return d.enabledSet }
func (d *DiscoveryConfig) AutoAgentsWasSet() bool { return d.autoAgentsSet }

func (d *DiscoveryConfig) UnmarshalJSON(data []byte) error {
	type alias DiscoveryConfig
	aux := struct {
		Enabled    *bool `json:"enabled"`
		AutoAgents *bool `json:"auto_agents"`
		*alias
	}{
		alias: (*alias)(d),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if aux.Enabled != nil {
		d.Enabled = *aux.Enabled
		d.enabledSet = true
	}
	if aux.AutoAgents != nil {
		d.AutoAgents = *aux.AutoAgents
		d.autoAgentsSet = true
	}
	return nil
}

type AutoAgentCategoryConfig struct {
	Enabled bool `json:"enabled"`
}

type AutoAgentsConfig struct {
	Fast      *AutoAgentCategoryConfig `json:"fast,omitempty"`
	Reasoning *AutoAgentCategoryConfig `json:"reasoning,omitempty"`
	Vision    *AutoAgentCategoryConfig `json:"vision,omitempty"`
	Tools     *AutoAgentCategoryConfig `json:"tools,omitempty"`
	Large     *AutoAgentCategoryConfig `json:"large,omitempty"`
	Balanced  *AutoAgentCategoryConfig `json:"balanced,omitempty"`
	Coding    *AutoAgentCategoryConfig `json:"coding,omitempty"`
}

func (a *AutoAgentsConfig) IsEnabled(category string) bool {
	if a == nil {
		return true
	}
	var cfg *AutoAgentCategoryConfig
	switch category {
	case "fast":
		cfg = a.Fast
	case "reasoning":
		cfg = a.Reasoning
	case "vision":
		cfg = a.Vision
	case "tools":
		cfg = a.Tools
	case "large":
		cfg = a.Large
	case "balanced":
		cfg = a.Balanced
	case "coding":
		cfg = a.Coding
	default:
		return false
	}
	if cfg == nil {
		return false
	}
	return cfg.Enabled
}
