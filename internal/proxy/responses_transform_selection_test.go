package proxy

// NENYA-337 wiring pins: the transformer selection must return the
// Responses→chat converter for a chat client dispatched to an
// openai-responses target, and the composite (Responses → chat → Anthropic)
// for an anthropic client.

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
	"github.com/nenya/internal/testutil"
)

func TestResolveTransformer_ResponsesFormatChatClient(t *testing.T) {
	p := &Proxy{}
	gw := &gateway.NenyaGateway{Logger: testutil.NewTestLogger()}

	transformer := p.resolveTransformer(gw, routing.UpstreamTarget{
		Provider: "openai", Format: routing.FormatOpenAIResponses, Model: "gpt-6-luna",
	}, "openai", nil)
	if _, ok := transformer.(*stream.ResponsesTransformer); !ok {
		t.Fatalf("resolveTransformer = %T, want *stream.ResponsesTransformer (chat client, responses upstream)", transformer)
	}
}

func TestResolveTransformer_ResponsesFormatAnthropicClient(t *testing.T) {
	p := &Proxy{}
	gw := &gateway.NenyaGateway{Logger: testutil.NewTestLogger()}

	transformer := p.resolveTransformer(gw, routing.UpstreamTarget{
		Provider: "openai", Format: routing.FormatOpenAIResponses, Model: "gpt-6-luna",
	}, "anthropic", nil)
	comp, ok := transformer.(*compositeTransformer)
	if !ok {
		t.Fatalf("resolveTransformer = %T, want the Responses→chat→Anthropic composite", transformer)
	}
	if _, ok := comp.providerFirst.(*stream.ResponsesTransformer); !ok {
		t.Fatalf("composite.providerFirst = %T, want *stream.ResponsesTransformer", comp.providerFirst)
	}
	if _, ok := comp.clientSecond.(*stream.OpenAIToAnthropicTransformer); !ok {
		t.Fatalf("composite.clientSecond = %T, want *stream.OpenAIToAnthropicTransformer", comp.clientSecond)
	}
}

// TestResolveTransformer_ResponsesCompositeStreamClose pins the round-3 [H]
// finding: an Anthropic-client Responses stream must terminate through the
// composite's held tail (message_delta stop_reason + message_stop) — without
// a FinishTail on the composite the reader never pulls the client-stage
// converter's held close, and the stream ends with a foreign [DONE], no
// stop_reason, and no client-visible usage.
func TestResolveTransformer_ResponsesCompositeStreamClose(t *testing.T) {
	p := &Proxy{}
	gw := &gateway.NenyaGateway{Logger: testutil.NewTestLogger()}
	tr := p.resolveTransformer(gw, routing.UpstreamTarget{
		Provider: "openai", Format: routing.FormatOpenAIResponses, Model: "gpt-6-luna",
	}, "anthropic", nil)

	wire := strings.Join([]string{
		`data: {"type":"response.created","response":{"id":"r1","model":"gpt-6-luna"}}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		``,
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`,
		``,
	}, "\n")
	reader := stream.NewSSETransformingReader(strings.NewReader(wire), tr, context.Background())
	var out bytes.Buffer
	if _, err := io.Copy(&out, reader); err != nil {
		t.Fatalf("read stream: %v", err)
	}
	body := out.String()
	if !strings.Contains(body, "stop_reason") {
		t.Fatalf("anthropic client must receive the stop_reason close, got:\n%s", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("anthropic client must receive message_stop, got:\n%s", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Fatalf("a converted Anthropic stream must not receive the OpenAI sentinel (AUD-01):\n%s", body)
	}
}
