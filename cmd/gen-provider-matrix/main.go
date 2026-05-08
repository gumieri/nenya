package main

import (
	"fmt"

	providerpkg "nenya/internal/providers"
)

func main() {
	providers := []string{
		"anthropic", "azure", "cohere", "deepinfra", "gemini", "github", "groq",
		"mistral", "nvidia", "nvidia_free", "ollama", "openai", "openrouter",
		"perplexity", "qwen_free", "sambanova", "deepseek", "together",
		"xai", "zai", "zen",
	}

	fmt.Println("# Provider Capabilities Matrix")
	fmt.Println()
	fmt.Println("This document provides a comprehensive overview of all supported LLM providers and their capabilities within the Nenya gateway.")
	fmt.Println()
	fmt.Println("| Provider | Stream Options | Auto Tool Choice | Content Arrays | Tool Calls | Reasoning | Vision | Notes |")
	fmt.Println("|----------|---------------|-----------------|----------------|------------|-----------|--------|-------|")

	for _, name := range providers {
		spec, ok := providerpkg.Get(name)
		if !ok {
			continue
		}

		fmt.Printf("| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			name,
			checkmark(spec.SupportsStreamOptions),
			checkmark(spec.SupportsAutoToolChoice),
			checkmark(spec.SupportsContentArrays),
			checkmark(spec.SupportsToolCalls),
			checkmark(spec.SupportsReasoning),
			checkmark(spec.SupportsVision),
			getNotes(name),
		)
	}
}

func checkmark(b bool) string {
	if b {
		return "✅"
	}
	return "❌"
}

func getNotes(name string) string {
	switch name {
	case "anthropic":
		return "Full OpenAI↔Anthropic format conversion"
	case "azure":
		return "Azure OpenAI endpoint"
	case "gemini":
		return "Google-style dual auth (Authorization + x-goog-api-key)"
	case "github":
		return "GitHub Models"
	case "ollama":
		return "Local inference"
	case "openrouter":
		return "Aggregator gateway"
	case "deepseek":
		return "Requires `reasoning_content` on assistant messages"
	case "zai":
		return "Zhipu GLM - supports thinking mode"
	default:
		return ""
	}
}
