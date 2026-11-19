package proxy

// NENYA-332 pins: a transformer frame (or held tail) the SSE-event decoder
// cannot parse must abort the whole replay conversion (ok=false -> the
// caller falls back to the verbatim buffer), the same contract a transform
// error already has. Pre-fix the decoder error was swallowed: the truncated
// conversion shipped silently.

import (
	"context"
	"testing"

	mcpproxy "github.com/nenya/internal/proxy/mcp"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

func TestConvertBufferedForReplayFallsBackOnMalformedTransformerFrame(t *testing.T) {
	gw := newReplayGateway(t)
	p := &Proxy{}
	p.StoreGateway(gw)

	buf := &bufferedSSE{
		RawBytes:      []byte(anthropicFrames),
		ServingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "anthropic"},
	}
	// The transformer "converts" the first frame into bytes no SSE-event
	// decoder can split — shipping them would corrupt the client stream.
	tr := &recordingTransformer{frames: [][]byte{[]byte("not json { at all")}}
	host := &replayStubHost{Host: p.mcpHost(), tr: tr}

	out := mcpproxy.ConvertBufferedForReplay(context.Background(), host, gw, buf, "openai", nil)
	if string(out) != anthropicFrames {
		t.Fatalf("a malformed converted frame must abort the whole conversion (verbatim fallback), got:\n%s", out)
	}
}

// malformedTailTransformer emits a held tail that no decoder can parse.
type malformedTailTransformer struct {
	stream.ResponseTransformer
	tail []byte
}

func (t *malformedTailTransformer) FinishTail() []byte { return t.tail }

func TestConvertBufferedForReplayFallsBackOnMalformedTail(t *testing.T) {
	gw := newReplayGateway(t)
	p := &Proxy{}
	p.StoreGateway(gw)

	buf := &bufferedSSE{
		RawBytes:      []byte(anthropicFrames),
		ServingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "anthropic"},
	}
	tr := &malformedTailTransformer{
		ResponseTransformer: &recordingTransformer{frames: [][]byte{[]byte(`{"id":"c1","choices":[]}`)}},
		tail:                []byte("garbage tail {"),
	}
	host := &replayStubHost{Host: p.mcpHost(), tr: tr}

	out := mcpproxy.ConvertBufferedForReplay(context.Background(), host, gw, buf, "openai", nil)
	if string(out) != anthropicFrames {
		t.Fatalf("a malformed held tail must abort the whole conversion (verbatim fallback), got:\n%s", out)
	}
}
