package adapter

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
)

type AdapterEntry struct {
	Adapter            ProviderAdapter
	NewTransformer     func(cache *infra.ThoughtSignatureCache) stream.ResponseTransformer
	ValidationEndpoint func(providerURL string) string
}

var registryMu sync.RWMutex
var registry = map[string]AdapterEntry{}

func Register(name string, entry AdapterEntry) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[strings.ToLower(name)] = entry
}

func ForProvider(name string) ProviderAdapter {
	registryMu.RLock()
	defer registryMu.RUnlock()
	if entry, ok := registry[strings.ToLower(name)]; ok {
		return entry.Adapter
	}
	return defaultAdapter
}

func ForProviderWithAuth(name string, authStyle string) ProviderAdapter {
	if a := ForProvider(name); a != defaultAdapter {
		return a
	}
	return AdapterForAuthStyle(authStyle)
}

func AdapterForAuthStyle(authStyle string) ProviderAdapter {
	switch strings.ToLower(authStyle) {
	case "none":
		return NewNoAuthAdapter(Capabilities{})
	case "bearer+x-goog":
		return &bearerPlusGoogAdapter{}
	case "anthropic":
		return NewAnthropicAdapter()
	case "azure":
		return NewAzureAdapter()
	default:
		return defaultAdapter
	}
}

type bearerPlusGoogAdapter struct{}

func (a *bearerPlusGoogAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return body, nil
}

func (a *bearerPlusGoogAdapter) InjectAuth(req *http.Request, apiKey string) error {
	return (&BearerPlusGoogAuth{}).InjectAuth(req, apiKey)
}

func (a *bearerPlusGoogAdapter) MutateResponse(body []byte) ([]byte, error) {
	return body, nil
}

func (a *bearerPlusGoogAdapter) NormalizeError(statusCode int, body []byte) ErrorClass {
	return defaultNormalizeError(statusCode, body)
}

func Entry(name string) (AdapterEntry, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	entry, ok := registry[strings.ToLower(name)]
	return entry, ok
}

var defaultAdapter ProviderAdapter = &OpenAIAdapter{
	Caps: Capabilities{},
}

func registerOpenAI(name string, caps Capabilities) {
	Register(name, AdapterEntry{
		Adapter: NewOpenAIAdapter(caps),
	})
}

func init() {
	registerOpenAI("openai", Capabilities{
		StreamOptions:  false,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("deepseek", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	Register("gemini", AdapterEntry{
		Adapter:        &geminiAdapterShim{},
		NewTransformer: newGeminiTransformerShim,
	})

	Register("zai", AdapterEntry{
		Adapter: &zaiAdapterShim{},
	})

	registerOpenAI("zai-coding-plan", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("groq", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("together", Capabilities{
		StreamOptions:  false,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("github", Capabilities{
		StreamOptions:  false,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	Register("openrouter", AdapterEntry{
		Adapter: NewOpenRouterAdapter(),
	})

	Register("anthropic", AdapterEntry{
		Adapter: NewAnthropicAdapter(),
	})

	Register("mistral", AdapterEntry{
		Adapter: NewMistralAdapter(),
	})

	Register("xai", AdapterEntry{
		Adapter: NewXAIAdapter(),
	})

	Register("azure", AdapterEntry{
		Adapter: NewAzureAdapter(),
	})

	Register("perplexity", AdapterEntry{
		Adapter: NewPerplexityAdapter(),
	})

	Register("cohere", AdapterEntry{
		Adapter: NewCohereAdapter(),
	})

	Register("deepinfra", AdapterEntry{
		Adapter: NewDeepInfraAdapter(),
	})

	registerOpenAI("sambanova", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("cerebras", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("nvidia", Capabilities{
		StreamOptions:  false,
		AutoToolChoice: false,
		ContentArrays:  true,
	})

	registerOpenAI("nvidia_free", Capabilities{
		StreamOptions:  false,
		AutoToolChoice: false,
		ContentArrays:  true,
	})

	registerOpenAI("qwen_free", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: false,
		ContentArrays:  true,
	})

	registerOpenAI("minimax_free", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	Register("ollama", AdapterEntry{
		Adapter: NewOllamaAdapter(),
	})

	registerOpenAI("zen", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})

	registerOpenAI("moonshot", Capabilities{
		StreamOptions:  true,
		AutoToolChoice: true,
		ContentArrays:  true,
	})
}

type geminiAdapterShim struct{}

func (s *geminiAdapterShim) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return ForProviderWithDeps("gemini", nil, nil).MutateRequest(body, model, stream)
}

func (s *geminiAdapterShim) InjectAuth(req *http.Request, apiKey string) error {
	return ForProviderWithDeps("gemini", nil, nil).InjectAuth(req, apiKey)
}

func (s *geminiAdapterShim) MutateResponse(body []byte) ([]byte, error) {
	return ForProviderWithDeps("gemini", nil, nil).MutateResponse(body)
}

func (s *geminiAdapterShim) NormalizeError(statusCode int, body []byte) ErrorClass {
	return ForProviderWithDeps("gemini", nil, nil).NormalizeError(statusCode, body)
}

type zaiAdapterShim struct{}

func (s *zaiAdapterShim) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
	return ForProviderWithDeps("zai", nil, nil).MutateRequest(body, model, stream)
}

func (s *zaiAdapterShim) InjectAuth(req *http.Request, apiKey string) error {
	return ForProviderWithDeps("zai", nil, nil).InjectAuth(req, apiKey)
}

func (s *zaiAdapterShim) MutateResponse(body []byte) ([]byte, error) {
	return ForProviderWithDeps("zai", nil, nil).MutateResponse(body)
}

func (s *zaiAdapterShim) NormalizeError(statusCode int, body []byte) ErrorClass {
	return ForProviderWithDeps("zai", nil, nil).NormalizeError(statusCode, body)
}

var (
	geminiOnce    sync.Once
	zaiOnce       sync.Once
	geminiAdapter *GeminiAdapter
	zaiAdapter    *ZAIAdapter
)

func ForProviderWithDeps(name string, cache *infra.ThoughtSignatureCache, extractFn func(map[string]interface{}) string) ProviderAdapter {
	switch strings.ToLower(name) {
	case "gemini":
		geminiOnce.Do(func() {
			geminiAdapter = NewGeminiAdapter(GeminiAdapterDeps{
				ThoughtSigCache: cache,
				ExtractContent:  extractFn,
			})
		})
		return geminiAdapter
	case "zai", "zai-coding-plan":
		zaiOnce.Do(func() {
			zaiAdapter = NewZAIAdapter(ZAIAdapterDeps{
				ExtractContent: extractFn,
			})
		})
		return zaiAdapter
	default:
		return ForProvider(name)
	}
}

func InitWithDeps(logger *slog.Logger, cache *infra.ThoughtSignatureCache, extractFn func(map[string]interface{}) string) {
	geminiOnce = sync.Once{}
	zaiOnce = sync.Once{}
	geminiAdapter = nil
	zaiAdapter = nil

	geminiAdapter = NewGeminiAdapter(GeminiAdapterDeps{
		ThoughtSigCache: cache,
		ExtractContent:  extractFn,
		Logger:          logger,
	})

	zaiAdapter = NewZAIAdapter(ZAIAdapterDeps{
		ExtractContent: extractFn,
		Logger:         logger,
	})

	Register("gemini", AdapterEntry{
		Adapter:        geminiAdapter,
		NewTransformer: newGeminiTransformerShim,
	})
	Register("zai", AdapterEntry{
		Adapter: zaiAdapter,
	})
	Register("zai-coding-plan", AdapterEntry{
		Adapter: zaiAdapter,
	})
}

func newGeminiTransformerShim(cache *infra.ThoughtSignatureCache) stream.ResponseTransformer {
	return &geminiTransformerShim{
		inner: NewGeminiAdapter(GeminiAdapterDeps{
			ThoughtSigCache: cache,
		}),
	}
}

type geminiTransformerShim struct {
	inner *GeminiAdapter
}

func (t *geminiTransformerShim) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	return t.inner.MutateResponse(data)
}
