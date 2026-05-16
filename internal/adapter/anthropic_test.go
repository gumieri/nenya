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

	assistantMsg := msgs[0].(map[string]interface{})
	assistantContent := assistantMsg["content"].([]interface{})
	if len(assistantContent) != 1 {
		t.Fatalf("expected 1 assistant content block, got %d", len(assistantContent))
	}
	toolUse := assistantContent[0].(map[string]interface{})
	if toolUse["type"] != "tool_use" {
		t.Errorf("expected tool_use block, got type %v", toolUse["type"])
	}
	if toolUse["id"] != "tc1" {
		t.Errorf("expected tool_use id 'tc1', got %v", toolUse["id"])
	}
	if toolUse["name"] != "get_weather" {
		t.Errorf("expected tool_use name 'get_weather', got %v", toolUse["name"])
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
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (orphaned tool result dropped), got %d", len(msgs))
	}

	assistantMsg := msgs[0].(map[string]interface{})
	content := assistantMsg["content"].([]interface{})
	if content[0].(map[string]interface{})["type"] != "tool_use" {
		t.Errorf("expected assistant message with tool_use, got %v", content)
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_MultiTool(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},{"id":"tu_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu_1","content":"sunny"},{"role":"tool","tool_call_id":"tu_2","content":"12:00"}]}`)
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
		t.Fatalf("expected 2 messages (assistant + coalesced tool results), got %d", len(msgs))
	}

	assistantMsg := msgs[0].(map[string]interface{})
	assistantContent := assistantMsg["content"].([]interface{})
	if len(assistantContent) != 2 {
		t.Fatalf("expected 2 tool_use blocks in assistant, got %d", len(assistantContent))
	}
	tu1 := assistantContent[0].(map[string]interface{})
	if tu1["id"] != "tu_1" || tu1["name"] != "get_weather" {
		t.Errorf("expected first tool_use {id:tu_1 name:get_weather}, got %v", tu1)
	}
	tu2 := assistantContent[1].(map[string]interface{})
	if tu2["id"] != "tu_2" || tu2["name"] != "get_time" {
		t.Errorf("expected second tool_use {id:tu_2 name:get_time}, got %v", tu2)
	}

	toolUser := msgs[1].(map[string]interface{})
	if toolUser["role"] != "user" {
		t.Errorf("expected coalesced tool results as user message, got role %v", toolUser["role"])
	}
	content := toolUser["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks in coalesced user message, got %d", len(content))
	}

	block1 := content[0].(map[string]interface{})
	if block1["tool_use_id"] != "tu_1" || block1["content"] != "sunny" {
		t.Errorf("expected first tool_result {tool_use_id:tu_1 content:sunny}, got %v", block1)
	}

	block2 := content[1].(map[string]interface{})
	if block2["tool_use_id"] != "tu_2" || block2["content"] != "12:00" {
		t.Errorf("expected second tool_result {tool_use_id:tu_2 content:12:00}, got %v", block2)
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
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (orphaned tool results dropped), got %d", len(msgs))
	}

	assistantMsg := msgs[0].(map[string]interface{})
	content := assistantMsg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 content block (tool_use), got %d", len(content))
	}
	if content[0].(map[string]interface{})["type"] != "tool_use" {
		t.Errorf("expected tool_use block, got %v", content[0])
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
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (orphaned tool result dropped), got %d", len(msgs))
	}

	userMsg := msgs[0].(map[string]interface{})
	if userMsg["role"] != "user" {
		t.Errorf("expected user message, got role %v", userMsg["role"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_MissingToolCallID(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hello"},{"role":"tool","content":"result"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message (tool without ID dropped), got %d", len(msgs))
	}

	userMsg := msgs[0].(map[string]interface{})
	if userMsg["role"] != "user" {
		t.Errorf("expected user message, got role %v", userMsg["role"])
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

// generateID is a simple deterministic ID generator for testing
func generateID() string {
	return "test12345678901234567890"
}

func TestAnthropicAdapter_MutateRequest_AssistantWithTextAndToolCalls(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"I will check the weather.","tool_calls":[{"id":"tc1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"London\"}"}}]},{"role":"tool","tool_call_id":"tc1","content":"sunny"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	assistantMsg := msgs[0].(map[string]interface{})
	assistantContent := assistantMsg["content"].([]interface{})
	if len(assistantContent) != 2 {
		t.Fatalf("expected 2 content blocks (text + tool_use), got %d", len(assistantContent))
	}

	textBlock := assistantContent[0].(map[string]interface{})
	if textBlock["type"] != "text" {
		t.Errorf("expected first block type 'text', got %v", textBlock["type"])
	}
	if textBlock["text"] != "I will check the weather." {
		t.Errorf("expected text content, got %v", textBlock["text"])
	}

	toolUse := assistantContent[1].(map[string]interface{})
	if toolUse["type"] != "tool_use" {
		t.Errorf("expected second block type 'tool_use', got %v", toolUse["type"])
	}
	if toolUse["name"] != "get_weather" {
		t.Errorf("expected tool_use name 'get_weather', got %v", toolUse["name"])
	}
	input := toolUse["input"].(map[string]interface{})
	if input["city"] != "London" {
		t.Errorf("expected input city 'London', got %v", input["city"])
	}
}

func TestAnthropicAdapter_MutateRequest_AssistantNoContentNoToolCalls(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":""}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	assistantMsg := msgs[1].(map[string]interface{})
	content := assistantMsg["content"].([]interface{})
	if len(content) == 0 {
		t.Fatal("expected non-empty content for empty assistant message")
	}
	textBlock := content[0].(map[string]interface{})
	if textBlock["type"] != "text" {
		t.Errorf("expected fallback text block, got type %v", textBlock["type"])
	}
}

func TestAnthropicAdapter_MutateRequest_AssistantNilContentWithToolCalls(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","tool_calls":[{"id":"tu1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"ls\"}"}}]},{"role":"tool","tool_call_id":"tu1","content":"file.txt"}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	assistantMsg := msgs[0].(map[string]interface{})
	content := assistantMsg["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 tool_use block (no text), got %d", len(content))
	}
	toolUse := content[0].(map[string]interface{})
	if toolUse["type"] != "tool_use" {
		t.Errorf("expected tool_use, got %v", toolUse["type"])
	}
	if toolUse["id"] != "tu1" {
		t.Errorf("expected id 'tu1', got %v", toolUse["id"])
	}
	input := toolUse["input"].(map[string]interface{})
	if input["command"] != "ls" {
		t.Errorf("expected input command 'ls', got %v", input["command"])
	}
}

func TestAnthropicAdapter_MutateRequest_UserEmptyContent(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"user","content":""}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	userMsg := msgs[0].(map[string]interface{})
	content := userMsg["content"].([]interface{})
	if len(content) == 0 {
		t.Fatal("expected non-empty content for empty user message")
	}
	textBlock := content[0].(map[string]interface{})
	if textBlock["type"] != "text" {
		t.Errorf("expected fallback text block, got type %v", textBlock["type"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_ReorderedResults(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{}"}},{"id":"tu_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu_2","content":"12:00"},{"role":"tool","tool_call_id":"tu_1","content":"sunny"}]}`)
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
		t.Fatalf("expected 2 messages (assistant + coalesced tool results), got %d", len(msgs))
	}

	toolUser := msgs[1].(map[string]interface{})
	if toolUser["role"] != "user" {
		t.Errorf("expected coalesced tool results as user message, got role %v", toolUser["role"])
	}
	content := toolUser["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks, got %d", len(content))
	}

	block1 := content[0].(map[string]interface{})
	if block1["tool_use_id"] != "tu_2" || block1["content"] != "12:00" {
		t.Errorf("expected first tool_result tu_2 with '12:00', got %v", block1)
	}

	block2 := content[1].(map[string]interface{})
	if block2["tool_use_id"] != "tu_1" || block2["content"] != "sunny" {
		t.Errorf("expected second tool_result tu_1 with 'sunny', got %v", block2)
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_ArrayContent(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu1","content":[{"type":"text","text":"Hello world"},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,/9j/4AAQSkZJRg=="}}]}]}`)
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
		t.Fatalf("expected 1 tool_result block, got %d", len(content))
	}

	block := content[0].(map[string]interface{})
	if block["type"] != "tool_result" {
		t.Fatalf("expected tool_result block, got %v", block["type"])
	}
	if block["tool_use_id"] != "tu1" {
		t.Errorf("expected tool_use_id 'tu1', got %v", block["tool_use_id"])
	}

	resultContent, ok := block["content"].([]interface{})
	if !ok {
		t.Fatalf("expected array content, got %T", block["content"])
	}
	if len(resultContent) != 2 {
		t.Fatalf("expected 2 content blocks in tool_result, got %d", len(resultContent))
	}

	textBlock := resultContent[0].(map[string]interface{})
	if textBlock["type"] != "text" || textBlock["text"] != "Hello world" {
		t.Errorf("expected text block, got %v", textBlock)
	}

	imageBlock := resultContent[1].(map[string]interface{})
	if imageBlock["type"] != "image" {
		t.Fatalf("expected image block, got %v", imageBlock["type"])
	}
	source, ok := imageBlock["source"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected image source, got %T", imageBlock["source"])
	}
	if source["type"] != "base64" || source["media_type"] != "image/jpeg" {
		t.Errorf("expected image source with base64 type and jpeg media_type, got %v", source)
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_NativeAnthropicImage(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu1","type":"function","function":{"name":"capture_screen","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUgAAAQ"}}]}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	toolMsg := msgs[1].(map[string]interface{})
	content := toolMsg["content"].([]interface{})
	block := content[0].(map[string]interface{})
	resultContent := block["content"].([]interface{})
	imageBlock := resultContent[0].(map[string]interface{})
	if imageBlock["type"] != "image" {
		t.Errorf("expected image block type, got %v", imageBlock["type"])
	}
	if imageBlock["source"].(map[string]interface{})["media_type"] != "image/png" {
		t.Errorf("expected image/png media_type, got %v", imageBlock["source"].(map[string]interface{})["media_type"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_EmptyArrayContent(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu1","type":"function","function":{"name":"nop","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu1","content":[]}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	toolMsg := msgs[1].(map[string]interface{})
	content := toolMsg["content"].([]interface{})
	block := content[0].(map[string]interface{})
	if block["content"] != "" {
		t.Errorf("expected empty string content for empty array, got %v", block["content"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_NativeToolResultBlock(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu1","type":"function","function":{"name":"nested_tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu1","content":[{"type":"tool_result","tool_use_id":"nested_tu1","content":"nested result"}]}]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})
	toolMsg := msgs[1].(map[string]interface{})
	content := toolMsg["content"].([]interface{})
	block := content[0].(map[string]interface{})
	if block["tool_use_id"] != "tu1" {
		t.Errorf("expected tool_use_id 'tu1', got %v", block["tool_use_id"])
	}
	resultContent := block["content"].([]interface{})
	nestedBlock := resultContent[0].(map[string]interface{})
	if nestedBlock["type"] != "tool_result" {
		t.Errorf("expected nested tool_result block, got %v", nestedBlock["type"])
	}
	if nestedBlock["tool_use_id"] != "nested_tu1" {
		t.Errorf("expected nested tool_use_id 'nested_tu1', got %v", nestedBlock["tool_use_id"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_PartialOrphans(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},{"role":"tool","tool_call_id":"tu_1","content":"sunny"},{"role":"tool","tool_call_id":"orphan_1","content":"stale"}]}`)
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
		t.Fatalf("expected 2 messages (assistant + coalesced valid results), got %d", len(msgs))
	}

	toolUser := msgs[1].(map[string]interface{})
	content := toolUser["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("expected 1 tool_result (orphan dropped), got %d", len(content))
	}
	block := content[0].(map[string]interface{})
	if block["tool_use_id"] != "tu_1" {
		t.Errorf("expected tool_use_id 'tu_1', got %v", block["tool_use_id"])
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_MultiTurnToolUse(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[
		{"role":"user","content":"check weather"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tu_1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"tu_1","content":"sunny"},
		{"role":"assistant","content":"The weather is sunny."},
		{"role":"user","content":"also check time"},
		{"role":"assistant","content":"","tool_calls":[{"id":"tu_2","type":"function","function":{"name":"get_time","arguments":"{}"}},{"id":"tu_3","type":"function","function":{"name":"get_timezone","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"tu_3","content":"UTC"},
		{"role":"tool","tool_call_id":"tu_2","content":"12:00"}
	]}`)
	out, err := a.MutateRequest(body, "claude-3", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := m["messages"].([]interface{})

	assistantCount := 0
	for _, msgRaw := range msgs {
		msg := msgRaw.(map[string]interface{})
		if msg["role"] == "assistant" {
			assistantCount++
		}
	}
	if assistantCount != 3 {
		t.Errorf("expected 3 assistant messages, got %d", assistantCount)
	}

	lastAssistant := msgs[len(msgs)-2].(map[string]interface{})
	assistantContent := lastAssistant["content"].([]interface{})
	var toolUseIDs []string
	for _, b := range assistantContent {
		bm := b.(map[string]interface{})
		if bm["type"] == "tool_use" {
			toolUseIDs = append(toolUseIDs, bm["id"].(string))
		}
	}
	if len(toolUseIDs) != 2 {
		t.Fatalf("expected 2 tool_use blocks in last assistant, got %d", len(toolUseIDs))
	}

	toolUser := msgs[len(msgs)-1].(map[string]interface{})
	if toolUser["role"] != "user" {
		t.Errorf("expected coalesced tool results as user, got %v", toolUser["role"])
	}
	content := toolUser["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("expected 2 tool_result blocks (coalesced), got %d", len(content))
	}
}

func TestAnthropicAdapter_MutateRequest_ToolMessage_AssistantWithoutTools(t *testing.T) {
	a := NewAnthropicAdapter()
	body := []byte(`{"model":"claude-3","messages":[
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"},
		{"role":"tool","tool_call_id":"tc1","content":"result"}
	]}`)
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
		t.Fatalf("expected 2 messages (orphaned tool result dropped), got %d", len(msgs))
	}
}
