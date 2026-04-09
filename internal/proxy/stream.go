package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"nenya/internal/routing"
	"nenya/internal/stream"
	providerpkg "nenya/internal/providers"
)

const streamIdleTimeout = 120 * time.Second

type stallReader struct {
	src     io.Reader
	mu      sync.Mutex
	timer   *time.Timer
	stalled bool
}

func newStallReader(src io.Reader, timeout time.Duration) *stallReader {
	sr := &stallReader{src: src}
	sr.timer = time.AfterFunc(timeout, func() {
		sr.mu.Lock()
		sr.stalled = true
		sr.mu.Unlock()
	})
	return sr
}

func (sr *stallReader) Read(p []byte) (int, error) {
	sr.mu.Lock()
	if sr.stalled {
		sr.mu.Unlock()
		return 0, errStreamStalled
	}
	sr.mu.Unlock()

	n, err := sr.src.Read(p)
	if n > 0 {
		sr.timer.Reset(streamIdleTimeout)
	}
	return n, err
}

func (sr *stallReader) Stop() {
	sr.timer.Stop()
}

var errStreamStalled = errors.New("stream stalled: no data received within idle timeout")

func (p *Proxy) streamResponse(w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName string, action upstreamAction) {
	defer action.cancel()
	routing.CopyHeaders(action.resp.Header, w.Header())
	w.WriteHeader(action.resp.StatusCode)

	var transformer stream.ResponseTransformer
	if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
		transformer = spec.NewResponseTransformer(p.GW.ThoughtSigCache)
		if transformer != nil {
			p.GW.Logger.Debug("SSE transformer active", "provider", target.Provider)
		}
	}

	stallR := newStallReader(action.resp.Body, streamIdleTimeout)
	defer stallR.Stop()

	transformingReader := stream.NewSSETransformingReader(stallR, transformer)
	transformingReader.SetOnUsage(func(completion, prompt, total int) {
		p.GW.Stats.RecordOutput(target.Model, completion)
		if p.GW.Metrics != nil {
			p.GW.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, completion)
		}
	})

	if p.GW.Config.SecurityFilter.OutputEnabled && (len(p.GW.SecretPatterns) > 0 || len(p.GW.BlockedPatterns) > 0) {
		sf := stream.NewStreamFilter(p.GW.SecretPatterns, p.GW.BlockedPatterns, p.GW.Config.SecurityFilter.RedactionLabel, p.GW.Config.SecurityFilter.OutputWindowChars)
		transformingReader.SetStreamFilter(sf)
		p.GW.Logger.Debug("stream filter active",
			"secret_patterns", len(p.GW.SecretPatterns),
			"block_patterns", len(p.GW.BlockedPatterns),
			"window_size", p.GW.Config.SecurityFilter.OutputWindowChars)
	}

	var copyErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, copyErr = io.Copy(w, transformingReader)
	}()

	select {
	case <-done:
		if errors.Is(copyErr, stream.ErrStreamBlocked) {
			action.cancel()
			action.resp.Body.Close()
			p.GW.Logger.Warn("stream blocked by execution policy, upstream killed",
				"model", target.Model, "provider", target.Provider)
			if p.GW.Metrics != nil {
				p.GW.Metrics.RecordStreamBlock(target.Model, target.Provider)
			}
			p.writeBlockedSSE(w)
			return
		}
		if errors.Is(copyErr, errStreamStalled) {
			action.cancel()
			action.resp.Body.Close()
			p.GW.Logger.Warn("stream stalled, aborting upstream",
				"model", target.Model, "provider", target.Provider,
				"idle_timeout", streamIdleTimeout)
			return
		}
		action.resp.Body.Close()
	case <-r.Context().Done():
		p.GW.Logger.Info("client disconnected, aborting upstream stream", "model", target.Model)
		action.resp.Body.Close()
		<-done
	}
}

func (p *Proxy) writeBlockedSSE(w http.ResponseWriter) {
	blockPayload := map[string]interface{}{
		"id":     "blocked",
		"object": "chat.completion.chunk",
		"choices": []map[string]interface{}{
			{
				"index": 0,
				"delta": map[string]interface{}{
					"content": "[Response blocked by execution policy]",
				},
				"finish_reason": "stop",
			},
		},
	}
	blockJSON, err := json.Marshal(blockPayload)
	if err != nil {
		p.GW.Logger.Error("failed to marshal blocked SSE payload", "err", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", blockJSON)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}
