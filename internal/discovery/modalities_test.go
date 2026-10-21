package discovery

import (
	"testing"
)

// Advertised modalities are structured capability data: they must reach
// ModelMetadata (which the resolver consults before the ID heuristics), not
// depend on the model ID containing "vision".
func TestOpenAIParser_MergesAdvertisedModalities(t *testing.T) {
	body := []byte(`{"data": [
		{"id": "meta-llama/llama-4-maverick", "context_length": 1048576,
		 "architecture": {"input_modalities": ["text", "Image"], "output_modalities": ["text"]}},
		{"id": "openai/audio-model", "architecture": {"input_modalities": ["text", "audio"]}},
		{"id": "plain-model"},
		{"id": "text-only", "context_length": "8192"},
		{"id": "openai/gpt-4o", "architecture": {"input_modalities": ["text"]}}
	]}`)
	models, err := (&OpenAIParser{}).Parse(body, "aggregator")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	byID := make(map[string]DiscoveredModel, len(models))
	for _, m := range models {
		byID[m.ID] = m
	}

	maverick := byID["meta-llama/llama-4-maverick"]
	if maverick.Metadata == nil || !maverick.Metadata.SupportsVision {
		t.Fatalf("an advertised image modality must set SupportsVision (case-insensitive): %+v", maverick.Metadata)
	}
	if maverick.MaxContext != 1048576 {
		t.Fatalf("the advertised context length must ride the model: %+v", maverick)
	}
	if maverick.Metadata.SupportsAudio {
		t.Fatalf("no audio modality was advertised: %+v", maverick.Metadata)
	}

	audio := byID["openai/audio-model"]
	if audio.Metadata == nil || !audio.Metadata.SupportsAudio || audio.Metadata.SupportsVision {
		t.Fatalf("an advertised audio modality must set SupportsAudio only: %+v", audio.Metadata)
	}

	// A modality-less entry must NOT carry metadata: an empty record would
	// mask the model-ID heuristics in the resolver.
	plain := byID["plain-model"]
	if plain.Metadata != nil {
		t.Fatalf("a modality-less entry must fall back to the heuristics: %+v", plain.Metadata)
	}
	if got := plain.HasCapability(CapVision); got != InferCapabilities("plain-model").HasCapability(CapVision) {
		t.Fatalf("heuristic inference should run for plain entries: vision=%v", got)
	}

	// Context length alone rides the model, not the metadata (and tolerates
	// the string form some aggregators ship).
	textOnly := byID["text-only"]
	if textOnly.MaxContext != 8192 || textOnly.Metadata != nil {
		t.Fatalf("context length rides MaxContext without masking heuristics: %+v", textOnly)
	}

	// MASKING GUARD: an aggregator advertising only ["text"] must NOT erase
	// the capabilities the model ID implies (tool_calls etc.) — the record is
	// seeded from the heuristics and the modalities overlay it.
	gpt4o := byID["openai/gpt-4o"]
	if gpt4o.Metadata == nil {
		t.Fatal("an advertised text-only modality must still produce a record")
	}
	heuristic := InferCapabilities("openai/gpt-4o")
	if !gpt4o.Metadata.SupportsVision {
		t.Fatalf("the ID heuristic's vision must survive the overlay: %+v", gpt4o.Metadata)
	}
	if gpt4o.Metadata.SupportsToolCalls != heuristic.SupportsToolCalls ||
		gpt4o.Metadata.SupportsReasoning != heuristic.SupportsReasoning ||
		gpt4o.Metadata.SupportsStreamOptions != heuristic.SupportsStreamOptions {
		t.Fatalf("the overlay must not lose the ID-inferred capabilities: got %+v want seed %+v", gpt4o.Metadata, heuristic)
	}
}

// Unknown modality spellings are ignored rather than guessed.
func TestMetadataFromAdvertisedUnknownSpellings(t *testing.T) {
	meta := metadataFromAdvertised("some-model", []string{"vector", "PDF"})
	if meta == nil {
		t.Fatal("advertised (unknown) modalities still produce a record")
	}
	if meta.SupportsVision || meta.SupportsAudio {
		t.Fatalf("unknown spellings must not be guessed into capabilities: %+v", meta)
	}
}

// Architecture present but with EMPTY modality lists must return nil — a
// non-nil record (even all-false) would mask the ID heuristics.
func TestMetadataFromAdvertisedEmptyLists(t *testing.T) {
	if meta := metadataFromAdvertised("gpt-4o", nil); meta != nil {
		t.Fatalf("empty modality lists must fall back to the heuristics: %+v", meta)
	}
}
