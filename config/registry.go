package config

// ProviderRegistry contains the built-in provider definitions (URLs,
// auth styles, and model references). User configs are merged on top of
// these defaults at startup.
var ProviderRegistry = map[string]ProviderEntry{
	"gemini": {
		URL:       "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions",
		AuthStyle: "bearer+x-goog",
	},
	"deepseek": {
		URL:       "https://api.deepseek.com/chat/completions",
		AuthStyle: "bearer",
	},
	"zai": {
		URL:       "https://api.z.ai/api/paas/v4/chat/completions",
		AuthStyle: "bearer",
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
		URL:       "https://api.together.xyz/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"anthropic": {
		URL:       "https://api.anthropic.com/v1/messages",
		AuthStyle: "anthropic",
	},
	"mistral": {
		URL:       "https://api.mistral.ai/v1/chat/completions",
		AuthStyle: "bearer",
	},
	"xai": {
		URL:       "https://api.x.ai/v1/chat/completions",
		AuthStyle: "bearer",
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
	"openai": {
		URL:       "https://api.openai.com/v1/chat/completions",
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
		Models: []ModelRef{
			{ID: "chat/llama3-8b-instruct", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/llama3-70b-instruct", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/mistral-7b-instruct", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/mixtral-8x7b-instruct", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/gemma-2b-it", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/gemma-7b-it", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/code-llama-7b-instruct", MaxContext: 16384, MaxOutput: 16384},
			{ID: "chat/code-llama-13b-instruct", MaxContext: 16384, MaxOutput: 16384},
			{ID: "chat/code-llama-34b-instruct", MaxContext: 16384, MaxOutput: 16384},
			{ID: "chat/code-llama-70b-instruct", MaxContext: 16384, MaxOutput: 16384},
			{ID: "chat/yi-34b-instruct", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/yi-6b-instruct", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/phi-2", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/phi-3", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/phi-3-medium", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/phi-3-mini", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/phi-3-small", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/stablelm-2-12b", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/stablelm-2-zephyr-1.6b", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/stablelm-zephyr-3b", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/starcoder2-15b", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/starcoder2-7b", MaxContext: 8192, MaxOutput: 8192},
			{ID: "chat/starcoder2-3b", MaxContext: 8192, MaxOutput: 8192},
		},
	},
	"zen": {
		URL:       "https://opencode.ai/zen/v1/chat/completions",
		AuthStyle: "bearer",
		FormatURLs: map[string]string{
			"anthropic": "https://opencode.ai/zen/v1/messages",
		},
	},
	"ollama": {
		URL:       "http://127.0.0.1:11434/v1/chat/completions",
		AuthStyle: "none",
	},
	"moonshot": {
		URL:       "https://api.moonshot.cn/v1/chat/completions",
		AuthStyle: "bearer",
	},
}

// ModelRegistry contains the static model definitions with provider
// mapping, context limits, output limits, and pricing. Dynamic discovery
// results are merged with this registry at runtime.
var ModelRegistry = map[string]ModelEntry{
	"gemini-3.1-flash-lite-preview": {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"minimal", "low", "medium", "high"}}, Pricing: PricingOverride{InputCostPer1M: 0.075, OutputCostPer1M: 0.3}},
	"gemini-3-flash-preview":        {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"minimal", "low", "medium", "high"}}, Pricing: PricingOverride{InputCostPer1M: 0.075, OutputCostPer1M: 0.3}},
	"gemini-2.5-flash-lite":         {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Max: 24576, ZeroAllowed: true, DynamicAllowed: true}, Pricing: PricingOverride{InputCostPer1M: 0.075, OutputCostPer1M: 0.3}},
	"gemini-2.5-flash":              {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Max: 24576, ZeroAllowed: true, DynamicAllowed: true}, Pricing: PricingOverride{InputCostPer1M: 0.075, OutputCostPer1M: 0.3}},
	"gemini-2.5-pro":                {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 128, Max: 32768, DynamicAllowed: true}, Pricing: PricingOverride{InputCostPer1M: 1.25, OutputCostPer1M: 10.0}},
	"gemini-3-pro-preview":          {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"low", "high"}}, Pricing: PricingOverride{InputCostPer1M: 2.0, OutputCostPer1M: 15.0}},
	"gemini-3.1-pro-preview":        {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"low", "medium", "high"}}, Pricing: PricingOverride{InputCostPer1M: 2.0, OutputCostPer1M: 15.0}},

	"deepseek-v4-pro":   {Provider: "deepseek", MaxContext: 1000000, MaxOutput: 384000, Pricing: PricingOverride{InputCostPer1M: 2.0, OutputCostPer1M: 8.0}},
	"deepseek-v4-flash": {Provider: "deepseek", MaxContext: 1000000, MaxOutput: 384000, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"glm-5.1":             {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-5-turbo":         {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-5v-turbo":        {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-5":               {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.7":             {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.7-flash":       {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},
	"glm-4.7-flashx":      {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},
	"glm-4.6":             {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.6v":            {Provider: "zai", MaxContext: 200000, MaxOutput: 32000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.5":             {Provider: "zai", MaxContext: 128000, MaxOutput: 96000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.5-air":         {Provider: "zai", MaxContext: 128000, MaxOutput: 96000, Pricing: PricingOverride{InputCostPer1M: 0.15, OutputCostPer1M: 0.15}},
	"glm-4.5-flash":       {Provider: "zai", MaxContext: 128000, MaxOutput: 96000, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},
	"glm-4.5v":            {Provider: "zai", MaxContext: 128000, MaxOutput: 16000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.5-airx":        {Provider: "zai", MaxContext: 128000, MaxOutput: 96000, Pricing: PricingOverride{InputCostPer1M: 0.15, OutputCostPer1M: 0.15}},
	"glm-4.5-x":           {Provider: "zai", MaxContext: 128000, MaxOutput: 96000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},
	"glm-4.6v-flash":      {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},
	"glm-4.6v-flashx":     {Provider: "zai", MaxContext: 200000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},
	"glm-4-32b-0414-128k": {Provider: "zai", MaxContext: 128000, MaxOutput: 16000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 2.0}},

	"nemotron-3-super": {Provider: "nvidia_free", MaxContext: 4000, MaxOutput: 1024, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"qwen-3.6-plus": {Provider: "qwen_free", MaxContext: 8000, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"minimax-m2.5": {Provider: "minimax_free", MaxContext: 8000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"llama-3.3-70b-versatile": {Provider: "groq", MaxContext: 131072, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.59, OutputCostPer1M: 0.79}},
	"mixtral-8x7b-32768":      {Provider: "groq", MaxContext: 32768, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.27, OutputCostPer1M: 0.27}},

	"llama-3.1-405b-instruct": {Provider: "sambanova", MaxContext: 128000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"llama-3.3-70b": {Provider: "cerebras", MaxContext: 8192, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"gpt-4o":                {Provider: "github", MaxContext: 8000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 2.5, OutputCostPer1M: 10.0}},
	"phi-3.5-mini-instruct": {Provider: "github", MaxContext: 128000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 0.1, OutputCostPer1M: 0.1}},

	"qwen2.5-72b-turbo": {Provider: "together", MaxContext: 32768, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 0.9, OutputCostPer1M: 0.9}},

	"claude-opus-4-5":            {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 64000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000, ZeroAllowed: true}, Pricing: PricingOverride{InputCostPer1M: 5.0, OutputCostPer1M: 25.0}},
	"claude-opus-4-1-20250805":   {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 32000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000}, Pricing: PricingOverride{InputCostPer1M: 15.0, OutputCostPer1M: 75.0}},
	"claude-opus-4-0":            {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 32000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000}, Pricing: PricingOverride{InputCostPer1M: 15.0, OutputCostPer1M: 75.0}},
	"claude-sonnet-4-5":          {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 64000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000, ZeroAllowed: true}, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}},
	"claude-sonnet-4-0":          {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 64000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000}, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}},
	"claude-haiku-4-5":           {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 64000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000, ZeroAllowed: true}, Pricing: PricingOverride{InputCostPer1M: 1.0, OutputCostPer1M: 5.0}},
	"claude-opus-4-7":            {Provider: "anthropic", Format: "anthropic", MaxContext: 1000000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000, ZeroAllowed: true, Levels: []string{"low", "medium", "high", "xhigh", "max"}}, Pricing: PricingOverride{InputCostPer1M: 5.0, OutputCostPer1M: 25.0}},
	"claude-opus-4-6":            {Provider: "anthropic", Format: "anthropic", MaxContext: 1000000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000, ZeroAllowed: true, Levels: []string{"low", "medium", "high", "max"}}, Pricing: PricingOverride{InputCostPer1M: 5.0, OutputCostPer1M: 25.0}},
	"claude-sonnet-4-6":          {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 64000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000, ZeroAllowed: true, Levels: []string{"low", "medium", "high"}}, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}},
	"claude-3-7-sonnet-20250219": {Provider: "anthropic", Format: "anthropic", MaxContext: 128000, MaxOutput: 8192, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000}, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}},
	"claude-3-5-sonnet-20241022": {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 64000, Thinking: ModelThinkingConfig{Min: 1024, Max: 128000}, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}},
	"claude-3-5-haiku-latest":    {Provider: "anthropic", Format: "anthropic", MaxContext: 200000, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.25, OutputCostPer1M: 1.25}},

	"mistral-large-latest":    {Provider: "mistral", MaxContext: 256000, MaxOutput: 262144, Pricing: PricingOverride{InputCostPer1M: 4.0, OutputCostPer1M: 12.0}},
	"mistral-small-latest":    {Provider: "mistral", MaxContext: 256000, MaxOutput: 256000, Pricing: PricingOverride{InputCostPer1M: 0.2, OutputCostPer1M: 0.6}},
	"mistral-medium-latest":   {Provider: "mistral", MaxContext: 256000, MaxOutput: 16384, Pricing: PricingOverride{InputCostPer1M: 2.7, OutputCostPer1M: 8.1}},
	"codestral-latest":        {Provider: "mistral", MaxContext: 128000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 0.3, OutputCostPer1M: 0.3}},
	"devstral-medium-latest":  {Provider: "mistral", MaxContext: 256000, MaxOutput: 262144, Pricing: PricingOverride{InputCostPer1M: 0.2, OutputCostPer1M: 0.6}},
	"magistral-medium-latest": {Provider: "mistral", MaxContext: 128000, MaxOutput: 16384, Pricing: PricingOverride{InputCostPer1M: 2.5, OutputCostPer1M: 7.5}},
	"pixtral-large-latest":    {Provider: "mistral", MaxContext: 128000, MaxOutput: 128000, Pricing: PricingOverride{InputCostPer1M: 0.2, OutputCostPer1M: 0.6}},

	"grok-4":      {Provider: "xai", MaxContext: 256000, MaxOutput: 64000, Pricing: PricingOverride{InputCostPer1M: 5.0, OutputCostPer1M: 15.0}},
	"grok-4-fast": {Provider: "xai", MaxContext: 2000000, MaxOutput: 32000, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 5.0}},
	"grok-3":      {Provider: "xai", MaxContext: 131072, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 12.0}},
	"grok-3-fast": {Provider: "xai", MaxContext: 131072, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 5.0}},
	"grok-3-mini": {Provider: "xai", MaxContext: 131072, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 0.5, OutputCostPer1M: 5.0}},

	"sonar-pro":           {Provider: "perplexity", MaxContext: 200000, MaxOutput: 8192, Pricing: PricingOverride{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}},
	"sonar-reasoning-pro": {Provider: "perplexity", MaxContext: 128000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 2.0, OutputCostPer1M: 8.0}},
	"sonar-deep-research": {Provider: "perplexity", MaxContext: 128000, MaxOutput: 32768, Pricing: PricingOverride{InputCostPer1M: 2.0, OutputCostPer1M: 8.0}},
	"sonar":               {Provider: "perplexity", MaxContext: 128000, MaxOutput: 4096, Pricing: PricingOverride{InputCostPer1M: 1.0, OutputCostPer1M: 1.0}},

	"qwen3.5-plus":          {Provider: "zen", MaxContext: 131072, MaxOutput: 8192},
	"minimax-m2.7":          {Provider: "zen", MaxContext: 200000, MaxOutput: 8192},
	"minimax-m2.5-free":     {Provider: "zen", MaxContext: 200000, MaxOutput: 8192},
	"kimi-k2.6":             {Provider: "zen", MaxContext: 262144, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true}},
	"kimi-k2.5":             {Provider: "zen", MaxContext: 131072, MaxOutput: 32768, Thinking: ModelThinkingConfig{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true}},
	"kimi-k2-thinking":      {Provider: "zen", MaxContext: 131072, MaxOutput: 32768, Thinking: ModelThinkingConfig{Min: 1024, Max: 32000, ZeroAllowed: true, DynamicAllowed: true}},
	"kimi-k2":               {Provider: "moonshot", MaxContext: 131072, MaxOutput: 32768},
	"big-pickle":            {Provider: "zen", MaxContext: 200000, MaxOutput: 8192},
	"ling-2.6-flash-free":   {Provider: "zen", MaxContext: 200000, MaxOutput: 8192},
	"hy3-preview-free":      {Provider: "zen", MaxContext: 131072, MaxOutput: 8192},
	"nemotron-3-super-free": {Provider: "zen", MaxContext: 4000, MaxOutput: 1024},
	"gpt-5-nano":            {Provider: "zen", MaxContext: 200000, MaxOutput: 8192},
	"gpt-5.2":               {Provider: "openai", MaxContext: 400000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"none", "low", "medium", "high", "xhigh"}}},
	"gpt-5.3-codex":         {Provider: "openai", MaxContext: 400000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"low", "medium", "high", "xhigh"}}},
	"gpt-5.3-codex-spark":   {Provider: "openai", MaxContext: 128000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"low", "medium", "high", "xhigh"}}},
	"gpt-5.4":               {Provider: "openai", MaxContext: 1050000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"low", "medium", "high", "xhigh"}}},
	"gpt-5.4-mini":          {Provider: "openai", MaxContext: 400000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"low", "medium", "high", "xhigh"}}},
	"gpt-5.5":               {Provider: "openai", MaxContext: 272000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"low", "medium", "high", "xhigh"}}},
	"codex-auto-review":     {Provider: "openai", MaxContext: 272000, MaxOutput: 128000, Thinking: ModelThinkingConfig{Levels: []string{"low", "medium", "high", "xhigh"}}},
}
