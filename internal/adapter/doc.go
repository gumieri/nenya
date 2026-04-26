// Package adapter provides the ProviderAdapter interface and concrete
// implementations for each upstream AI provider. Adapters handle provider-specific
// concerns: authentication injection (Bearer, Google API key, etc.), request body
// mutation (model name mapping, parameter normalization), response mutation
// (Gemini tool_calls normalization, Anthropic format conversion), and error
// classification (permanent vs retryable vs rate-limited).
package adapter
