package proxy

import (
	"strings"
	"testing"

	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
)

func imageResult(mime, data string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "text", Text: "chart follows"},
		{Type: "image", MimeType: mime, Data: data},
	}}
}

func TestMCPVisionGateAccept(t *testing.T) {
	cases := []struct {
		name   string
		gate   mcpVisionGate
		mime   string
		want   bool
		reason string
	}{
		{"zero gate drops everything", mcpVisionGate{}, "image/png", false, "no_target_context"},
		{"visionless model", mcpVisionGate{resolved: true, vision: false, format: "openai"}, "image/png", false, "model_no_vision"},
		{"vision model, accepted media", mcpVisionGate{resolved: true, vision: true, format: "openai"}, "image/png", true, ""},
		{"vision model, rejected media", mcpVisionGate{resolved: true, vision: true, format: "openai"}, "image/svg+xml", false, "format_media"},
		{"remote URL on anthropic", mcpVisionGate{resolved: true, vision: true, format: "anthropic"}, "", false, "format_remote_url"},
		{"remote URL on openai", mcpVisionGate{resolved: true, vision: true, format: "openai"}, "", true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := tc.gate.accept(tc.mime)
			if ok != tc.want || reason != tc.reason {
				t.Fatalf("accept(%q) = (%v, %q), want (%v, %q)", tc.mime, ok, reason, tc.want, tc.reason)
			}
		})
	}
}

// An accepted image rides as an OpenAI image part next to the tool text.
func TestToolResultContentKeepsSupportedImage(t *testing.T) {
	gate := mcpVisionGate{resolved: true, vision: true, format: "openai", model: "gpt-4o"}
	content, drops := toolResultContent("chart follows", imageResult("image/png", "AAAA"), gate, nil)
	if len(drops) != 0 {
		t.Fatalf("no drops expected: %+v", drops)
	}
	parts, ok := content.([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("want a text part and an image part, got %+v", content)
	}
	img, _ := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("the image part must be image_url: %+v", parts[1])
	}
	url, _ := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("the image must be a data URL: %q", url)
	}
}

// A rejected image is dropped, counted by the caller, and the omission is
// visible in the tool text (never a silent loss, never a hard 400).
func TestToolResultContentDropsUnsupportedImage(t *testing.T) {
	gate := mcpVisionGate{resolved: true, vision: false, format: "openai", model: "glm-4.5-text-only"}
	metrics := &infra.Metrics{}
	content, drops := toolResultContent("chart follows", imageResult("image/png", "AAAA"), gate, nil)
	if len(drops) != 1 || drops[0].reason != "model_no_vision" {
		t.Fatalf("want one model_no_vision drop: %+v", drops)
	}
	recordVisionDrops(metrics, drops)
	// No image survived: the legacy string shape with the omission noted.
	body, isStr := content.(string)
	if !isStr {
		t.Fatalf("a fully-dropped result must keep the string shape: %+v", content)
	}
	if !strings.Contains(body, "chart follows") || !strings.Contains(body, "[image omitted") {
		t.Fatalf("the tool text must keep the result and note the omission: %q", body)
	}
	var sb strings.Builder
	metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_vision_content_dropped_total{reason="model_no_vision", site="tool_result"} 1`) {
		t.Fatalf("the drop must be counted:\n%s", sb.String())
	}
}

// A result whose ONLY content is an image, and where every image is dropped,
// keeps the plain string shape with the visible omission marker.
func TestToolResultContentImageOnlyOmission(t *testing.T) {
	gate := mcpVisionGate{resolved: true, vision: false, format: "openai"}
	content, drops := toolResultContent("", &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image", MimeType: "image/png", Data: "AAAA"},
	}}, gate, nil)
	if len(drops) != 1 {
		t.Fatalf("want one drop: %+v", drops)
	}
	text, ok := content.(string)
	if !ok || text == "" {
		t.Fatalf("a fully-dropped result must keep the string shape with a marker: %+v", content)
	}
	if !strings.Contains(text, "[image omitted") {
		t.Fatalf("the marker must be visible: %q", text)
	}
}

// Malformed image blocks (missing data or media type) are dropped and counted,
// never silently lost.
func TestToolResultContentMalformedImage(t *testing.T) {
	gate := mcpVisionGate{resolved: true, vision: true, format: "openai"}
	content, drops := toolResultContent("look", &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image"},
		{Type: "text", Text: "look"},
	}}, gate, nil)
	if len(drops) != 1 || drops[0].reason != "malformed_image" {
		t.Fatalf("want one malformed_image drop: %+v", drops)
	}
	if s, ok := content.(string); !ok || !strings.Contains(s, "[image omitted") {
		t.Fatalf("the malformed image must be noted: %+v", content)
	}
}

// An oversized image is dropped with the size_limit reason.
func TestToolResultContentOversizedImage(t *testing.T) {
	gate := mcpVisionGate{resolved: true, vision: true, format: "openai"}
	content, drops := toolResultContent("", &mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image", MimeType: "image/png", Data: strings.Repeat("A", (4<<20)+1)},
	}}, gate, nil)
	if len(drops) != 1 || drops[0].reason != "size_limit" {
		t.Fatalf("want one size_limit drop: %+v", drops)
	}
	if s, ok := content.(string); !ok || !strings.Contains(s, "[image omitted") {
		t.Fatalf("the oversized image must be noted: %+v", content)
	}
}

// A text-only result keeps its plain-string content: the pre-NENYA-149 shape.
func TestToolResultContentTextOnlyUnchanged(t *testing.T) {
	gate := mcpVisionGate{resolved: true, vision: true, format: "openai"}
	content, drops := toolResultContent("plain", &mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: "plain"}}}, gate, nil)
	if len(drops) != 0 {
		t.Fatalf("no drops expected: %+v", drops)
	}
	if s, ok := content.(string); !ok || s != "plain" {
		t.Fatalf("a text-only result must keep its string shape: %+v", content)
	}
}

// The gate resolves the vision capability from the catalog metadata first.
func TestNewMCPVisionGateResolvesCapability(t *testing.T) {
	catalog := discovery.NewModelCatalog()
	catalog.Add(discovery.DiscoveredModel{ID: "gpt-4o", Metadata: &discovery.ModelMetadata{SupportsVision: true}})

	gate := newMCPVisionGate(catalog, []routing.UpstreamTarget{{Model: "gpt-4o", Format: "openai"}})
	if !gate.resolved || !gate.vision || gate.format != "openai" {
		t.Fatalf("a vision-capable model must resolve an accepting gate: %+v", gate)
	}

	// No targets at all: the gate is unresolved and fail-closed.
	if got := newMCPVisionGate(catalog, nil); got.resolved {
		t.Fatalf("no target context must leave the gate unresolved: %+v", got)
	}
}

// Append-level: with a RESOLVED vision gate, an image-bearing result lands as
// a content array (text + image_url) in the tool message, the spotlight step
// envelopes only the text part, and the spotlight counter fires once — while a
// zero gate drops the image and keeps the string shape.
func TestAppendMCPResultsGatesImages(t *testing.T) {
	payload := func() map[string]any {
		return map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": "hi"},
		}}
	}
	calls := []mcpToolCall{{ID: "call_1", Name: "mcp__mem__chart"}}
	assistant := map[string]any{"role": "assistant", "content": nil}
	results := []*mcp.CallToolResult{imageResult("image/png", "AAAA")}

	// Vision-capable gate: the array with the image survives into the message.
	visionPayload := payload()
	appendMCPResults(visionPayload, calls, results, assistant, mcpAppendContext{
		vision: mcpVisionGate{resolved: true, vision: true, format: "openai", model: "gpt-4o"},
	})
	toolMsg := visionPayload["messages"].([]any)[2].(map[string]any)
	parts, ok := toolMsg["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("the accepted image must ride as a content array: %+v", toolMsg["content"])
	}
	if parts[0].(map[string]any)["type"] != "text" || parts[1].(map[string]any)["type"] != "image_url" {
		t.Fatalf("want text then image_url: %+v", parts)
	}

	// Spotlight over the array: only the text part is enveloped.
	settings := pipeline.SpotlightSettings{Enabled: true, MaxToolResultBytes: 1 << 20}
	content, enveloped := applySpotlightToParts(parts, settings, "mem", "chart")
	if _, isArr := content.([]any); !isArr || !enveloped {
		t.Fatal("applySpotlightToParts must keep the array and report enveloping")
	}
	if text := parts[0].(map[string]any)["text"].(string); !strings.Contains(text, "untrusted") {
		t.Fatalf("the text part must be enveloped: %q", text)
	}
	if _, isImage := parts[1].(map[string]any)["image_url"]; !isImage {
		t.Fatal("the image part must be untouched by the spotlight")
	}

	// Zero gate (no target context): fail-closed drop; no image survives, so
	// the tool message keeps the string shape with the omission visible.
	legacyPayload := payload()
	appendMCPResults(legacyPayload, calls, results, assistant, mcpAppendContext{})
	legacyMsg := legacyPayload["messages"].([]any)[2].(map[string]any)
	text, ok := legacyMsg["content"].(string)
	if !ok {
		t.Fatalf("the zero gate must fall back to the string shape: %+v", legacyMsg["content"])
	}
	if !strings.Contains(text, "[image omitted") {
		t.Fatalf("the omission must be visible: %q", text)
	}
}
