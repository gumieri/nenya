package routing

import (
	"testing"

	"github.com/nenya/config"
)

// TestTransformWithNilLoggerDoesNotPanic pins AUD-38 (NENYA-246): a nil
// TransformDeps.Logger must degrade to silence, not panic mid-request. The
// transform and sanitize paths log best-effort at a dozen sites; pre-fix any
// of them dereferenced the nil logger directly. Two sites are driven
// deliberately: stripStreamOptions (a payload carrying stream_options on an
// OpenAI-shape target logs its strip) and the DeepSeek reasoning-injection
// fix (model prefix match).
func TestTransformWithNilLoggerDoesNotPanic(t *testing.T) {
	deps := TransformDeps{Config: &config.Config{}}
	payload := map[string]interface{}{
		"model":          "gpt-5.6",
		"messages":       []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"stream_options": map[string]interface{}{"include_usage": true},
	}

	SanitizePayload(deps, payload, "gpt-5.6", "openai")
	if _, ok := payload["stream_options"]; ok {
		t.Fatal("stream_options must be stripped for an OpenAI-shape target (the strip is the logged site under test)")
	}

	_, _, _ = TransformRequestForUpstream(deps, TransformParams{ProviderName: "prov", Model: "deepseek-v4", MaxOutput: 0, MaxContext: 0, Format: "openai", ReasoningEffort: ""}, payload)
}
