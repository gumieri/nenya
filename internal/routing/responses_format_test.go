package routing

// Pinning tests for the openai-responses wire format (NENYA-337, GitHub
// issue #154). The incident: OpenAI rejects function tools + reasoning_effort
// on /v1/chat/completions for gpt-6-luna/gpt-6.2-sol, so an agent that wants
// reasoning_effort AND tools needs the request dispatched to /v1/responses
// in Responses-API shape while the client keeps speaking chat completions.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

func responsesTestDeps(t *testing.T) TransformDeps {
	t.Helper()
	cfg := &config.Config{}
	if err := config.ApplyDefaults(cfg); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	providers := config.ResolveProviders(cfg, &config.SecretsConfig{
		ProviderKeys: map[string]string{"openai": "test-openai-key"},
	})
	return TransformDeps{
		Config:    cfg,
		Providers: providers,
		Metrics:   infra.NewMetrics(),
	}
}

// openCodeLikePayload is the incident shape: OpenCode always sends its
// built-in tools (nested chat-completions spelling) and auto-sends
// reasoning_effort for models flagged reasoning-capable.
func openCodeLikePayload() map[string]interface{} {
	return map[string]interface{}{
		"model":                 "gpt-6-luna",
		"stream":                true,
		"reasoning_effort":      "medium",
		"max_completion_tokens": 16384,
		"temperature":           0.7,
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        "bash",
					"description": "Run a shell command",
					"parameters": map[string]interface{}{
						"type":       "object",
						"properties": map[string]interface{}{"cmd": map[string]interface{}{"type": "string"}},
					},
				},
			},
		},
		"tool_choice": "auto",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are a coding agent."},
			map[string]interface{}{"role": "user", "content": "list files"},
			map[string]interface{}{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []interface{}{
					map[string]interface{}{
						"id":   "call_1",
						"type": "function",
						"function": map[string]interface{}{
							"name":      "bash",
							"arguments": `{"cmd":"ls"}`,
						},
					},
				},
			},
			map[string]interface{}{"role": "tool", "tool_call_id": "call_1", "content": "file1\nfile2"},
		},
	}
}

// TestTransformRequestForUpstream_ResponsesFormatConvertsToolsAndEffort pins
// the NENYA-337 incident: with the target format "openai-responses" the
// transformed body must be a Responses-API create request — input items,
// flat function tools, reasoning:{effort} — never the chat-completions
// spelling (which is what the upstream 400s on).
func TestTransformRequestForUpstream_ResponsesFormatConvertsToolsAndEffort(t *testing.T) {
	deps := responsesTestDeps(t)
	payload := openCodeLikePayload()

	body, _, err := TransformRequestForUpstream(deps, TransformParams{
		ProviderName: "openai",
		Model:        "gpt-6-luna",
		Format:       "openai-responses",
	}, payload)
	if err != nil {
		t.Fatalf("TransformRequestForUpstream: %v", err)
	}

	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal transformed body: %v", err)
	}

	// The chat-completions spellings must be gone: they are what the
	// upstream rejects for these models.
	for _, banned := range []string{"reasoning_effort", "max_tokens", "max_completion_tokens", "messages"} {
		if _, present := got[banned]; present {
			t.Errorf("transformed body still carries chat-completions key %q: %s", banned, body)
		}
	}

	// reasoning_effort must survive as reasoning.effort — the user wants
	// the effort, not a stripped request.
	reasoning, ok := got["reasoning"].(map[string]interface{})
	if !ok {
		t.Fatalf("transformed body has no reasoning object: %s", body)
	}
	if effort, _ := reasoning["effort"].(string); effort != "medium" {
		t.Errorf("reasoning.effort = %v, want medium", reasoning["effort"])
	}

	// Tools flatten: nested {type,function:{name,...}} → {type:"function", name,...}.
	tools, ok := got["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("transformed body tools = %v, want 1 flat function tool", got["tools"])
	}
	tool, ok := tools[0].(map[string]interface{})
	if !ok {
		t.Fatalf("tool entry is not an object: %v", tools[0])
	}
	if tool["type"] != "function" || tool["name"] != "bash" {
		t.Errorf("flat tool = %v, want type=function name=bash at top level", tool)
	}
	if _, nested := tool["function"]; nested {
		t.Errorf("tool entry still nests a function object: %v", tool)
	}
	if _, hasParams := tool["parameters"]; !hasParams {
		t.Errorf("flat tool lost its parameters: %v", tool)
	}

	// Messages → input items, including the tool loop's shapes.
	input, ok := got["input"].([]interface{})
	if !ok || len(input) != 4 {
		t.Fatalf("transformed body input = %v, want 4 items", got["input"])
	}
	sys, _ := input[0].(map[string]interface{})
	if sys["role"] != "system" {
		t.Errorf("input[0] role = %v, want system", sys["role"])
	}
	assistant, _ := input[2].(map[string]interface{})
	if assistant["type"] != "function_call" || assistant["call_id"] != "call_1" || assistant["name"] != "bash" {
		t.Errorf("assistant tool_calls did not convert to a function_call item: %v", assistant)
	}
	if args, _ := assistant["arguments"].(string); args != `{"cmd":"ls"}` {
		t.Errorf("function_call arguments = %v, want the raw JSON string", assistant["arguments"])
	}
	toolResult, _ := input[3].(map[string]interface{})
	if toolResult["type"] != "function_call_output" || toolResult["call_id"] != "call_1" {
		t.Errorf("tool message did not convert to function_call_output: %v", toolResult)
	}
	if out, _ := toolResult["output"].(string); out != "file1\nfile2" {
		t.Errorf("function_call_output output = %v, want the tool result text", toolResult["output"])
	}

	// The token cap renames; temperature/stream/tool_choice pass through.
	if mot, ok := got["max_output_tokens"].(float64); !ok || int(mot) != 16384 {
		t.Errorf("max_output_tokens = %v, want 16384", got["max_output_tokens"])
	}
	if got["stream"] != true || got["tool_choice"] != "auto" || got["temperature"] != 0.7 {
		t.Errorf("pass-through params drifted: stream=%v tool_choice=%v temperature=%v", got["stream"], got["tool_choice"], got["temperature"])
	}
	if got["model"] != "gpt-6-luna" {
		t.Errorf("model = %v, want gpt-6-luna", got["model"])
	}
}

// TestTransformRequestForUpstream_ResponsesFormatAgentDefaultEffort pins the
// agent-config injection path: params.ReasoningEffort (the agent default)
// must land as reasoning.effort when the client sent none.
func TestTransformRequestForUpstream_ResponsesFormatAgentDefaultEffort(t *testing.T) {
	deps := responsesTestDeps(t)
	payload := openCodeLikePayload()
	delete(payload, "reasoning_effort")

	body, _, err := TransformRequestForUpstream(deps, TransformParams{
		ProviderName:    "openai",
		Model:           "gpt-6-luna",
		Format:          "openai-responses",
		ReasoningEffort: "high",
	}, payload)
	if err != nil {
		t.Fatalf("TransformRequestForUpstream: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reasoning, ok := got["reasoning"].(map[string]interface{})
	if !ok {
		t.Fatalf("no reasoning object in %s", body)
	}
	if effort, _ := reasoning["effort"].(string); effort != "high" {
		t.Errorf("reasoning.effort = %v, want the agent default high", reasoning["effort"])
	}
}

// TestTransformRequestForUpstream_ResponsesFormatResponseFormat pins the
// response_format mapping: json_object → text.format, dropped from the top
// level (the Responses create request has no top-level response_format).
func TestTransformRequestForUpstream_ResponsesFormatResponseFormat(t *testing.T) {
	deps := responsesTestDeps(t)
	payload := openCodeLikePayload()
	payload["messages"] = []interface{}{
		map[string]interface{}{"role": "user", "content": "give json"},
	}
	payload["response_format"] = map[string]interface{}{"type": "json_object"}

	body, _, err := TransformRequestForUpstream(deps, TransformParams{
		ProviderName: "openai",
		Model:        "gpt-6-luna",
		Format:       "openai-responses",
	}, payload)
	if err != nil {
		t.Fatalf("TransformRequestForUpstream: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["response_format"]; present {
		t.Errorf("top-level response_format survived the conversion: %s", body)
	}
	text, ok := got["text"].(map[string]interface{})
	if !ok {
		t.Fatalf("no text object carrying the format: %s", body)
	}
	format, ok := text["format"].(map[string]interface{})
	if !ok || format["type"] != "json_object" {
		t.Errorf("text.format = %v, want type json_object", text["format"])
	}
}

// TestTransformRequestForUpstream_ResponsesFormatJsonSchema pins the
// structured-output mapping: the chat json_schema envelope unwraps into
// text.format WITH its type field — Responses requires format.type, and a
// missing one 400s every structured-output request upstream.
func TestTransformRequestForUpstream_ResponsesFormatJsonSchema(t *testing.T) {
	deps := responsesTestDeps(t)
	payload := openCodeLikePayload()
	payload["messages"] = []interface{}{
		map[string]interface{}{"role": "user", "content": "give json"},
	}
	payload["response_format"] = map[string]interface{}{
		"type": "json_schema",
		"json_schema": map[string]interface{}{
			"name":   "out",
			"strict": true,
			"schema": map[string]interface{}{"type": "object"},
		},
	}

	body, _, err := TransformRequestForUpstream(deps, TransformParams{
		ProviderName: "openai",
		Model:        "gpt-6-luna",
		Format:       "openai-responses",
	}, payload)
	if err != nil {
		t.Fatalf("TransformRequestForUpstream: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	text, ok := got["text"].(map[string]interface{})
	if !ok {
		t.Fatalf("no text object: %s", body)
	}
	format, ok := text["format"].(map[string]interface{})
	if !ok {
		t.Fatalf("no text.format: %s", body)
	}
	if format["type"] != "json_schema" {
		t.Errorf("text.format.type = %v, want json_schema (Responses requires it)", format["type"])
	}
	if format["name"] != "out" || format["strict"] != true {
		t.Errorf("text.format lost envelope fields: %v", format)
	}
	if _, hasSchema := format["schema"]; !hasSchema {
		t.Errorf("text.format lost the schema: %v", format)
	}
}

// TestProviderURL_ResponsesFormat pins the URL routing: the built-in openai
// provider must route the openai-responses format to /v1/responses, not
// silently fall back to the chat-completions URL.
func TestProviderURL_ResponsesFormat(t *testing.T) {
	cfg := &config.Config{}
	if err := config.ApplyDefaults(cfg); err != nil {
		t.Fatalf("ApplyDefaults: %v", err)
	}
	providers := config.ResolveProviders(cfg, &config.SecretsConfig{
		ProviderKeys: map[string]string{"openai": "k"},
	})
	openai := providers["openai"]
	if openai == nil {
		t.Fatal("no built-in openai provider")
	}
	url := ProviderURL("openai", "", "openai-responses", openai.FormatURLs, providers)
	if !strings.HasSuffix(url, "/v1/responses") {
		t.Errorf("ProviderURL(openai, openai-responses) = %q, want the /v1/responses endpoint", url)
	}
	// The chat format keeps its historical endpoint.
	if chat := ProviderURL("openai", "", "openai", openai.FormatURLs, providers); !strings.HasSuffix(chat, "/v1/chat/completions") {
		t.Errorf("ProviderURL(openai, openai) = %q, want the chat-completions endpoint", chat)
	}
}
