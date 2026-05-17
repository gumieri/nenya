package providers

import "testing"

func TestProviderSpecSupports(t *testing.T) {
	spec := ProviderSpec{
		ServiceKinds: []ServiceKind{ServiceKindLLM, ServiceKindEmbedding},
	}

	if !spec.Supports(ServiceKindLLM) {
		t.Error("Expected Supports(ServiceKindLLM) to return true")
	}
	if !spec.Supports(ServiceKindEmbedding) {
		t.Error("Expected Supports(ServiceKindEmbedding) to return true")
	}
	if spec.Supports(ServiceKindTTS) {
		t.Error("Expected Supports(ServiceKindTTS) to return false")
	}
}