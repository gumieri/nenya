package main

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
	return ProviderConfig(e)
}

var ProviderRegistry = map[string]ProviderEntry{
	"gemini": {
		URL:           "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		RoutePrefixes: []string{"gemini-"},
		AuthStyle:     "bearer+x-goog",
	},
	"deepseek": {
		URL:           "https://api.deepseek.com/chat/completions",
		RoutePrefixes: []string{"deepseek-"},
		AuthStyle:     "bearer",
	},
	"zai": {
		URL:           "https://api.z.ai/api/paas/v4/chat/completions",
		RoutePrefixes: []string{"zai-", "glm-"},
		AuthStyle:     "bearer",
	},
	"zai-coding-plan": {
		URL:       "https://api.z.ai/api/coding/paas/v4/chat/completions",
		AuthStyle: "bearer",
	},
	"groq": {
		URL:           "https://api.groq.com/openai/v1/chat/completions",
		RoutePrefixes: []string{"llama-", "llama3-", "mixtral-", "whisper-"},
		AuthStyle:     "bearer",
	},
	"together": {
		URL:           "https://api.together.xyz/v1/chat/completions",
		RoutePrefixes: []string{"meta-llama/", "mistralai/", "qwen/", "together/"},
		AuthStyle:     "bearer",
	},
	"nvidia_free": {
		URL:       "https://integrate.api.nvidia.com/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"qwen_free": {
		URL:       "https://dashscope.aliyuncs.com/compatible-mode/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"minimax_free": {
		URL:       "https://api.minimax.chat/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"sambanova": {
		URL:       "https://api.sambanova.ai/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"cerebras": {
		URL:       "https://api.cerebras.ai/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"github": {
		URL:       "https://models.inference.ai.azure.com/chat/completions",
		AuthStyle: "bearer",
	},
	"ollama": {
		URL:       "http://127.0.0.1:11434/v1/chat/completions",
		AuthStyle: "none",
	},
}

var ModelRegistry = map[string]ModelEntry{
	// Google Gemini
	"gemini-3.1-flash-lite-preview": {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},
	"gemini-3-flash-preview":        {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},
	"gemini-2.5-flash-lite":         {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},
	"gemini-2.5-flash":              {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},

	// DeepSeek
	"deepseek-reasoner": {Provider: "deepseek", MaxContext: 128000, MaxOutput: 8192},
	"deepseek-chat":     {Provider: "deepseek", MaxContext: 128000, MaxOutput: 8192},

	// Z.ai
	"glm-5.1":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-5-turbo":    {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-5v-turbo":   {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-5":          {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.7":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.7-flash":  {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.7-flashx": {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.6":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.6v":       {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5-air":    {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5-flash":  {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5v":       {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},

	// NVIDIA Free
	"nemotron-3-super": {Provider: "nvidia_free", MaxContext: 4000, MaxOutput: 1024},

	// Qwen Free
	"qwen-3.6-plus": {Provider: "qwen_free", MaxContext: 8000, MaxOutput: 8192},

	// MiniMax Free
	"minimax-m2.5": {Provider: "minimax_free", MaxContext: 8000, MaxOutput: 4096},

	// Groq
	"llama-3.3-70b-versatile": {Provider: "groq", MaxContext: 131072, MaxOutput: 32768},
	"mixtral-8x7b-32768":      {Provider: "groq", MaxContext: 32768, MaxOutput: 32768},

	// Sambanova
	"llama-3.1-405b-instruct": {Provider: "sambanova", MaxContext: 128000, MaxOutput: 4096},

	// Cerebras
	"llama-3.3-70b": {Provider: "cerebras", MaxContext: 8192, MaxOutput: 8192},

	// GitHub Models
	"gpt-4o":                {Provider: "github", MaxContext: 8000, MaxOutput: 4096},
	"phi-3.5-mini-instruct": {Provider: "github", MaxContext: 128000, MaxOutput: 4096},

	// Together
	"qwen2.5-72b-turbo": {Provider: "together", MaxContext: 32768, MaxOutput: 4096},
}
