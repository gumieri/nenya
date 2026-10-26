package routing

import (
	"strings"
	"testing"

	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
)

func visionDeps(t *testing.T) TransformDeps {
	t.Helper()
	return TransformDeps{Metrics: &infra.Metrics{}, Catalog: discovery.NewModelCatalog()}
}

func imageMsg(url string) map[string]interface{} {
	return map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "look at this"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}},
		},
	}
}

func TestFormatAcceptsImageMedia(t *testing.T) {
	cases := []struct {
		format, mime string
		want         bool
	}{
		{"openai", "image/png", true},
		{"openai", "image/jpeg", true},
		{"openai", "image/webp", true},
		{"openai", "image/heic", false},
		{"anthropic", "image/png", true},
		{"anthropic", "image/svg+xml", false},
		{"gemini", "image/heic", true},
		{"gemini", "image/gif", false}, // Gemini carries png/jpeg/webp/heic/heif
		{"gemini", "image/svg+xml", false},
		{"", "image/png", true},  // unknown format: conservative default
		{"", "image/gif", false}, // unknown format: conservative default
		{"mystery", "image/jpeg", true},
		{"openai", "IMAGE/PNG", true}, // media types are case-insensitive
		{"openai", "", false},
	}
	for _, tc := range cases {
		if got := FormatAcceptsImageMedia(tc.format, tc.mime); got != tc.want {
			t.Fatalf("FormatAcceptsImageMedia(%q, %q) = %v, want %v", tc.format, tc.mime, got, tc.want)
		}
	}
}

func TestImageMimeFromDataURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"data:image/png;base64,AAAA", "image/png"},
		{"data:image/jpeg;base64,/9j/4A==", "image/jpeg"},
		{"https://example.com/x.png", ""},
		{"data:image/png,AAAA", ""}, // not base64: unknown to us
		{"", ""},
	}
	for _, tc := range cases {
		if got := imageMimeFromDataURL(tc.url); got != tc.want {
			t.Fatalf("imageMimeFromDataURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

// A vision-less model loses every image part; the array keeps its text and the
// drop is counted (never a hard 400, never a silent loss).
func TestDropUnsupportedVisionContent_VisionlessModel(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{imageMsg("data:image/png;base64,AAAA")}}

	if dropped := DropUnsupportedVisionContent(deps, payload, "glm-4.5-text-only", "openai"); dropped != 1 {
		t.Fatalf("want 1 dropped image, got %d", dropped)
	}
	parts := payload["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("the text part must survive: %+v", parts)
	}
	if parts[0].(map[string]interface{})["type"] != "text" {
		t.Fatalf("the surviving part must be text: %+v", parts[0])
	}
}

// A vision-capable model keeps a supported media type...
func TestDropUnsupportedVisionContent_VisionModelKeepsImage(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{imageMsg("data:image/png;base64,AAAA")}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "gpt-4o", "openai"); dropped != 0 {
		t.Fatalf("a vision model with an accepted media type must keep the image, dropped %d", dropped)
	}
	parts := payload["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if len(parts) != 2 {
		t.Fatalf("both parts must survive: %+v", parts)
	}
}

// ...but loses one whose media type the format rejects.
func TestDropUnsupportedVisionContent_UnsupportedMediaType(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{imageMsg("data:image/svg+xml;base64,AAAA")}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "gpt-4o", "openai"); dropped != 1 {
		t.Fatalf("an unsupported media type must be dropped, got %d", dropped)
	}
}

// An image-only content array must never become an empty array: the omission
// marker keeps it valid for providers that reject empty content.
func TestDropUnsupportedVisionContent_ImageOnlyBecomesMarker(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}},
		},
	}}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "glm-4.5-text-only", "openai"); dropped != 1 {
		t.Fatalf("want 1 dropped image, got %d", dropped)
	}
	parts := payload["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	if len(parts) != 1 {
		t.Fatalf("the array must not be left empty: %+v", parts)
	}
	text, _ := parts[0].(map[string]interface{})["text"].(string)
	if text == "" {
		t.Fatalf("the omission marker must be visible: %+v", parts[0])
	}
}

// TestDropUnsupportedVisionContent_MarkerNamesTheReason pins AUD-80
// (NENYA-284): the omission marker names the ACTUAL drop reason — a
// vision-less model's marker says the model has no vision, not the generic
// "not supported by the routed model or format". Pre-fix every emptied
// array carried the same generic string.
func TestDropUnsupportedVisionContent_MarkerNamesTheReason(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}},
		},
	}}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "glm-4.5-text-only", "openai"); dropped != 1 {
		t.Fatalf("want 1 dropped image, got %d", dropped)
	}
	parts := payload["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	text, _ := parts[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text, "no vision capability") {
		t.Fatalf("the marker must name the vision reason, got %q", text)
	}
	if strings.Contains(text, "not supported by the routed model or format") {
		t.Fatalf("the generic fallback must not be used when the reason is known: %q", text)
	}

	// A malformed image (no URL, no media type) on a vision-capable model
	// names the malformed reason instead.
	payload2 := map[string]interface{}{"messages": []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{}},
		},
	}}}
	if dropped := DropUnsupportedVisionContent(deps, payload2, "gpt-4o", "openai"); dropped != 1 {
		t.Fatalf("want 1 dropped malformed image, got %d", dropped)
	}
	parts2 := payload2["messages"].([]interface{})[0].(map[string]interface{})["content"].([]interface{})
	text2, _ := parts2[0].(map[string]interface{})["text"].(string)
	if !strings.Contains(text2, "malformed") {
		t.Fatalf("the marker must name the malformed reason, got %q", text2)
	}
}

// An OpenAI-shaped remote image URL on an Anthropic target is unsendable
// (converter only carries data: URLs, and message content is copied verbatim)
// while an OpenAI target keeps it.
func TestDropUnsupportedVisionContent_RemoteURL(t *testing.T) {
	for _, tc := range []struct {
		format string
		want   int
	}{{"anthropic", 1}, {"openai", 0}} {
		deps := visionDeps(t)
		payload := map[string]interface{}{"messages": []interface{}{imageMsg("https://example.com/x.png")}}
		if dropped := DropUnsupportedVisionContent(deps, payload, "gpt-4o", tc.format); dropped != tc.want {
			t.Fatalf("format %s: want %d dropped, got %d", tc.format, tc.want, dropped)
		}
	}
}

// An Anthropic-NATIVE image part in message content (a /v1/messages client's
// url-source image, kept in native shape by the ingress) round-trips verbatim
// and must SURVIVE the gate on an anthropic-format target.
func TestDropUnsupportedVisionContent_NativeAnthropicBlockSurvives(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type":   "image",
				"source": map[string]interface{}{"type": "url", "url": "https://example.com/x.png"},
			},
		},
	}}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "claude-4-sonnet", "anthropic"); dropped != 0 {
		t.Fatalf("a native url-source image must survive on the anthropic format, dropped %d", dropped)
	}
	// ...but a native block with a media type the format rejects is dropped.
	payload["messages"] = []interface{}{map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type":   "image",
				"source": map[string]interface{}{"type": "base64", "media_type": "image/svg+xml", "data": "AA"},
			},
		},
	}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "claude-4-sonnet", "anthropic"); dropped != 1 {
		t.Fatalf("a bad native media type must be dropped, got %d", dropped)
	}
}

// Tool-message images are gated at the tool_result site: the Anthropic
// tool-content converter carries them, so the format_shape message rule must
// not strip what the MCP gate deliberately accepted.
func TestDropUnsupportedVisionContent_ToolMessagesUseToolSite(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{map[string]interface{}{
		"role":    "tool",
		"content": []interface{}{map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}}},
	}}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "claude-4-sonnet", "anthropic"); dropped != 0 {
		t.Fatalf("a tool message image must ride the anthropic tool-content converter, dropped %d", dropped)
	}
	// ...and its drops report the tool_result site.
	payload["messages"] = []interface{}{map[string]interface{}{
		"role":    "tool",
		"content": []interface{}{map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}}},
	}}
	DropUnsupportedVisionContent(deps, payload, "glm-4.5-text-only", "anthropic")
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_vision_content_dropped_total{reason="model_no_vision", site="tool_result"} 1`) {
		t.Fatalf("a tool-message drop must report the tool_result site:\n%s", sb.String())
	}
}

// Message-content images on the Anthropic FORMAT are unsendable: the adapter
// copies message content parts verbatim (hard 400) and converts only
// tool-result content, so the gate drops them up front.
func TestDropUnsupportedVisionContent_AnthropicMessageShape(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{imageMsg("data:image/png;base64,AAAA")}}
	if dropped := DropUnsupportedVisionContent(deps, payload, "claude-4-sonnet", "anthropic"); dropped != 1 {
		t.Fatalf("an anthropic target cannot carry a message-content image, dropped %d", dropped)
	}
	var sb strings.Builder
	deps.Metrics.WritePrometheus(&sb)
	if !strings.Contains(sb.String(), `nenya_vision_content_dropped_total{reason="format_shape", site="request"} 1`) {
		t.Fatalf("the format_shape drop must be counted:\n%s", sb.String())
	}
}

// An Anthropic-native URL-source image part (no media_type) is classified by
// shape as remote, not as an unknown media type.
func TestTypeAnyImageClassifiesSourceShape(t *testing.T) {
	ref, ok := typeAnyImage(map[string]interface{}{
		"type":   "image",
		"source": map[string]interface{}{"type": "url", "url": "https://x/y.png"},
	})
	if !ok || !ref.remote || ref.mime != "" {
		t.Fatalf("a url-source image must classify as remote: %+v ok=%v", ref, ok)
	}
	ref, ok = typeAnyImage(map[string]interface{}{
		"type":   "image",
		"source": map[string]interface{}{"type": "base64", "media_type": "image/png", "data": "AA"},
	})
	if !ok || ref.remote || ref.mime != "image/png" {
		t.Fatalf("a base64-source image must classify by media_type: %+v ok=%v", ref, ok)
	}
}

// The counter records the drop with the site and reason labels.
func TestVisionDropCounter(t *testing.T) {
	deps := visionDeps(t)
	payload := map[string]interface{}{"messages": []interface{}{imageMsg("data:image/png;base64,AAAA")}}
	DropUnsupportedVisionContent(deps, payload, "glm-4.5-text-only", "openai")
	var out strings.Builder
	deps.Metrics.WritePrometheus(&out)
	if !strings.Contains(out.String(), `nenya_vision_content_dropped_total{reason="model_no_vision", site="request"} 1`) {
		t.Fatalf("the drop must be counted:\n%s", out.String())
	}
}
