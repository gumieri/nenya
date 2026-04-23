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
	Enabled    bool `json:"enabled"`
	AutoAgents bool `json:"auto_agents"`
}
