package providers

import (
	"testing"
)

func TestGet_ExistingProvider(t *testing.T) {
	cases := []string{"gemini", "zai", "groq", "deepseek", "together", "openai", "github", "openrouter", "sambanova", "cerebras"}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			spec, ok := Get(name)
			if !ok {
				t.Fatalf("expected provider %q to exist", name)
			}
			if len(spec.ServiceKinds) == 0 {
				t.Fatalf("expected provider %q to have ServiceKinds", name)
			}
		})
	}
}

func TestGet_NonExistingProvider(t *testing.T) {
	_, ok := Get("nonexistent")
	if ok {
		t.Fatal("expected false for nonexistent provider")
	}
}

func TestAllProvidersHaveLLMKind(t *testing.T) {
	for name, spec := range Registry {
		if !spec.Supports(ServiceKindLLM) {
			t.Errorf("provider %q should support ServiceKindLLM", name)
		}
	}
}

func TestOpenAIHasEmbedding(t *testing.T) {
	spec, ok := Get("openai")
	if !ok {
		t.Fatal("expected openai provider to exist")
	}
	if !spec.Supports(ServiceKindEmbedding) {
		t.Error("expected openai to support ServiceKindEmbedding")
	}
	if !spec.Supports(ServiceKindTTS) {
		t.Error("expected openai to support ServiceKindTTS")
	}
	if !spec.Supports(ServiceKindSTT) {
		t.Error("expected openai to support ServiceKindSTT")
	}
}

func TestPerplexityHasWebSearch(t *testing.T) {
	spec, ok := Get("perplexity")
	if !ok {
		t.Fatal("expected perplexity provider to exist")
	}
	if !spec.Supports(ServiceKindWebSearch) {
		t.Error("expected perplexity to support ServiceKindWebSearch")
	}
}

func TestCohereHasRerank(t *testing.T) {
	spec, ok := Get("cohere")
	if !ok {
		t.Fatal("expected cohere provider to exist")
	}
	if !spec.Supports(ServiceKindRerank) {
		t.Error("expected cohere to support ServiceKindRerank")
	}
}
