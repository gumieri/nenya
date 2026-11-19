package proxy

// NENYA-332 pins: the MCP replay conversion must propagate the request
// context into the transformer (trace propagation — convertReplayFrame drove
// it with context.Background()) and must NOT ship a silently broken
// conversion: a transformer frame (or held tail) the SSE-event decoder cannot
// parse aborts the whole conversion (ok=false → the caller falls back to the
// verbatim buffer), the same contract a transform error already has.

import (
	"context"
	"strings"
	"testing"

	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	mcpproxy "github.com/nenya/internal/proxy/mcp"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/stream"
)

// replayStubHost wraps the real host and substitutes one stub transformer.
type replayStubHost struct {
	mcpproxy.Host
	tr stream.ResponseTransformer
}

func (h *replayStubHost) ResolveTransformer(*gateway.NenyaGateway, routing.UpstreamTarget, string, infra.ThoughtSignatureKV) stream.ResponseTransformer {
	return h.tr
}

// recordingTransformer is a ResponseTransformer that hands back scripted
// frames verbatim and records the context it was driven with.
type recordingTransformer struct {
	lastCtx context.Context
	frames  [][]byte
}

func (t *recordingTransformer) TransformSSEChunk(ctx context.Context, _ []byte) ([]byte, error) {
	t.lastCtx = ctx
	if len(t.frames) == 0 {
		return nil, stream.ErrEventConsumed
	}
	f := t.frames[0]
	t.frames = t.frames[1:]
	return f, nil
}

type ctxKeyTest struct{}

// TestConvertBufferedForReplayConvertsWithCanceledContext pins the NENYA-332
// review fix: the replay fires exactly when the request ctx is already dead
// (loop deadline exhausted, dispatch failed on the deadline) — conversion of
// already-received bytes must NOT fail on cancellation (every production
// transformer rejects a canceled ctx; failing here silently degrades
// cross-format replays to verbatim bytes). Trace VALUES must survive.
func TestConvertBufferedForReplayConvertsWithCanceledContext(t *testing.T) {
	gw := newReplayGateway(t)
	p := &Proxy{}
	p.StoreGateway(gw)

	tr := &recordingTransformer{frames: [][]byte{[]byte(`{"id":"c1","choices":[]}`)}}
	host := &replayStubHost{Host: p.mcpHost(), tr: tr}

	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), ctxKeyTest{}, "request-sentinel"))
	cancel() // the deadline-exhausted replay runs after cancellation

	buf := &bufferedSSE{
		RawBytes:      []byte(anthropicFrames),
		ServingTarget: routing.UpstreamTarget{Provider: "prov", Model: "m", Format: "anthropic"},
	}
	out := mcpproxy.ConvertBufferedForReplay(ctx, host, gw, buf, "openai", nil)
	if string(out) == anthropicFrames {
		t.Fatalf("a canceled request ctx must not abort the conversion (verbatim fallback defeats the replay)")
	}
	if !strings.Contains(string(out), `"choices"`) {
		t.Fatalf("the canceled-ctx replay must still convert to the client format:\n%s", out)
	}
	if got, _ := tr.lastCtx.Value(ctxKeyTest{}).(string); got != "request-sentinel" {
		t.Fatalf("detached ctx lost the request values (trace propagation): %q", got)
	}
}

func TestConvertReplayFramesPropagatesRequestContext(t *testing.T) {
	tr := &recordingTransformer{frames: [][]byte{[]byte(`{"id":"c1","choices":[]}`)}}

	ctx := context.WithValue(context.Background(), ctxKeyTest{}, "request-sentinel")
	raw := []byte("data: {\"frame\": 1}\n\ndata: [DONE]\n\n")
	if _, ok := mcpproxy.ConvertReplayFrames(ctx, tr, raw); !ok {
		t.Fatalf("conversion failed")
	}
	if tr.lastCtx == nil {
		t.Fatal("the transformer was never driven")
	}
	if got, _ := tr.lastCtx.Value(ctxKeyTest{}).(string); got != "request-sentinel" {
		t.Fatalf("transformer received the wrong context (value %q) — the replay drives it with context.Background(), breaking trace propagation", got)
	}
}
