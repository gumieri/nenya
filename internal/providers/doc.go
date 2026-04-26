// Package providers defines the ProviderSpec registry that describes each
// upstream provider's capabilities (streaming, tool calls, vision, reasoning,
// content arrays) and optional request sanitization and response transformation
// functions. Built-in provider specs are registered at init time and extended
// by dynamically discovered models.
package providers
