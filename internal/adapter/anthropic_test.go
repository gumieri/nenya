package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicAdapter_InjectAuth(t *testing.T) {
	a := NewAnthropicAdapter()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "sk-ant-test123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-ant-test123" {
		t.Errorf("expected x-api-key 'sk-ant-test123', got %q", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("expected anthropic-version '2023-06-01', got %q", got)
	}
}

func TestAnthropicAdapter_InjectAuth_EmptyKey(t *testing.T) {
	a := NewAnthropicAdapter()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	err := a.InjectAuth(req, "")
	if err == nil {
		t.Error("expected error for empty API key")
	}
}

func TestAnthropicAdapter_MutateRequest_EmptyBody(t *testing.T) {
	a := NewAnthropicAdapter()
	out, err := a.MutateRequest(nil, "claude-3-opus", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input")
	}
}

func TestAnthropicAdapter_MutateRequest_Basic(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3-opus","messages":[{"role":"user","content":"hello"}],"max_tokens":4096,"temperature":0.7}`)
	out, err := a.MutateRequest(body, "claude-3-opus", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if m["model"] != "claude-3-opus" {
		t.Errorf("expected model 'claude-3-opus', got %v", m["model"])
	}
	if m["stream"] != true {
		t.Error("expected stream=true")
	}
	if m["max_tokens"] != float64(4096) {
		t.Errorf("expected max_tokens=4096, got %v", m["max_tokens"])
	}
	if m["temperature"] != 0.7 {
		t.Errorf("expected temperature=0.7, got %v", m["temperature"])
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	msg := msgs[0].(map[string]interface{})
	if msg["role"] != "user" {
		t.Errorf("expected role 'user', got %v", msg["role"])
	}
}

func TestAnthropicAdapter_MutateRequest_SystemMessage(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3-opus","messages":[{"role":"system","content":"You are a helpful assistant"},{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "claude-3-opus", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if m["system"] != "You are a helpful assistant" {
		t.Errorf("expected system 'You are a helpful assistant', got %v", m["system"])
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (system removed), got %d", len(msgs))
	}
}

func TestAnthropicAdapter_MutateRequest_Tools(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{
		"model": "claude-3-opus",
		"messages": [{"role": "user", "content": "what's the weather?"}],
		"tools": [{"type": "function", "function": {"name": "get_weather", "description": "Get weather", "parameters": {"type": "object"}}}],
		"tool_choice": {"type": "function", "function": {"name": "get_weather"}}
	}`)
	out, err := a.MutateRequest(body, "claude-3-opus", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	tools := m["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "get_weather" {
		t.Errorf("expected tool name 'get_weather', got %v", tool["name"])
	}
	if tool["description"] != "Get weather" {
		t.Errorf("expected description 'Get weather', got %v", tool["description"])
	}

	tc := m["tool_choice"].(map[string]interface{})
	if tc["type"] != "tool" || tc["name"] != "get_weather" {
		t.Errorf("expected tool_choice {type:tool name:get_weather}, got %v", tc)
	}
}

func TestAnthropicAdapter_MutateRequest_ToolChoiceAuto(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"tool_choice":"auto"}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	tc := m["tool_choice"].(map[string]interface{})
	if tc["type"] != "auto" {
		t.Errorf("expected tool_choice type 'auto', got %v", tc["type"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolChoiceNone(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"tool_choice":"none"}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if _, ok := m["tool_choice"]; ok {
		t.Error("tool_choice should be absent when 'none'")
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tc1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tc1","content":"sunny"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	toolMsg := msgs[1].(map[string]interface{})
	if toolMsg["role"] != "user" {
		t.Errorf("expected tool message role 'user', got %v", toolMsg["role"])
	}
	content := toolMsg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["type"] != "tool_result" {
		t.Errorf("expected block type 'tool_result', got %v", block["type"])
	}
	if block["tool_use_id"] != "tc1" {
		t.Errorf("expected tool_use_id 'tc1', got %v", block["tool_use_id"])
	}
	if block["content"] != "sunny" {
		t.Errorf("expected content 'sunny', got %v", block["content"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_ClientModifiedID(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu_original_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"chatcmpl-tool-xxx","content":"sunny"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	toolMsg := msgs[1].(map[string]interface{})
	if toolMsg["role"] != "user" {
		t.Errorf("expected tool message role 'user', got %v", toolMsg["role"])
	}

	content := toolMsg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}

	block := content[0].(map[string]interface{})
	if block["type"] != "tool_result" {
		t.Errorf("expected block type 'tool_result', got %v", block["type"])
	}

	if block["tool_use_id"] != "tu_original_1" {
		t.Errorf("expected tool_use_id 'tu_original_1' (original tool_use.id), got %v", block["tool_use_id"])
	}

	if block["content"] != "sunny" {
		t.Errorf("expected content 'sunny', got %v", block["content"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_MultiTool(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},{"id":"tu_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]},{"role":"tool","tool_call_id":"whatever_1","content":"sunny"},{"role":"tool","tool_call_id":"whatever_2","content":"12:00"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	tool1 := msgs[1].(map[string]interface{})
	content1 := tool1["content"].([]interface{})
	if len(content1) != 1 {
		t.Fatalf("expected 1 content block for tool1, got %d", len(content1))
	}

	block1 := content1[0].(map[string]interface{})
	if block1["tool_use_id"] != "tu_1" {
		t.Errorf("expected tool_use_id 'tu_1' for first tool, got %v", block1["tool_use_id"])
	}

	tool2 := msgs[2].(map[string]interface{})
	content2 := tool2["content"].([]interface{})
	if len(content2) != 1 {
		t.Fatalf("expected 1 content block for tool2, got %d", len(content2))
	}

	block2 := content2[0].(map[string]interface{})
	if block2["tool_use_id"] != "tu_2" {
		t.Errorf("expected tool_use_id 'tu_2' for second tool, got %v", block2["tool_use_id"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_MoreToolsThanIDs(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tc1","content":"sunny"},{"role":"tool","tool_call_id":"tc2","content":"12:00"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	tool1 := msgs[1].(map[string]interface{})
	content1 := tool1["content"].([]interface{})
	if len(content1) != 1 {
		t.Fatalf("expected 1 content block for tool1, got %d", len(content1))
	}

	block1 := content1[0].(map[string]interface{})
	if block1["type"] != "tool_result" {
		t.Errorf("expected block type 'tool_result', got %v", block1["type"])
	}

	if block1["tool_use_id"] != "tu_1" {
		t.Errorf("expected tool_use_id 'tu_1' for first tool, got %v", block1["tool_use_id"])
	}

	tool2 := msgs[2].(map[string]interface{})
	content2 := tool2["content"].([]interface{})
	if len(content2) != 1 {
		t.Fatalf("expected 1 content block for tool2, got %d", len(content2))
	}

	block2 := content2[0].(map[string]interface{})
	if _, ok := block2["tool_use_id"]; ok {
		t.Errorf("expected no tool_use_id for orphaned tool result, got %v", block2["tool_use_id"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_NoAssistant(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hello"},{"role":"tool","tool_call_id":"tc1","content":"result"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}

	toolMsg := msgs[1].(map[string]interface{})
	content := toolMsg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block for tool, got %d", len(content))
	}

	block := content[0].(map[string]interface{})
	if block["type"] != "tool_result" {
		t.Errorf("expected block type 'tool_result', got %v", block["type"])
	}

	if _, ok := block["tool_use_id"]; ok {
		t.Errorf("expected no tool_use_id for orphaned tool result, got %v", block["tool_use_id"])
	}
}

func TestAnthropicAdapter_MutateResponse_NonEmpty(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hello!"}],"model":"claude-3","stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	if m["object"] != "chat.completion" {
		t.Errorf("expected object 'chat.completion', got %v", m["object"])
	}
	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	delta := choice["delta"].(map[string]interface{})
	if delta["content"] != "Hello!" {
		t.Errorf("expected content 'Hello!', got %v", delta["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Errorf("expected finish_reason 'stop', got %v", choice["finish_reason"])
	}
	usage := m["usage"].(map[string]interface{})
	if usage["prompt_tokens"] != float64(10) {
		t.Errorf("expected prompt_tokens=10, got %v", usage["prompt_tokens"])
	}
	if usage["completion_tokens"] != float64(5) {
		t.Errorf("expected completion_tokens=5, got %v", usage["completion_tokens"])
	}
}

func TestAnthropicAdapter_MutateResponse_ToolUse(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"get_weather","input":{"city":"London"}}],"model":"claude-3","stop_reason":"tool_use"}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("expected finish_reason 'tool_calls', got %v", choice["finish_reason"])
	}
	delta := choice["delta"].(map[string]interface{})
	tcs := delta["tool_calls"].([]interface{})
	tc := tcs[0].(map[string]interface{})
	if tc["id"] != "tu1" {
		t.Errorf("expected tool_call id 'tu1', got %v", tc["id"])
	}
	fn := tc["function"].(map[string]interface{})
	if fn["name"] != "get_weather" {
		t.Errorf("expected function name 'get_weather', got %v", fn["name"])
	}
}

func TestAnthropicAdapter_MutateResponse_MaxTokensStop(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"id":"msg_123","type":"message","role":"assistant","content":[{"type":"text","text":"partial"}],"model":"claude-3","stop_reason":"max_tokens"}`)
	out, err := a.MutateResponse(body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	choices := m["choices"].([]interface{})
	choice := choices[0].(map[string]interface{})
	if choice["finish_reason"] != "length" {
		t.Errorf("expected finish_reason 'length', got %v", choice["finish_reason"])
	}
}

func TestAnthropicAdapter_MutateResponse_EmptyBody(t *testing.T) {
	a := NewAnthropicAdapter()
	out, err := a.MutateResponse(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil output for nil input")
	}
}

func TestAnthropicAdapter_NormalizeError(t *testing.T) {
	a := NewAnthropicAdapter()

	tests := []struct {
		code int
		body string
		want ErrorClass
	}{
		{429, "", ErrorRateLimited},
		{529, "", ErrorRateLimited},
		{500, "", ErrorRetryable},
		{502, "", ErrorRetryable},
		{503, "", ErrorRetryable},
		{504, "", ErrorRetryable},
		{400, `{"error":"overloaded"}`, ErrorRetryable},
		{400, `{"error":"rate limit exceeded"}`, ErrorRetryable},
		{400, `{"error":"invalid request"}`, ErrorPermanent},
		{404, "", ErrorPermanent},
	}

	for _, tt := range tests {
		got := a.NormalizeError(tt.code, []byte(tt.body))
		if got != tt.want {
			t.Errorf("NormalizeError(%d, %q) = %v, want %v", tt.code, tt.body, got, tt.want)
		}
	}
}

func TestAnthropicAdapter_MutateRequest_StopSequences(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"stop":["\n\n","."]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	stopSeqs, ok := m["stop_sequences"].([]interface{})
	if !ok || len(stopSeqs) != 2 {
		t.Fatalf("expected stop_sequences with 2 elements, got %v", m["stop_sequences"])
	}
}

func TestAnthropicAdapter_MutateRequest_UserMetadata(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"}],"user":"user-123"}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	meta, ok := m["metadata"].(map[string]interface{})
	if !ok || meta["user_id"] != "user-123" {
		t.Errorf("expected metadata.user_id 'user-123', got %v", m["metadata"])
	}
}

func TestAnthropicAdapter_MutateRequest_ContentArraySystem(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"system","content":[{"type":"text","text":"You are helpful."},{"type":"text","text":"Be concise."}]},{"role":"user","content":"hi"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	system := m["system"].(string)
	if !strings.Contains(system, "You are helpful.") || !strings.Contains(system, "Be concise.") {
		t.Errorf("expected system to contain both texts, got %q", system)
	}
}

func TestAnthropicAdapter_ConvertOpenAIToAnthropicBody(t *testing.T) {
	a := NewAnthropicAdapter()
	openai := map[string]interface{}{
		"model": "gpt-4",
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	result := a.ConvertOpenAIToAnthropicBody(openai, "claude-3", false)
	if result["model"] != "claude-3" {
		t.Errorf("expected model 'claude-3', got %v", result["model"])
	}
}

func TestAnthropicAdapter_ConvertAnthropicToOpenAIBody(t *testing.T) {
	a := NewAnthropicAdapter()
	anthropic := map[string]interface{}{
		"id":      "msg_123",
		"type":    "message",
		"role":    "assistant",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "hi"}},
		"model":   "claude-3",
	}
	result := a.ConvertAnthropicToOpenAIBody(anthropic)
	if result["object"] != "chat.completion" {
		t.Errorf("expected object 'chat.completion', got %v", result["object"])
	}
}

func TestAnthropicAdapter_GetAnthropicAdapter(t *testing.T) {
	a1 := GetAnthropicAdapter()
	a2 := GetAnthropicAdapter()
	if a1 != a2 {
		t.Error("GetAnthropicAdapter should return the same instance")
	}
	if a1.version != "2023-06-01" {
		t.Errorf("expected version '2023-06-01', got %q", a1.version)
	}
}

func TestGenerateID(t *testing.T) {
	id := generateID()
	if len(id) != 24 {
		t.Errorf("expected ID length 24, got %d", len(id))
	}
}