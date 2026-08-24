package stream

import (
	"context"
	"io"
	"strings"
	"testing"
)

type recordingObserver struct {
	events []SSEEvent
}

func (o *recordingObserver) OnSSEEvent(event SSEEvent) {
	o.events = append(o.events, event)
}

func (o *recordingObserver) OnStreamClose(err error) {}

func TestSSETransformingReader_ErrorPayloadNotifiesObserver(t *testing.T) {
	obs := &recordingObserver{}
	tr := NewAnthropicTransformer()

	// Input: content chunk, then upstream error payload, then done
	input := "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"data: {\"message\":\"Streaming response failed: [502] Upstream error from Nvidia: Service temporarily overloaded\",\"type\":\"server_error\"}\n\n" +
		"data: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), tr, context.Background())
	reader.SetObserver(obs)

	var output strings.Builder
	_, _ = io.Copy(&output, reader)

	// Should have received 3 events: content (empty Type -> ""), error, done
	if len(obs.events) != 3 {
		t.Fatalf("expected 3 observer events, got %d: %+v", len(obs.events), obs.events)
	}

	// First event: content delta consumed by the transformer -> Type = "consumed"
	if obs.events[0].Type != "consumed" {
		t.Errorf("first event type = %q, want \"consumed\"", obs.events[0].Type)
	}

	// Second event: error payload -> Type = "error"
	if obs.events[1].Type != "error" {
		t.Errorf("second event type = %q, want \"error\"", obs.events[1].Type)
	}
	if obs.events[1].Data == nil {
		t.Errorf("second event data is nil")
	} else {
		if obs.events[1].Data["type"] != "server_error" {
			t.Errorf("second event data type = %v, want server_error", obs.events[1].Data["type"])
		}
		if !strings.Contains(obs.events[1].Data["message"].(string), "Nvidia") {
			t.Errorf("second event data message missing Nvidia: %v", obs.events[1].Data["message"])
		}
	}

	// Third event: [DONE] -> Type = "done"
	if obs.events[2].Type != "done" {
		t.Errorf("third event type = %q, want \"done\"", obs.events[2].Type)
	}
}

func TestSSETransformingReader_OpenAIErrorNotifiesObserver(t *testing.T) {
	obs := &recordingObserver{}

	input := "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"rate limited\",\"type\":\"rate_limit_error\"}}\n\n" +
		"data: [DONE]\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), nil, context.Background())
	reader.SetObserver(obs)

	var output strings.Builder
	_, _ = io.Copy(&output, reader)

	// The error should be tagged "error" so observer sees it
	var errorSeen bool
	for _, e := range obs.events {
		if e.Type == "error" && e.Data != nil && e.Data["error"] != nil {
			errorSeen = true
			errMap := e.Data["error"].(map[string]any)
			if errMap["type"] != "rate_limit_error" {
				t.Errorf("error type = %v, want rate_limit_error", errMap["type"])
			}
		}
	}
	if !errorSeen {
		t.Errorf("expected observer to see Type=error for OpenAI error payload")
	}
}

func TestSSETransformingReader_AnthropicErrorNotifiesObserver(t *testing.T) {
	obs := &recordingObserver{}
	tr := NewAnthropicTransformer()

	input := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\"}}\n\n" +
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	reader := NewSSETransformingReader(strings.NewReader(input), tr, context.Background())
	reader.SetObserver(obs)

	var output strings.Builder
	_, _ = io.Copy(&output, reader)

	var errorSeen bool
	for _, e := range obs.events {
		if e.Type == "error" && e.Data != nil {
			// Anthropic transformer may wrap the error differently; check raw
			errorSeen = true
		}
	}
	if !errorSeen {
		t.Errorf("expected observer to see Type=error for Anthropic error event")
	}
}
