package proxy

import (
	"strings"

	"net/http"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/stream"
)

// exfilGuardFor builds the effective egress guard for a request: the
// per-agent override (enabled/action) applied on top of the global
// config. Returns nil when the guard is disabled for this agent — the
// zero-cost fast path for the response hot loop. Unknown agents resolve
// to the global settings.
func exfilGuardFor(gw *gateway.NenyaGateway, agentName string) *stream.ExfilGuard {
	global := gw.Config.Governance.ExfilGuard
	if global == nil {
		return nil
	}
	enabled := global.Enabled != nil && *global.Enabled
	action := global.Action
	if agent, ok := gw.Config.Agents[agentName]; ok && agent.ExfilGuard != nil {
		if agent.ExfilGuard.Enabled != nil {
			enabled = *agent.ExfilGuard.Enabled
		}
		if agent.ExfilGuard.Action != "" {
			action = agent.ExfilGuard.Action
		}
	}
	if !enabled {
		return nil
	}
	effective := *global
	effective.Enabled = &enabled
	effective.Action = action
	return stream.NewExfilGuard(effective, gw.Metrics)
}

// canaryScanHits aggregates a buffered canary scan.
type canaryScanHits struct {
	hit bool
}

// inspectCanaryTexts scans every model-produced text surface of a
// buffered response for the canary token. Block action never needs the
// surfaces rewritten (the response is discarded); log action strips the
// canary occurrence from each surface in place.
func inspectCanaryTexts(responseMap map[string]interface{}, canary string) canaryScanHits {
	hits := canaryScanHits{}
	rewrite := func(surface string) string {
		if pipeline.CanaryTripped(canary, surface) {
			hits.hit = true
			return strings.ReplaceAll(surface, canary, "")
		}
		return surface
	}
	walkResponseText(responseMap, rewrite)
	return hits
}

// responseExfilHits is the aggregated outcome of inspecting every text
// surface of a non-streaming response.
type responseExfilHits struct {
	blocked bool
	reason  string
}

// inspectResponseTexts runs the guard over every text surface of a
// response body (OpenAI choices[].message.content in string or
// content-array form, and Anthropic content[] text blocks), mutating
// stripped surfaces in place.
func inspectResponseTexts(guard *stream.ExfilGuard, responseMap map[string]interface{}) responseExfilHits {
	hits := responseExfilHits{}
	rewrite := func(surface string) string {
		cleaned, action, reason := guard.InspectFull(surface)
		switch action {
		case stream.ActionBlock:
			hits.blocked = true
			if hits.reason == "" {
				hits.reason = reason
			}
		case stream.ActionRedact:
			if hits.reason == "" {
				hits.reason = reason
			}
		}
		return cleaned
	}

	walkResponseText(responseMap, rewrite)
	return hits
}

// walkResponseText visits every model-produced text surface in a
// response body, applying fn and storing its result back in place.
// Covered: OpenAI choices[].message.content (string or content array),
// message.reasoning_content, message.tool_calls[].function.arguments;
// Anthropic content[] text and thinking blocks. Anthropic tool_use input
// objects are structured JSON, not prose — excluded (documented).
func walkResponseText(responseMap map[string]interface{}, fn func(string) string) {
	if responseMap == nil {
		return
	}
	// OpenAI shape: choices[].message.*.
	if choices, ok := responseMap["choices"].([]interface{}); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			walkOpenAIMessage(choice, fn)
		}
	}
	// Anthropic shape: content[] blocks.
	if blocks, ok := responseMap["content"].([]interface{}); ok {
		for _, b := range blocks {
			block, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				if text, ok := block["text"].(string); ok {
					block["text"] = fn(text)
				}
			case "thinking":
				if text, ok := block["thinking"].(string); ok {
					block["thinking"] = fn(text)
				}
			}
		}
	}
}

// walkOpenAIMessage applies fn to every model-produced text surface of
// one OpenAI choice: message.content, reasoning_content, and tool-call
// argument strings.
func walkOpenAIMessage(choice map[string]interface{}, fn func(string) string) {
	msg, ok := choice["message"].(map[string]interface{})
	if !ok {
		return
	}
	msg["content"] = walkContentSurface(msg["content"], fn)
	if reasoning, isStr := msg["reasoning_content"].(string); isStr {
		msg["reasoning_content"] = fn(reasoning)
	}
	toolCalls, ok := msg["tool_calls"].([]interface{})
	if !ok {
		return
	}
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]interface{})
		if !ok {
			continue
		}
		fnObj, ok := call["function"].(map[string]interface{})
		if !ok {
			continue
		}
		if args, ok := fnObj["arguments"].(string); ok {
			fnObj["arguments"] = fn(args)
		}
	}
}

// walkContentSurface handles a single content field: plain string or
// OpenAI content-array (text parts only; other part types untouched).
func walkContentSurface(content interface{}, fn func(string) string) interface{} {
	switch v := content.(type) {
	case string:
		return fn(v)
	case []interface{}:
		for _, p := range v {
			part, ok := p.(map[string]interface{})
			if !ok || part["type"] != "text" {
				continue
			}
			if text, ok := part["text"].(string); ok {
				part["text"] = fn(text)
			}
		}
		return content
	default:
		return content
	}
}

// handleExfilBlock writes the structured block response for a
// non-streaming reply: 403 with error_kind=exfil_blocked.
func (p *Proxy) handleExfilBlock(gw *gateway.NenyaGateway, w http.ResponseWriter, model, reason string) {
	gw.Logger.Warn("response blocked by exfil guard",
		"model", model, "reason", reason)
	writeStructuredError(w, http.StatusForbidden, infra.ErrorKindExfil,
		"response blocked by data-exfiltration policy")
}
