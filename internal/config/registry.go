package config

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
		RoutePrefixes: []string{"glm-"},
		AuthStyle:     "bearer",
	},
	"zai-coding-plan": {
		URL:       "https://api.z.ai/api/coding/paas/v4/chat/completions",
		AuthStyle: "bearer",
	},
	"groq": {
		URL:       "https://api.groq.com/openai/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"together": {
		URL:           "https://api.together.xyz/v1/chat/completions",
		RoutePrefixes: []string{"together/"},
		AuthStyle:     "bearer",
	},
	"anthropic": {
		URL:           "https://api.anthropic.com/v1/messages",
		RoutePrefixes: []string{"claude-"},
		AuthStyle:     "anthropic",
	},
	"mistral": {
		URL:           "https://api.mistral.ai/v1/chat/completions",
		RoutePrefixes: []string{"mistral-", "codestral-", "devstral-"},
		AuthStyle:     "bearer",
	},
	"xai": {
		URL:           "https://api.x.ai/v1/chat/completions",
		RoutePrefixes: []string{"grok-"},
		AuthStyle:     "bearer",
	},
	"perplexity": {
		URL:       "https://api.perplexity.ai/chat/completions",
		AuthStyle: "bearer",
	},
	"cohere": {
		URL:       "https://api.cohere.com/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"deepinfra": {
		URL:       "https://api.deepinfra.com/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"openrouter": {
		URL:       "https://openrouter.ai/api/v1/chat/completions",
		AuthStyle: "bearer",
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
	"nvidia": {
		URL:       "https://integrate.api.nvidia.com/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"ollama": {
		URL:       "http://127.0.0.1:11434/v1/chat/completions",
		AuthStyle: "none",
	},
}

var ModelRegistry = map[string]ModelEntry{
	"gemini-3.1-flash-lite-preview": {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},
	"gemini-3-flash-preview":        {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},
	"gemini-2.5-flash-lite":         {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},
	"gemini-2.5-flash":              {Provider: "gemini", MaxContext: 128000, MaxOutput: 8192},

	"deepseek-reasoner": {Provider: "deepseek", MaxContext: 128000, MaxOutput: 8192},
	"deepseek-chat":     {Provider: "deepseek", MaxContext: 128000, MaxOutput: 8192},

	"glm-5.1":        {Provider: "zai", MaxContext: 128000, MaxOutput: 16384},
	"glm-5-turbo":    {Provider: "zai", MaxContext: 128000, MaxOutput: 16384},
	"glm-5v-turbo":   {Provider: "zai", MaxContext: 128000, MaxOutput: 16384},
	"glm-5":          {Provider: "zai", MaxContext: 128000, MaxOutput: 16384},
	"glm-4.7":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.7-flash":  {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.7-flashx": {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.6":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.6v":       {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5":        {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5-air":    {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5-flash":  {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},
	"glm-4.5v":       {Provider: "zai", MaxContext: 128000, MaxOutput: 4096},

	"nemotron-3-super": {Provider: "nvidia_free", MaxContext: 4000, MaxOutput: 1024},

	"qwen-3.6-plus": {Provider: "qwen_free", MaxContext: 8000, MaxOutput: 8192},

	"minimax-m2.5": {Provider: "minimax_free", MaxContext: 8000, MaxOutput: 4096},

	"llama-3.3-70b-versatile": {Provider: "groq", MaxContext: 131072, MaxOutput: 8192},
	"mixtral-8x7b-32768":      {Provider: "groq", MaxContext: 32768, MaxOutput: 8192},

	"llama-3.1-405b-instruct": {Provider: "sambanova", MaxContext: 128000, MaxOutput: 4096},

	"llama-3.3-70b": {Provider: "cerebras", MaxContext: 8192, MaxOutput: 8192},

	"gpt-4o":                {Provider: "github", MaxContext: 8000, MaxOutput: 4096},
	"phi-3.5-mini-instruct": {Provider: "github", MaxContext: 128000, MaxOutput: 4096},

	"qwen2.5-72b-turbo": {Provider: "together", MaxContext: 32768, MaxOutput: 4096},

	"claude-opus-4-5":            {Provider: "anthropic", MaxContext: 200000, MaxOutput: 64000},
	"claude-opus-4-0":            {Provider: "anthropic", MaxContext: 200000, MaxOutput: 32000},
	"claude-sonnet-4-5":          {Provider: "anthropic", MaxContext: 200000, MaxOutput: 64000},
	"claude-sonnet-4-0":          {Provider: "anthropic", MaxContext: 200000, MaxOutput: 64000},
	"claude-haiku-4-5":           {Provider: "anthropic", MaxContext: 200000, MaxOutput: 64000},
	"claude-3-7-sonnet-20250219": {Provider: "anthropic", MaxContext: 200000, MaxOutput: 64000},
	"claude-3-5-sonnet-20241022": {Provider: "anthropic", MaxContext: 200000, MaxOutput: 8192},
	"claude-3-5-haiku-latest":    {Provider: "anthropic", MaxContext: 200000, MaxOutput: 8192},

	"mistral-large-latest":    {Provider: "mistral", MaxContext: 262144, MaxOutput: 262144},
	"mistral-small-latest":    {Provider: "mistral", MaxContext: 256000, MaxOutput: 256000},
	"mistral-medium-latest":   {Provider: "mistral", MaxContext: 128000, MaxOutput: 16384},
	"codestral-latest":        {Provider: "mistral", MaxContext: 256000, MaxOutput: 4096},
	"devstral-medium-latest":  {Provider: "mistral", MaxContext: 262144, MaxOutput: 262144},
	"magistral-medium-latest": {Provider: "mistral", MaxContext: 128000, MaxOutput: 16384},
	"pixtral-large-latest":    {Provider: "mistral", MaxContext: 128000, MaxOutput: 128000},

	"grok-4":      {Provider: "xai", MaxContext: 256000, MaxOutput: 64000},
	"grok-4-fast": {Provider: "xai", MaxContext: 2000000, MaxOutput: 30000},
	"grok-3":      {Provider: "xai", MaxContext: 131072, MaxOutput: 8192},
	"grok-3-fast": {Provider: "xai", MaxContext: 131072, MaxOutput: 8192},
	"grok-3-mini": {Provider: "xai", MaxContext: 131072, MaxOutput: 8192},

	"sonar-pro":           {Provider: "perplexity", MaxContext: 200000, MaxOutput: 8192},
	"sonar-reasoning-pro": {Provider: "perplexity", MaxContext: 128000, MaxOutput: 4096},
	"sonar-deep-research": {Provider: "perplexity", MaxContext: 128000, MaxOutput: 32768},
	"sonar":               {Provider: "perplexity", MaxContext: 128000, MaxOutput: 4096},
}
