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
