package gateway

import (
	"io"
	"log/slog"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/discovery"
)

func TestWarnModelsMissingMaxContext(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	catalog := discovery.NewModelCatalog()

	t.Run("catalog with MaxContext>0 logs no warnings", func(t *testing.T) {
		catalog.Add(discovery.DiscoveredModel{
			ID:         "model-1",
			Provider:   "provider-a",
			MaxContext: 128000,
		})
		catalog.Add(discovery.DiscoveredModel{
			ID:         "model-2",
			Provider:   "provider-b",
			MaxContext: 64000,
		})

		providers := map[string]*config.Provider{}

		warnModelsMissingMaxContext(logger, providers, catalog)

	})

	t.Run("catalog with MaxContext=0 logs warnings", func(t *testing.T) {
		catalog.Add(discovery.DiscoveredModel{
			ID:         "qwen3:14b",
			Provider:   "ollama",
			MaxContext: 0,
		})
		catalog.Add(discovery.DiscoveredModel{
			ID:         "llama2",
			Provider:   "ollama",
			MaxContext: 0,
		})

		providers := map[string]*config.Provider{}

		warnModelsMissingMaxContext(logger, providers, catalog)

	})

	t.Run("catalog with mixed MaxContext logs only warnings for missing", func(t *testing.T) {
		catalog.Add(discovery.DiscoveredModel{
			ID:         "gpt-4",
			Provider:   "openai",
			MaxContext: 128000,
		})
		catalog.Add(discovery.DiscoveredModel{
			ID:         "local-model",
			Provider:   "ollama",
			MaxContext: 0,
		})

		providers := map[string]*config.Provider{}

		warnModelsMissingMaxContext(logger, providers, catalog)

	})

	t.Run("empty provider name uses 'static config'", func(t *testing.T) {
		catalog.Add(discovery.DiscoveredModel{
			ID:         "static-model",
			Provider:   "",
			MaxContext: 0,
		})

		providers := map[string]*config.Provider{}

		warnModelsMissingMaxContext(logger, providers, catalog)

	})
}