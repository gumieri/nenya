package discovery

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type ProviderDiscoveryResult struct {
	ProviderRef string            `json:"provider_ref"`
	Models      []DiscoveredModel `json:"models"`
	Timestamp   time.Time         `json:"timestamp"`
	Error       string            `json:"error,omitempty"`
}

type PersistentProviderCache struct {
	mu        sync.RWMutex
	entries   map[string]ProviderDiscoveryResult
	cacheDir  string
	cacheTTL  time.Duration
	logger    *slog.Logger
}

func NewPersistentProviderCache(cacheDir string, cacheTTL time.Duration, logger *slog.Logger) *PersistentProviderCache {
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		logger.Error("failed to create cache directory", "dir", cacheDir, "error", err)
	}
	return &PersistentProviderCache{
		entries:  make(map[string]ProviderDiscoveryResult),
		cacheDir: cacheDir,
		cacheTTL: cacheTTL,
		logger:   logger,
	}
}

func (c *PersistentProviderCache) Load() {
	c.mu.Lock()
	defer c.mu.Unlock()

	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		c.logger.Warn("failed to read cache directory", "dir", c.cacheDir, "error", err)
		return
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		providerRef := entry.Name()[:len(entry.Name())-5]
		cachePath := filepath.Join(c.cacheDir, entry.Name())

		data, err := os.ReadFile(cachePath)
		if err != nil {
			c.logger.Warn("failed to read cache file", "file", cachePath, "error", err)
			continue
		}

		var cached ProviderDiscoveryResult
		if err := json.Unmarshal(data, &cached); err != nil {
			c.logger.Warn("failed to unmarshal cache file", "file", cachePath, "error", err)
			continue
		}

		if time.Since(cached.Timestamp) > c.cacheTTL {
			c.logger.Debug("cache entry expired", "provider", providerRef, "age", time.Since(cached.Timestamp))
			os.Remove(cachePath)
			continue
		}

		c.entries[providerRef] = cached
		c.logger.Debug("loaded cached discovery result", "provider", providerRef, "models", len(cached.Models))
	}
}

func (c *PersistentProviderCache) Save(result ProviderDiscoveryResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[result.ProviderRef] = result

	cachePath := filepath.Join(c.cacheDir, result.ProviderRef+".json")
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		c.logger.Error("failed to marshal cache entry", "provider", result.ProviderRef, "error", err)
		return
	}

	if err := os.WriteFile(cachePath, data, 0644); err != nil {
		c.logger.Error("failed to write cache file", "file", cachePath, "error", err)
		return
	}

	c.logger.Debug("saved discovery result to cache", "provider", result.ProviderRef, "models", len(result.Models))
}

func (c *PersistentProviderCache) Get(providerRef string) (ProviderDiscoveryResult, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	result, ok := c.entries[providerRef]
	return result, ok
}

func (c *PersistentProviderCache) GetAll() map[string]ProviderDiscoveryResult {
	c.mu.RLock()
	defer c.mu.RUnlock()

	cp := make(map[string]ProviderDiscoveryResult, len(c.entries))
	for k, v := range c.entries {
		cp[k] = v
	}
	return cp
}

func (c *PersistentProviderCache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries = make(map[string]ProviderDiscoveryResult)

	entries, err := os.ReadDir(c.cacheDir)
	if err != nil {
		return fmt.Errorf("failed to read cache directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		cachePath := filepath.Join(c.cacheDir, entry.Name())
		if err := os.Remove(cachePath); err != nil {
			c.logger.Warn("failed to remove cache file", "file", cachePath, "error", err)
		}
	}

	c.logger.Info("cleared discovery cache", "dir", c.cacheDir)
	return nil
}

func (c *PersistentProviderCache) Prune() {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	for providerRef, result := range c.entries {
		if now.Sub(result.Timestamp) > c.cacheTTL {
			delete(c.entries, providerRef)
			cachePath := filepath.Join(c.cacheDir, providerRef+".json")
			os.Remove(cachePath)
			c.logger.Debug("pruned expired cache entry", "provider", providerRef)
		}
	}
}
