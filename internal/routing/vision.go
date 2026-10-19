package routing

import (
	"strings"

	"github.com/nenya/internal/discovery"
)

// ---- NENYA-149: capability-gated image blocks ------------------------------
//
// Image blocks in tool results and message content are gated on the routed
// model's vision capability and the target wire format's accepted image media
// types, so vision-less targets stop taking hard 400s. Unsupported blocks are
// DROPPED with a telemetry counter and a debug log — never failing the
// request.

// imageMediaByFormat maps a target wire format to the image media types it can
// carry. The gateway sends the OpenAI shape for openai/gemini targets (the
// latter through providers' OpenAI-compatible surfaces). Anthropic tool-result
// content is converted to base64 sources (png/jpeg/gif/webp).
var imageMediaByFormat = map[string]map[string]bool{
	"openai": {
		"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	},
	"anthropic": {
		"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true,
	},
	"gemini": {
		"image/png": true, "image/jpeg": true, "image/webp": true,
		"image/heic": true, "image/heif": true,
	},
}

// defaultImageMedia is the near-universal pair for unknown/empty formats.
var defaultImageMedia = map[string]bool{
	"image/png": true, "image/jpeg": true,
}

// FormatAcceptsImageMedia reports whether the target wire format can carry an
// image of the given media type. Unknown formats are treated conservatively
// (the near-universal pair). An unknown media type is never accepted.
func FormatAcceptsImageMedia(format, mime string) bool {
	table := imageMediaByFormat[strings.ToLower(format)]
	if table == nil {
		table = defaultImageMedia
	}
	return table[strings.ToLower(mime)]
}

// FormatAcceptsRemoteImageURL reports whether the target format's conversion
// carries a non-data image URL (http/https). Anthropic's converter only
// transforms data: URLs, so a remote URL would be silently dropped there —
// the gate rejects it up front instead.
func FormatAcceptsRemoteImageURL(format string) bool {
	return strings.ToLower(format) != "anthropic"
}

// formatCarriesMessageImages reports whether the target format's converter
// carries image parts inside MESSAGE content (not tool-result content). The
// Anthropic conversion copies message content parts verbatim and converts only
// tool-result content, so an OpenAI-shaped image part on that format is
// unsendable and the gate drops it up front.
func formatCarriesMessageImages(format string) bool {
	return strings.ToLower(format) != "anthropic"
}

// SupportsVision reports whether the routed model carries the vision
// capability (catalog metadata first, then the ID-based inference).
func SupportsVision(deps TransformDeps, modelName string) bool {
	return modelSupportsCapability(deps, modelName, discovery.CapVision)
}

// VisionContentDropped marks one image block the gate removed.
type VisionContentDropped struct {
	// Site is where the image block was found: "request" (message content)
	// or "tool_result" (an MCP tool-result content).
	Site string
	// Reason is why it was dropped: model_no_vision, format_media,
	// format_remote_url, format_shape, malformed_image, size_limit, or
	// no_target_context.
	Reason string
	// MimeType is the image's media type ("" for remote URLs).
	MimeType string
}

// RecordVisionDrop records one dropped image block. Nil-safe.
func (deps TransformDeps) RecordVisionDrop(drop VisionContentDropped) {
	deps.Metrics.RecordVisionContentDropped(drop.Site, drop.Reason)
	if deps.Logger != nil {
		deps.Logger.Debug("dropped image block unsupported by the routed model or format",
			"site", drop.Site, "reason", drop.Reason, "media_type", drop.MimeType)
	}
}

// imageRef classifies one image-bearing content part: its kind, its media type
// ("" when the image is a remote URL or the type is unknown), and whether it
// references a remote URL rather than inline data.
type imageRef struct {
	kind   string // image_url | input_image | image
	mime   string
	remote bool
}

// typeAnyImage reports whether a content part is an image block, classifying
// it by SHAPE: a `source` block with type "url" is remote regardless of the
// absent media_type, and an `image_url` with a non-data URL is remote.
func typeAnyImage(part map[string]interface{}) (imageRef, bool) {
	t, _ := part["type"].(string)
	switch t {
	case "image_url", "input_image":
		switch iu := part["image_url"].(type) {
		case map[string]interface{}:
			url, _ := iu["url"].(string)
			if mime := imageMimeFromDataURL(url); mime != "" {
				return imageRef{kind: t, mime: mime}, true
			}
			if url != "" {
				return imageRef{kind: t, remote: true}, true
			}
			return imageRef{kind: t}, true
		case string:
			if mime := imageMimeFromDataURL(iu); mime != "" {
				return imageRef{kind: t, mime: mime}, true
			}
			return imageRef{kind: t, remote: true}, true
		}
		return imageRef{kind: t}, true
	case "image":
		if src, ok := part["source"].(map[string]interface{}); ok {
			srcType, _ := src["type"].(string)
			if srcType == "url" {
				return imageRef{kind: t, remote: true}, true
			}
			mt, _ := src["media_type"].(string)
			return imageRef{kind: t, mime: mt}, true
		}
		if mt, ok := part["mimeType"].(string); ok {
			return imageRef{kind: t, mime: mt}, true
		}
		return imageRef{kind: t}, true
	default:
		return imageRef{}, false
	}
}

// imageMimeFromDataURL extracts the media type from a data: URL ("image/png"
// in "data:image/png;base64,..."), or "" for anything else (remote URLs,
// malformed values).
func imageMimeFromDataURL(url string) string {
	idx := strings.Index(url, ";base64,")
	if !strings.HasPrefix(url, "data:") || idx < 0 {
		return ""
	}
	return strings.TrimPrefix(url[:idx], "data:")
}

// nativeAnthropicDropReason reports why an Anthropic-NATIVE image part in
// message content must be dropped ("" = keep): the block rides verbatim to an
// anthropic-format target (the ingress keeps non-base64 sources in native
// shape and the adapter copies message content parts verbatim), so only a
// known-bad media type is dropped.
func nativeAnthropicDropReason(format string, ref imageRef) string {
	if ref.mime != "" && !FormatAcceptsImageMedia(format, ref.mime) {
		return "format_media"
	}
	return ""
}

// visionDropReason reports why an image part must be dropped ("" = keep).
func visionDropReason(site string, vision bool, format string, ref imageRef) string {
	if ref.kind == "" {
		return ""
	}
	if !vision {
		return "model_no_vision"
	}
	if site == "request" {
		if ref.kind == "image" && format == "anthropic" {
			return nativeAnthropicDropReason(format, ref)
		}
		if !formatCarriesMessageImages(format) {
			return "format_shape"
		}
	}
	if ref.remote {
		if FormatAcceptsRemoteImageURL(format) {
			return ""
		}
		return "format_remote_url"
	}
	if FormatAcceptsImageMedia(format, ref.mime) {
		return ""
	}
	return "format_media"
}

// DropUnsupportedVisionContent walks the payload's message content arrays and
// removes image parts the routed model or format cannot carry (NENYA-149):
// a model without the vision capability loses every image, and an image whose
// media type the format rejects loses just that block. A content array left
// empty becomes a single visible "[image omitted: ...]" text part — never a
// bare empty array, which providers reject. Returns the number of images
// dropped; every drop is counted and debug-logged, never failing the request.
func DropUnsupportedVisionContent(deps TransformDeps, payload map[string]interface{}, modelName, format string) int {
	messages, ok := payload["messages"].([]interface{})
	if !ok {
		return 0
	}
	vision := SupportsVision(deps, modelName)
	dropped := 0
	for i := range messages {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		parts, ok := msg["content"].([]interface{})
		if !ok {
			continue
		}
		// The message ROLE picks the gate site: a tool message's images are
		// carried by the Anthropic tool-content converter (site tool_result),
		// while message-content images on that format are copied verbatim and
		// hard-400 (site request, reason format_shape).
		site := "request"
		if role, _ := msg["role"].(string); role == "tool" {
			site = "tool_result"
		}
		kept, n := keepSupportedVisionParts(deps, parts, site, vision, format)
		if n == 0 {
			continue
		}
		dropped += n
		msg["content"] = kept
	}
	return dropped
}

// keepSupportedVisionParts filters one content array, returning the kept parts
// and the number of images dropped. An array emptied by the filter keeps a
// visible omission marker instead.
func keepSupportedVisionParts(deps TransformDeps, parts []interface{}, site string, vision bool, format string) ([]interface{}, int) {
	kept := make([]interface{}, 0, len(parts))
	dropped := 0
	for _, rawPart := range parts {
		part, ok := rawPart.(map[string]interface{})
		if !ok {
			kept = append(kept, rawPart)
			continue
		}
		ref, isImage := typeAnyImage(part)
		if !isImage {
			kept = append(kept, rawPart)
			continue
		}
		reason := visionDropReason(site, vision, format, ref)
		if reason == "" {
			kept = append(kept, rawPart)
			continue
		}
		dropped++
		deps.RecordVisionDrop(VisionContentDropped{Site: site, Reason: reason, MimeType: ref.mime})
	}
	if dropped > 0 && len(kept) == 0 {
		kept = append(kept, map[string]interface{}{
			"type": "text", "text": "[image omitted: not supported by the routed model or format]",
		})
	}
	return kept, dropped
}
