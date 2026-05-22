package discovery

import (
	"testing"
)

func TestOpenAIParser_StandardFormat(t *testing.T) {
	body := []byte(`{
		"data": [
			{"id": "gpt-4"},
			{"id": "gpt-3.5-turbo"},
			{"id": "text-davinci-003"}
		]
	}`)

	parser := &OpenAIParser{}
	models, err := parser.Parse(body, "openai")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4" {
		t.Fatalf("expected first model id gpt-4, got %s", models[0].ID)
	}
	if models[0].Provider != "openai" {
		t.Fatalf("expected provider openai, got %s", models[0].Provider)
	}
}

func TestOpenAIParser_PlainStringArray(t *testing.T) {
	body := []byte(`[
		"gpt-4",
		"gpt-3.5-turbo",
		"text-davinci-003"
	]`)

	parser := &OpenAIParser{}
	models, err := parser.Parse(body, "custom-provider")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4" {
		t.Fatalf("expected first model id gpt-4, got %s", models[0].ID)
	}
	if models[0].Provider != "custom-provider" {
		t.Fatalf("expected provider custom-provider, got %s", models[0].Provider)
	}
}

func TestOpenAIParser_PlainObjectArray(t *testing.T) {
	body := []byte(`[
		{"id": "gpt-4"},
		{"id": "gpt-3.5-turbo"},
		{"id": "text-davinci-003"}
	]`)

	parser := &OpenAIParser{}
	models, err := parser.Parse(body, "another-provider")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].ID != "gpt-4" {
		t.Fatalf("expected first model id gpt-4, got %s", models[0].ID)
	}
}

func TestOpenAIParser_EmptyArray(t *testing.T) {
	body := []byte(`[]`)

	parser := &OpenAIParser{}
	models, err := parser.Parse(body, "provider")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("expected 0 models, got %d", len(models))
	}
}

func TestOpenAIParser_InvalidJSON(t *testing.T) {
	body := []byte(`not valid json`)

	parser := &OpenAIParser{}
	_, err := parser.Parse(body, "provider")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestOpenAIParser_FiltersEmptyIDs(t *testing.T) {
	body := []byte(`[
		{"id": "gpt-4"},
		{"id": ""},
		{"id": "gpt-3.5-turbo"}
	]`)

	parser := &OpenAIParser{}
	models, err := parser.Parse(body, "provider")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models (filtered empty id), got %d", len(models))
	}
}

func TestOpenAIParser_FiltersInvalidIDs(t *testing.T) {
	body := []byte(`{
		"data": [
			{"id": "valid-model"},
			{"id": "\u0000"},
			{"id": "also-valid"}
		]
	}`)

	parser := &OpenAIParser{}
	models, err := parser.Parse(body, "provider")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models (filtered invalid id), got %d", len(models))
	}
}

func TestExtractContextLength(t *testing.T) {
	tests := []struct {
		name     string
		modelInfo map[string]any
		want     int
	}{
		{
			name:     "gemma4 context length",
			modelInfo: map[string]any{"gemma4.context_length": 131072},
			want:     131072,
		},
		{
			name:     "llama context length",
			modelInfo: map[string]any{"llama.context_length": 4096},
			want:     4096,
		},
		{
			name:     "no context length",
			modelInfo: map[string]any{"foo": "bar"},
			want:     0,
		},
		{
			name:     "zero context length",
			modelInfo: map[string]any{"gemma4.context_length": 0},
			want:     0,
		},
		{
			name:     "float64 context length",
			modelInfo: map[string]any{"gemma4.context_length": 131072.0},
			want:     131072,
		},
		{
			name:     "multiple context lengths",
			modelInfo: map[string]any{"gemma4.context_length": 131072, "llama.context_length": 4096},
			want:     131072,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractContextLength(tt.modelInfo)
			if got != tt.want {
				t.Errorf("extractContextLength() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestExtractHasEmbeddings(t *testing.T) {
	tests := []struct {
		name     string
		modelInfo map[string]any
		want     bool
	}{
		{
			name:     "has embedding length",
			modelInfo: map[string]any{"nomic-embed-text.embedding_length": 768},
			want:     true,
		},
		{
			name:     "no embedding length",
			modelInfo: map[string]any{"gemma4.context_length": 131072},
			want:     false,
		},
		{
			name:     "empty model info",
			modelInfo: map[string]any{},
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractHasEmbeddings(tt.modelInfo)
			if got != tt.want {
				t.Errorf("extractHasEmbeddings() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMapOllamaCaps(t *testing.T) {
	tests := []struct {
		name string
		caps []string
		want []Capability
	}{
		{
			name: "vision tools reasoning",
			caps: []string{"vision", "tools", "thinking"},
			want: []Capability{CapReasoning, CapToolCalls, CapVision},
		},
		{
			name: "audio only",
			caps: []string{"audio"},
			want: []Capability{CapAudio},
		},
		{
			name: "empty",
			caps: []string{},
			want: nil,
		},
		{
			name: "unknown caps ignored",
			caps: []string{"vision", "unknown", "tools"},
			want: []Capability{CapToolCalls, CapVision},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapOllamaCaps(tt.caps)
			if !equalCapabilities(got, tt.want) {
				t.Errorf("mapOllamaCaps() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMapOllamaServiceKinds(t *testing.T) {
	tests := []struct {
		name          string
		caps          []string
		hasEmbeddings bool
		want          []string
	}{
		{
			name:          "completion only",
			caps:          []string{"completion"},
			hasEmbeddings: false,
			want:          []string{"llm"},
		},
		{
			name:          "audio",
			caps:          []string{"completion", "audio"},
			hasEmbeddings: false,
			want:          []string{"llm", "tts", "stt"},
		},
		{
			name:          "embedding only",
			caps:          []string{},
			hasEmbeddings: true,
			want:          []string{"embedding"},
		},
		{
			name:          "completion and embedding",
			caps:          []string{"completion"},
			hasEmbeddings: true,
			want:          []string{"llm", "embedding"},
		},
		{
			name:          "no completion no embedding",
			caps:          []string{},
			hasEmbeddings: false,
			want:          nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapOllamaServiceKinds(tt.caps, tt.hasEmbeddings)
			if !equalStringSlices(got, tt.want) {
				t.Errorf("mapOllamaServiceKinds() = %v, want %v", got, tt.want)
			}
		})
	}
}

func equalCapabilities(a, b []Capability) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
