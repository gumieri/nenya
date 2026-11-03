package mcpproxy

import (
	"log/slog"

	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/mcp"
	"github.com/nenya/internal/routing"
)

// ---- NENYA-149: capability-gated tool-result image blocks ------------------
//
// An MCP tool result may carry image content (MCP content blocks of type
// "image"). Those images ride to the routed target ONLY when the target model
// has the vision capability and the target wire format accepts the image's
// media type. Everything else is dropped with a telemetry counter and a debug
// log, and the omission is made visible in the tool text — never a hard 400
// from a vision-less target, never a silent loss.

// NewMCPVisionGate resolves the gate from the request's primary target: the
// model's vision capability (catalog metadata first, then ID inference) and the
// target wire format. Targets beyond the primary one are not consulted: the
// gate is a conservative pre-filter, and a failover to a target with a
// different format re-runs the transform path, which applies the same rules
// (routing.DropUnsupportedVisionContent) on the request side.
func NewMCPVisionGate(catalog *discovery.ModelCatalog, targets []routing.UpstreamTarget) VisionGate {
	if len(targets) == 0 {
		return VisionGate{}
	}
	primary := targets[0]
	deps := routing.TransformDeps{Catalog: catalog}
	return VisionGate{
		Model:    primary.Model,
		Format:   primary.Format,
		Vision:   routing.SupportsVision(deps, primary.Model),
		Resolved: true,
	}
}

// Accept reports whether an image with the given media type ("" for a remote
// URL) can ride to the target, and the drop reason when it cannot.
func (g VisionGate) Accept(mime string) (bool, string) {
	if !g.Resolved {
		return false, "no_target_context"
	}
	if !g.Vision {
		return false, "model_no_vision"
	}
	if mime == "" {
		if routing.FormatAcceptsRemoteImageURL(g.Format) {
			return true, ""
		}
		return false, "format_remote_url"
	}
	if routing.FormatAcceptsImageMedia(g.Format, mime) {
		return true, ""
	}
	return false, "format_media"
}

// maxToolResultImageBytes caps one tool-result image's base64 payload: the
// MCP transport bounds a result to ~1 MiB, but N images must not turn into an
// N MiB request with no per-request budget.
const maxToolResultImageBytes = 4 << 20 // 4 MiB of base64

// toolResultContent builds the content of one tool-result message: the tool
// text, plus the result's image blocks as OpenAI image parts when the gate
// accepts them. Malformed image blocks (missing data or media type) and
// oversized ones are dropped exactly like gate-rejected images — counted and
// debug-logged, never silently lost — and the omission is appended to the
// text, so the model is never told a silently incomplete result is complete.
// A result with NO image blocks, or where no image SURVIVES, keeps the plain
// string shape (the pre-NENYA-149 shape, and the shape the spotlight byte cap
// knows how to bound).
func toolResultContent(text string, result *mcp.CallToolResult, gate mcpVisionGate, logger *slog.Logger) (any, []visionDrop) {
	images := result.Images()
	malformed := malformedImageCount(result)
	if len(images) == 0 && malformed == 0 {
		return text, nil
	}
	drops := classifyImageBlocks(malformed, gate, logger)
	kept := survivingImages(images, gate, &drops, logger)
	if len(kept) == 0 {
		// No image survived: keep the legacy string shape, with the omission
		// made visible.
		if text == "" {
			text = visionOmissionNote(drops)
		} else {
			text += "\n" + visionOmissionNote(drops)
		}
		return text, drops
	}
	parts := make([]any, 0, len(kept)+1)
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, img := range kept {
		parts = append(parts, map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + img.MimeType + ";base64," + img.Data,
			},
		})
	}
	if len(drops) > 0 {
		if parts[0].(map[string]any)["type"] == "text" {
			parts[0].(map[string]any)["text"] = text + "\n" + visionOmissionNote(drops)
		} else {
			parts = append([]any{map[string]any{"type": "text", "text": visionOmissionNote(drops)}}, parts...)
		}
	}
	return parts, drops
}

// classifyImageBlocks builds the drop records for the result's malformed image
// blocks (missing data or media type) — they are image INTENT the model should
// know was dropped.
func classifyImageBlocks(malformed int, gate mcpVisionGate, logger *slog.Logger) []visionDrop {
	var drops []visionDrop
	for range malformed {
		drops = append(drops, VisionDrop{Reason: "malformed_image"})
		if logger != nil {
			logger.Debug("dropped malformed tool-result image block", "model", gate.Model)
		}
	}
	return drops
}

// survivingImages returns the image blocks the gate accepts, appending a drop
// record for each one it rejects (oversized, or unsupported by the routed
// model/format).
func survivingImages(images []mcp.ContentBlock, gate mcpVisionGate, drops *[]visionDrop, logger *slog.Logger) []mcp.ContentBlock {
	var kept []mcp.ContentBlock
	for _, img := range images {
		if len(img.Data) > maxToolResultImageBytes {
			*drops = append(*drops, VisionDrop{Reason: "size_limit", Mime: img.MimeType})
			if logger != nil {
				logger.Debug("dropped oversized tool-result image block",
					"bytes", len(img.Data), "media_type", img.MimeType, "model", gate.Model)
			}
			continue
		}
		ok, reason := gate.Accept(img.MimeType)
		if !ok {
			*drops = append(*drops, VisionDrop{Reason: reason, Mime: img.MimeType})
			if logger != nil {
				logger.Debug("dropped tool-result image block",
					"reason", reason, "media_type", img.MimeType, "model", gate.Model)
			}
			continue
		}
		kept = append(kept, img)
	}
	return kept
}

// visionOmissionNote is the visible marker appended to a tool result whose
// image blocks were dropped.
func visionOmissionNote(drops []visionDrop) string {
	note := "[image omitted: not supported by the routed model or format]"
	if len(drops) > 1 {
		note = "[images omitted: not supported by the routed model or format]"
	}
	return note
}

// malformedImageCount counts the result's image blocks that carry no data or
// no media type: they are image INTENT the model should know was dropped, not
// content to lose silently.
func malformedImageCount(result *mcp.CallToolResult) int {
	n := 0
	for _, b := range result.Content {
		if b.Type == "image" && (b.Data == "" || b.MimeType == "") {
			n++
		}
	}
	return n
}

// recordVisionDrops counts the drops of one tool result.
func recordVisionDrops(metrics *infra.Metrics, drops []visionDrop) {
	for _, d := range drops {
		metrics.RecordVisionContentDropped("tool_result", d.Reason)
	}
}
