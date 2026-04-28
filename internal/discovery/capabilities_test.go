package discovery

import (
	"testing"
)

func TestInferCapabilities_ProviderQualifiedModels(t *testing.T) {
	tests := []struct {
		name                string
		modelID             string
		wantToolCalls       bool
		wantReasoning       bool
		wantVision          bool
		wantNil             bool
	}{
		{
			name:          "external-deepseek v4-pro trial",
			modelID:       "external-deepseek/deepseek-v4-pro-trial",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "azureml-deepseek DeepSeek-R1",
			modelID:       "azureml-deepseek/DeepSeek-R1",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "z-ai glm-4.7",
			modelID:       "z-ai/glm-4.7",
			wantToolCalls: true,
			wantReasoning: false,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "z-ai glm-5-turbo",
			modelID:       "z-ai/glm-5-turbo",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    true,
			wantNil:       false,
		},
		{
			name:          "nvidia qwen3 coder",
			modelID:       "nvidia/qwen3-coder-480b-a35b-instruct",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "nvidia qwen2.5 coder",
			modelID:       "nvidia/qwen2.5-coder-32b-instruct",
			wantToolCalls: true,
			wantReasoning: false,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "openai gpt-4",
			modelID:       "openai/gpt-4",
			wantToolCalls: true,
			wantReasoning: false,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "openai gpt-4o",
			modelID:       "openai/gpt-4o",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    true,
			wantNil:       false,
		},
		{
			name:          "anthropic claude-4",
			modelID:       "anthropic/claude-4-opus",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    true,
			wantNil:       false,
		},
		{
			name:          "mistral devstral",
			modelID:       "mistralai/devstral-2-123b-instruct-2512",
			wantToolCalls: true,
			wantReasoning: false,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "mistral mistral-large",
			modelID:       "mistralai/mistral-large-2411",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "unknown provider model",
			modelID:       "unknown-org/unknown-model-123",
			wantToolCalls: false,
			wantReasoning: false,
			wantVision:    false,
			wantNil:       true,
		},
		{
			name:          "simple deepseek v4",
			modelID:       "deepseek-v4-flash",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    false,
			wantNil:       false,
		},
		{
			name:          "simple glm-5",
			modelID:       "glm-5-turbo",
			wantToolCalls: true,
			wantReasoning: true,
			wantVision:    true,
			wantNil:       false,
		},
		{
			name:          "empty model ID",
			modelID:       "",
			wantToolCalls: false,
			wantReasoning: false,
			wantVision:    false,
			wantNil:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferCapabilities(tt.modelID)
			if tt.wantNil {
				if got != nil {
					t.Errorf("InferCapabilities(%q) = %v, want nil", tt.modelID, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("InferCapabilities(%q) returned nil, expected capabilities", tt.modelID)
			}

			if got.SupportsToolCalls != tt.wantToolCalls {
				t.Errorf("InferCapabilities(%q).SupportsToolCalls = %v, want %v", tt.modelID, got.SupportsToolCalls, tt.wantToolCalls)
			}
			if got.SupportsReasoning != tt.wantReasoning {
				t.Errorf("InferCapabilities(%q).SupportsReasoning = %v, want %v", tt.modelID, got.SupportsReasoning, tt.wantReasoning)
			}
			if got.SupportsVision != tt.wantVision {
				t.Errorf("InferCapabilities(%q).SupportsVision = %v, want %v", tt.modelID, got.SupportsVision, tt.wantVision)
			}
		})
	}
}

func TestInferCapabilities_BackwardCompatibility(t *testing.T) {
	tests := []struct {
		name          string
		modelID       string
		wantToolCalls bool
		wantReasoning bool
		wantVision    bool
	}{
		{"claude-3-haiku", "claude-3-haiku", true, false, true},
		{"claude-4-opus", "claude-4-opus", true, true, true},
		{"gemini-2-flash", "gemini-2-flash", true, true, true},
		{"gemini-1.5-pro", "gemini-1.5-pro", true, false, true},
		{"gpt-4o", "gpt-4o", true, true, true},
		{"gpt-4-turbo", "gpt-4-turbo", true, false, true},
		{"gpt-4", "gpt-4", true, false, false},
		{"o1", "o1", true, true, false},
		{"o3", "o3", true, true, false},
		{"o4", "o4", true, true, false},
		{"deepseek-v4-pro", "deepseek-v4-pro", true, true, false},
		{"deepseek-r1", "deepseek-r1", true, true, false},
		{"glm-4.6", "glm-4.6", true, false, false},
		{"glm-5", "glm-5", true, true, true},
		{"qwen2.5-coder", "qwen2.5-coder", true, false, false},
		{"qwen3-coder", "qwen3-coder", true, true, false},
		{"mistral-large", "mistral-large", true, true, false},
		{"codestral", "codestral", true, false, false},
		{"devstral", "devstral", true, false, false},
		{"phi-4", "phi-4", true, true, false},
		{"llama-4", "llama-4", true, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InferCapabilities(tt.modelID)
			if got == nil {
				t.Fatalf("InferCapabilities(%q) returned nil", tt.modelID)
			}

			if got.SupportsToolCalls != tt.wantToolCalls {
				t.Errorf("InferCapabilities(%q).SupportsToolCalls = %v, want %v", tt.modelID, got.SupportsToolCalls, tt.wantToolCalls)
			}
			if got.SupportsReasoning != tt.wantReasoning {
				t.Errorf("InferCapabilities(%q).SupportsReasoning = %v, want %v", tt.modelID, got.SupportsReasoning, tt.wantReasoning)
			}
			if got.SupportsVision != tt.wantVision {
				t.Errorf("InferCapabilities(%q).SupportsVision = %v, want %v", tt.modelID, got.SupportsVision, tt.wantVision)
			}
		})
	}
}
