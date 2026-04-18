package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	providerpkg "nenya/internal/providers"
	"nenya/internal/routing"
	"nenya/internal/stream"
)

const (
	streamIdleTimeout = 120 * time.Second
	streamBufferSize  = 32 * 1024
)

var streamingBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, streamBufferSize)
		return &b
	},
}

func getStreamBuffer() *[]byte {
	buf := streamingBufPool.Get().(*[]byte)
	clear(*buf)
	return buf
}

type contentBuilder struct {
	buf strings.Builder
}

func newContentBuilder() *contentBuilder {
	return &contentBuilder{}
}

func (b *contentBuilder) addContent(s string) {
	b.buf.WriteString(s)
}

func (b *contentBuilder) build() string {
	return b.buf.String()
}

type stallReader struct {
	src     io.Reader
	mu      sync.Mutex
	timer   *time.Timer
	stalled bool
	stallCh chan struct{}
}

func newStallReader(src io.Reader, timeout time.Duration) *stallReader {
	sr := &stallReader{
		src:     src,
		stallCh: make(chan struct{}),
	}
	sr.timer = time.AfterFunc(timeout, func() {
		sr.mu.Lock()
		sr.stalled = true
		sr.mu.Unlock()
		close(sr.stallCh)
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

	type readResult struct {
		n   int
		err error
	}
	ch := make(chan readResult, 1)
	go func() {
		n, err := sr.src.Read(p)
		ch <- readResult{n, err}
	}()

	select {
	case <-sr.stallCh:
		return 0, errStreamStalled
	case rr := <-ch:
		if rr.n > 0 {
			sr.timer.Reset(streamIdleTimeout)
		}
		sr.mu.Lock()
		if sr.stalled {
			sr.mu.Unlock()
			if rr.n > 0 {
				return rr.n, errStreamStalled
			}
			return 0, errStreamStalled
		}
		sr.mu.Unlock()
		return rr.n, rr.err
	}
}

func (sr *stallReader) Stop() {
	sr.timer.Stop()
	sr.mu.Lock()
	if !sr.stalled {
		sr.stalled = true
		sr.mu.Unlock()
		close(sr.stallCh)
	} else {
		sr.mu.Unlock()
	}
}

var errStreamStalled = errors.New("stream stalled: no data received within idle timeout")

type immediateFlushWriter struct {
	dst     http.ResponseWriter
	flusher http.Flusher
}

func newImmediateFlushWriter(w http.ResponseWriter) *immediateFlushWriter {
	fw, _ := newImmediateFlushWriterSafe(w)
	return fw
}

func newImmediateFlushWriterSafe(w http.ResponseWriter) (*immediateFlushWriter, bool) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return &immediateFlushWriter{dst: w}, false
	}
	return &immediateFlushWriter{dst: w, flusher: flusher}, true
}

func (fw *immediateFlushWriter) Write(p []byte) (int, error) {
	n, err := fw.dst.Write(p)
	if err == nil && fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

func (fw *immediateFlushWriter) Header() http.Header {
	return fw.dst.Header()
}

func (fw *immediateFlushWriter) WriteHeader(statusCode int) {
	fw.dst.WriteHeader(statusCode)
}

type sseTeeWriter struct {
	dst      io.Writer
	buf      *bytes.Buffer
	maxBytes int64
	exceeded bool
}

func (t *sseTeeWriter) Write(p []byte) (int, error) {
	if !t.exceeded {
		if t.maxBytes > 0 && int64(t.buf.Len()+len(p)) > t.maxBytes {
			t.exceeded = true
		} else {
			t.buf.Write(p)
		}
	}
	return t.dst.Write(p)
}

func copyStream(ctx context.Context, dst io.Writer, src io.Reader, buf []byte) (int64, error) {
	if len(buf) == 0 {
		buf = make([]byte, streamBufferSize)
	}
	var written int64
	for {
		nr, rerr := src.Read(buf)
		if nr > 0 {
			nw, werr := dst.Write(buf[:nr])
			if werr != nil {
				return written, fmt.Errorf("writing to client: %w", werr)
			}
			if nr != nw {
				return written, io.ErrShortWrite
			}
			written += int64(nw)
		}
		if rerr != nil {
			if rerr == io.EOF {
				return written, nil
			}
			return written, fmt.Errorf("reading from upstream: %w", rerr)
		}
		if ctx.Err() != nil {
			return written, ctx.Err()
		}
	}
}

func (p *Proxy) streamResponse(w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName string, action upstreamAction, cacheKey string) {
	defer action.cancel()
	routing.CopyHeaders(action.resp.Header, w.Header())
	if cacheKey != "" {
		w.Header().Set("X-Nenya-Cache-Status", "MISS")
	}
	w.WriteHeader(action.resp.StatusCode)

	var transformer stream.ResponseTransformer
	if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
		transformer = spec.NewResponseTransformer(p.Gateway().ThoughtSigCache)
		if transformer != nil {
			p.Gateway().Logger.Debug("SSE transformer active", "provider", target.Provider)
		}
	}

	stallR := newStallReader(action.resp.Body, streamIdleTimeout)
	defer stallR.Stop()

	transformingReader := stream.NewSSETransformingReader(stallR, transformer)
	transformingReader.SetOnUsage(func(completion, prompt, total int) {
		p.Gateway().Stats.RecordOutput(target.Model, completion)
		if p.Gateway().Metrics != nil {
			p.Gateway().Metrics.RecordTokens("output", target.Model, agentName, target.Provider, completion)
		}
	})

	if p.Gateway().Config.SecurityFilter.OutputEnabled && (len(p.Gateway().SecretPatterns) > 0 || len(p.Gateway().BlockedPatterns) > 0) {
		sf := stream.NewStreamFilter(p.Gateway().SecretPatterns, p.Gateway().BlockedPatterns, p.Gateway().Config.SecurityFilter.RedactionLabel, p.Gateway().Config.SecurityFilter.OutputWindowChars)
		transformingReader.SetStreamFilter(sf)
		p.Gateway().Logger.Debug("stream filter active",
			"secret_patterns", len(p.Gateway().SecretPatterns),
			"block_patterns", len(p.Gateway().BlockedPatterns),
			"window_size", p.Gateway().Config.SecurityFilter.OutputWindowChars)
	}

	var contentBuilder *contentBuilder
	if agent, ok := p.Gateway().Config.Agents[agentName]; ok && agent.MCP != nil && agent.MCP.AutoSave {
		contentBuilder = newContentBuilder()
		transformingReader.SetOnContent(contentBuilder.addContent)
	}

	buf := getStreamBuffer()
	defer streamingBufPool.Put(buf)

	flushWriter, canFlush := newImmediateFlushWriterSafe(w)

	var captureBuf *bytes.Buffer
	var tee *sseTeeWriter
	if cacheKey != "" && p.Gateway().ResponseCache != nil {
		captureBuf = new(bytes.Buffer)
		tee = &sseTeeWriter{
			dst:      flushWriter,
			buf:      captureBuf,
			maxBytes: p.Gateway().Config.ResponseCache.MaxEntryBytes,
		}
	}

	dst := io.Writer(flushWriter)
	if !canFlush {
		dst = w
	}
	if tee != nil {
		dst = tee
	}

	var copyErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, copyErr = copyStream(r.Context(), dst, transformingReader, *buf)
	}()

	select {
	case <-done:
		if errors.Is(copyErr, stream.ErrStreamBlocked) {
			action.cancel()
			action.resp.Body.Close()
			p.Gateway().Logger.Warn("stream blocked by execution policy, upstream killed",
				"model", target.Model, "provider", target.Provider)
			if p.Gateway().Metrics != nil {
				p.Gateway().Metrics.RecordStreamBlock(target.Model, target.Provider)
			}
			p.writeBlockedSSE(w)
			return
		}
		if errors.Is(copyErr, errStreamStalled) {
			action.cancel()
			action.resp.Body.Close()
			p.Gateway().Logger.Warn("stream stalled, aborting upstream",
				"model", target.Model, "provider", target.Provider,
				"idle_timeout", streamIdleTimeout)
			return
		}
		if copyErr != nil {
			p.Gateway().Logger.Warn("stream copy error",
				"model", target.Model, "provider", target.Provider,
				"err", copyErr)
		}
		action.resp.Body.Close()
		p.Gateway().AgentState.RecordSuccess(target.CoolKey)

		if cacheKey != "" && p.Gateway().ResponseCache != nil && tee != nil && !tee.exceeded && captureBuf.Len() > 0 {
			p.Gateway().ResponseCache.Store(cacheKey, captureBuf.Bytes())
			p.Gateway().Logger.Debug("response cache stored",
				"model", target.Model, "size", captureBuf.Len())
		}

		p.asyncMCPAutoSave(agentName, contentBuilder)
	case <-r.Context().Done():
		p.Gateway().Logger.Info("client disconnected, aborting upstream stream", "model", target.Model)
		action.resp.Body.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			p.Gateway().Logger.Warn("timed out waiting for stream copy to finish after client disconnect", "model", target.Model)
		}
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
		p.Gateway().Logger.Error("failed to marshal blocked SSE payload", "err", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", blockJSON)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (p *Proxy) asyncMCPAutoSave(agentName string, contentBuilder *contentBuilder) {
	if agentName == "" || contentBuilder == nil {
		return
	}
	agent, ok := p.Gateway().Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
		return
	}

	assistantContent := contentBuilder.build()
	if assistantContent == "" {
		return
	}

	go func() {
		for _, serverName := range agent.MCP.Servers {
			client, ok := p.Gateway().MCPClients[serverName]
			if !ok || !client.Ready() {
				continue
			}

			saveTool := agent.MCP.SaveTool
			if saveTool == "" {
				saveTool = p.discoverToolByPrefix(serverName, "add")
				if saveTool == "" {
					saveTool = p.discoverToolByPrefix(serverName, "save")
					if saveTool == "" {
						p.Gateway().Logger.Warn("MCP auto-save: no 'add'/'save' tool found on server",
							"server", serverName, "agent", agentName)
						continue
					}
				}
			}

			ctx, cancel := context.WithTimeout(context.Background(), mcpExecTimeout)

			start := time.Now()
			_, err := client.CallTool(ctx, saveTool, map[string]any{
				"wing":     agentName,
				"room":     "conversation",
				"content":  assistantContent,
				"added_by": "nenya",
			})
			duration := time.Since(start)
			cancel()
			if err != nil {
				p.Gateway().Logger.Warn("MCP auto-save failed (best-effort)",
					"server", serverName, "agent", agentName, "err", err,
					"duration_ms", duration.Milliseconds())
				if p.Gateway().Metrics != nil {
					p.Gateway().Metrics.RecordMCPAutoSave(serverName, agentName, err)
				}
			} else {
				p.Gateway().Logger.Debug("MCP auto-save completed",
					"server", serverName, "agent", agentName,
					"duration_ms", duration.Milliseconds(),
					"content_len", len(assistantContent))
				if p.Gateway().Metrics != nil {
					p.Gateway().Metrics.RecordMCPAutoSave(serverName, agentName, nil)
				}
			}
			return
		}
	}()
}
