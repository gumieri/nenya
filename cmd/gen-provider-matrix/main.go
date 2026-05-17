package main

import (
	"fmt"
	"strings"

	providerpkg "nenya/internal/providers"
)

func main() {
	providers := []string{
		"anthropic", "azure", "cohere", "deepinfra", "gemini", "github", "groq",
		"mistral", "nvidia", "nvidia_free", "ollama", "openai", "openrouter",
		"perplexity", "qwen_free", "sambanova", "deepseek", "together",
		"xai", "zai", "zen",
	}

	fmt.Println("# Provider Service Kinds Matrix")
	fmt.Println()
	fmt.Println("This document provides a comprehensive overview of all supported providers and the service kinds (endpoints) they support within the Nenya gateway.")
	fmt.Println()
	fmt.Println("Note: Wire format capabilities (stream_options, tool_calls, reasoning, vision) are now **model-level** and inferred dynamically via `discovery.InferCapabilities()` from model IDs.")
	fmt.Println()
	fmt.Println("| Provider | Service Kinds | Notes |")
	fmt.Println("|----------|--------------|-------|")

	for _, name := range providers {
		spec, ok := providerpkg.Get(name)
		if !ok {
			continue
		}

		kindNames := make([]string, len(spec.ServiceKinds))
		for i, k := range spec.ServiceKinds {
			kindNames[i] = string(k)
		}
		kindsStr := strings.Join(kindNames, ", ")

		fmt.Printf("| %s | %s | %s |\n",
			name,
			kindsStr,
			getNotes(name),
		)
	}
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
