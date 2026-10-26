package proxy

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"io"

	"github.com/nenya/config"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/testutil"
)

const anthropicFrames = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

const openAIFrames = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func newReplayGateway(t *testing.T) *gateway.NenyaGateway {
	t.Helper()
	cfg := testutil.MinimalConfig()
	gw := gateway.New(context.Background(), *cfg, &config.SecretsConfig{ClientToken: "t"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(gw.Close)
	return gw
}

// TestConvertBufferedForReplay pins issue 723f3db1: the MCP replay must
// re-render the buffer into the CLIENT's wire format when the serving
// target's format differs. Pre-fix replayBufferedResponse wrote the
// upstream's NATIVE frames verbatim — an OpenAI client of an Anthropic-format
// chain received message_start/content_block_delta frames it cannot parse,
// and an Anthropic client of an OpenAI chain received OpenAI chunks with no
// message_stop.
func TestConvertBufferedForReplay(t *testing.T) {
	gw := newReplayGateway(t)
	p := &Proxy{}
	p.StoreGateway(gw)

	t.Run("anthropic target served to an openai client", func(t *testing.T) {
		buf := &bufferedSSE{
			rawBytes:      []byte(anthropicFrames),
			servingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "anthropic"},
		}
		out := string(p.convertBufferedForReplay(gw, buf, "openai", nil))
		if !strings.Contains(out, `"choices"`) {
			t.Fatalf("an OpenAI client must receive OpenAI chunks, got:\n%s", out)
		}
		if !strings.Contains(out, `data: [DONE]`) {
			t.Fatalf("the OpenAI stream must terminate with the sentinel:\n%s", out)
		}
		if strings.Contains(out, "message_start") {
			t.Fatalf("native Anthropic frames must not reach an OpenAI client:\n%s", out)
		}
	})

	t.Run("openai target served to an anthropic client", func(t *testing.T) {
		buf := &bufferedSSE{
			rawBytes:      []byte(openAIFrames),
			servingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "openai"},
		}
		out := string(p.convertBufferedForReplay(gw, buf, "anthropic", nil))
		if !strings.Contains(out, `"message_stop"`) {
			t.Fatalf("an Anthropic client must receive the message_stop terminator:\n%s", out)
		}
		if !strings.Contains(out, `"text_delta"`) {
			t.Fatalf("the content must ride a text_delta block:\n%s", out)
		}
		if strings.Contains(out, `"choices"`) {
			t.Fatalf("native OpenAI frames must not reach an Anthropic client:\n%s", out)
		}
	})

	t.Run("matching format stays verbatim", func(t *testing.T) {
		buf := &bufferedSSE{
			rawBytes:      []byte(openAIFrames),
			servingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "openai"},
		}
		out := p.convertBufferedForReplay(gw, buf, "openai", nil)
		if string(out) != openAIFrames {
			t.Fatalf("a matching format must replay verbatim:\n%s", out)
		}
	})
}

// TestReplayGuardedWritesClientFormat pins the wiring: the guarded replay
// writes the CONVERTED frames to the client, not the native buffer.
func TestReplayGuardedWritesClientFormat(t *testing.T) {
	gw := newReplayGateway(t)
	p := &Proxy{}
	p.StoreGateway(gw)

	buf := &bufferedSSE{
		rawBytes:      []byte(anthropicFrames),
		servingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "anthropic"},
	}
	w := httptest.NewRecorder()
	p.replayGuardedBufferedWithProvider(gw, w, buf, "prov", "test-agent", "openai", nil)

	body := w.Body.String()
	if !strings.Contains(body, `"choices"`) || strings.Contains(body, "message_start") {
		t.Fatalf("the client must receive OpenAI-shaped frames, got:\n%s", body)
	}
}
