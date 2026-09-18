package proxy

import "testing"

// TestCapturedStreamHasRefusalOrFilter pins the decode-based replacement for
// the loose serialized-JSON substring match (NENYA-27): terminal outcomes are
// read from parsed fields, and content merely containing the words must not
// block caching.
func TestCapturedStreamHasRefusalOrFilter(t *testing.T) {
	tests := []struct {
		name     string
		captured string
		want     bool
	}{
		{"clean stop", "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", false},
		{"bare json stop", "{\"choices\":[{\"finish_reason\":\"stop\"}]}", false},
		{"openai refusal finish", "{\"choices\":[{\"finish_reason\":\"refusal\"}]}", true},
		{"openai content_filter finish", "{\"choices\":[{\"finish_reason\":\"content_filter\"}]}", true},
		{"anthropic stop_reason refusal", "{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"refusal\"}}", true},
		{"anthropic stop_reason content_filter", "{\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"content_filter\"}}", true},
		{"refusal delta field", "data: {\"choices\":[{\"delta\":{\"refusal\":\"I cannot help with that\"}}]}\n\n", true},
		{"choice delta stop_reason", "{\"choices\":[{\"delta\":{\"stop_reason\":\"refusal\"}}]}", true},
		{"user content mentioning refusal", "data: {\"choices\":[{\"delta\":{\"content\":\"the word refusal appears here\"}}]}\n\ndata: [DONE]\n\n", false},
		{"user content mentioning content_filter", "{\"choices\":[{\"delta\":{\"content\":\"content_filter docs\"}}]}", false},
		{"not json ignored", "data: not-json\n\ndata: [DONE]\n\n", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := capturedStreamHasRefusalOrFilter([]byte(tt.captured)); got != tt.want {
				t.Errorf("capturedStreamHasRefusalOrFilter(%q) = %v, want %v", tt.captured, got, tt.want)
			}
		})
	}
}
