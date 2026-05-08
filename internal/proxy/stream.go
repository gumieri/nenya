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

	"nenya/config"
	"nenya/internal/gateway"
	providerpkg "nenya/internal/providers"
	"nenya/internal/routing"
	"nenya/internal/stream"
)

const (
	streamIdleTimeout = 60 * time.Second
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

type readResult struct {
	data []byte
	err  error
}

// stallReader wraps an io.Reader and detects stalls where no data is received
// within the configured timeout. It runs a background goroutine to read from
// the underlying source and signals stall detection via a channel.
type stallReader struct {
	mu        sync.Mutex
	timer     *time.Timer
	stalled   bool
	stallCh   chan struct{}
	closeOnce sync.Once
	ch        chan readResult
}

// newStallReader creates a stallReader that reads from src with the given timeout.
// The context is used to cancel the background read goroutine on shutdown.
func newStallReader(ctx context.Context, src io.Reader, timeout time.Duration) *stallReader {
	sr := &stallReader{
		stallCh: make(chan struct{}),
		ch:      make(chan readResult, 1),
	}
	sr.timer = time.AfterFunc(timeout, func() {
		sr.mu.Lock()
		sr.stalled = true
		sr.mu.Unlock()
		sr.closeOnce.Do(func() { close(sr.stallCh) })
	})
	go sr.readLoop(ctx, src)
	return sr
}

func (sr *stallReader) readLoop(ctx context.Context, src io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		var data []byte
		if n > 0 {
			data = make([]byte, n)
			copy(data, buf[:n])
		}
		select {
		case sr.ch <- readResult{data, err}:
		case <-ctx.Done():
			return
		}
		if err != nil {
			return
		}
	}
}

func (sr *stallReader) Read(p []byte) (int, error) {
	sr.mu.Lock()
	if sr.stalled {
		sr.mu.Unlock()
		return 0, errStreamStalled
	}
	sr.mu.Unlock()

	select {
	case <-sr.stallCh:
		return 0, errStreamStalled
	case rr := <-sr.ch:
		if len(rr.data) > 0 {
			sr.timer.Reset(streamIdleTimeout)
		}
		sr.mu.Lock()
		stalled := sr.stalled
		sr.mu.Unlock()
		if stalled {
			if len(rr.data) > 0 {
				n := copy(p, rr.data)
				return n, errStreamStalled
			}
			return 0, errStreamStalled
		}
		n := copy(p, rr.data)
		return n, rr.err
	}
}

// Stop stops the stall reader timer and marks the reader as stalled.
// Safe to call multiple times.
func (sr *stallReader) Stop() {
	sr.timer.Stop()
	sr.mu.Lock()
	if !sr.stalled {
		sr.stalled = true
		sr.mu.Unlock()
		sr.closeOnce.Do(func() { close(sr.stallCh) })
	} else {
		sr.mu.Unlock()
	}
}

// DrainPending reads any remaining buffered data from the reader with the given timeout.
// Returns the number of bytes drained and any error.
func (sr *stallReader) DrainPending(timeout time.Duration) (int, error) {
	sr.closeOnce.Do(func() { close(sr.stallCh) })
	select {
	case rr := <-sr.ch:
		return len(rr.data), rr.err
	case <-time.After(timeout):
		return 0, errors.New("stall reader drain timeout")
	}
}

var errStreamStalled = errors.New("stream stalled: no data received within idle timeout")

// streamResult carries the outcome of streamResponse back to the retry loop.
type streamResult struct {
	empty bool
}

// prefixedReadCloser returns a prefix buffer first, then delegates to the
// underlying reader. Used to prepend already-read bytes to a stream body.
type prefixedReadCloser struct {
	prefix []byte
	pos    int
	reader io.ReadCloser
}

func (p *prefixedReadCloser) Read(buf []byte) (int, error) {
	if p.pos < len(p.prefix) {
		n := copy(buf, p.prefix[p.pos:])
		p.pos += n
		return n, nil
	}
	return p.reader.Read(buf)
}

func (p *prefixedReadCloser) Close() error {
	return p.reader.Close()
}

type immediateFlushWriter struct {
	dst     http.ResponseWriter
	flusher http.Flusher
}

func newImmediateFlushWriter(w http.ResponseWriter) *immediateFlushWriter {
	fw, _ := newImmediateFlushWriterSafe(w)
	return fw
}

// newImmediateFlushWriterSafe returns an immediateFlushWriter if the response writer
// supports http.Flusher, otherwise returns nil. The boolean indicates success.
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

// copyStream copies data from src to dst using the provided buffer, respecting context cancellation.
// Returns the number of bytes copied and any error encountered.
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

// streamResponse handles streaming responses from upstream providers.
// It sets up SSE transformation, monitors for stalls, and streams the response to the client.
func (p *Proxy) streamResponse(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName string, action upstreamAction, cacheKey string, cooldownDuration time.Duration) streamResult {
	defer action.cancel()

	// When EmptyStreamAsError is enabled, probe the upstream body before writing
	// headers so we can detect empty streams and fall back to the next target
	// rather than sending an error SSE chunk to the client.
	if gw.Config.Governance.EmptyStreamAsError != nil && *gw.Config.Governance.EmptyStreamAsError {
		firstBuf := make([]byte, 4096)
		n, readErr := action.resp.Body.Read(firstBuf)

		if n == 0 {
			_ = action.resp.Body.Close()
			gw.AgentState.RecordFailure(target, cooldownDuration)
			gw.Metrics.RecordEmptyStream(target.Model, target.Provider)
			gw.Logger.Warn("empty upstream stream detected, falling back to next target",
				"model", target.Model, "provider", target.Provider)
			if readErr != nil && readErr != io.EOF {
				gw.Logger.Debug("upstream body read error", "err", readErr)
			}
			return streamResult{empty: true}
		}

		action.resp.Body = &prefixedReadCloser{
			prefix: firstBuf[:n],
			reader: action.resp.Body,
		}
	}

	routing.CopyHeaders(action.resp.Header, w.Header())
	if cacheKey != "" {
		w.Header().Set("X-Nenya-Cache-Status", "MISS")
	}
	w.WriteHeader(action.resp.StatusCode)

	transformingReader, contentBuilder, stallR := p.setupTransformingReader(gw, target, agentName, action, r.Context())
	if transformingReader == nil {
		return streamResult{}
	}

	buf := getStreamBuffer()
	flushWriter, canFlush := newImmediateFlushWriterSafe(w)
	dst, captureBuf, tee := p.setupStreamWriter(gw, flushWriter, canFlush, w, cacheKey)

	_, copyErr := copyStream(r.Context(), dst, transformingReader, *buf)

	p.handleStreamCompletion(gw, w, target, agentName, action, cacheKey, cooldownDuration, buf, copyErr, captureBuf, tee, contentBuilder, stallR)
	return streamResult{}
}

// setupTransformingReader creates and configures the SSE transforming reader, content builder, and stall reader.
// Returns the transforming reader for streaming, the content builder for post-processing, and the stall reader for cleanup.
func (p *Proxy) setupTransformingReader(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, action upstreamAction, ctx context.Context) (*stream.SSETransformingReader, *contentBuilder, *stallReader) {
	var transformer stream.ResponseTransformer
	switch target.Format {
	case "anthropic":
		transformer = stream.NewAnthropicTransformer()
		gw.Logger.Debug("SSE transformer active", "provider", target.Provider, "format", target.Format)
	case "gemini":
		if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
			transformer = spec.NewResponseTransformer(gw.ThoughtSigCache)
			if transformer != nil {
				gw.Logger.Debug("SSE transformer active", "provider", target.Provider)
			}
		}
	default:
		if spec, ok := providerpkg.Get(target.Provider); ok && spec.NewResponseTransformer != nil {
			transformer = spec.NewResponseTransformer(gw.ThoughtSigCache)
			if transformer != nil {
				gw.Logger.Debug("SSE transformer active", "provider", target.Provider)
			}
		}
	}

	stallR := newStallReader(ctx, action.resp.Body, streamIdleTimeout)

	transformingReader := stream.NewSSETransformingReader(stallR, transformer, ctx)
	transformingReader.SetOnUsage(p.makeUsageCallback(gw, target, agentName))
	transformingReader.SetObserver(newUpstreamErrorObserver(gw, target))

	p.setupStreamFilterIfEnabled(gw, transformingReader)
	p.setupStreamEntropyFilterIfEnabled(gw, transformingReader)
	contentBuilder := p.setupContentBuilderIfNeeded(gw, agentName, transformingReader)

	return transformingReader, contentBuilder, stallR
}

// upstreamErrorObserver is an SSE observer that detects error events within
// a 200 OK stream and records them as circuit breaker failures.
type upstreamErrorObserver struct {
	gw     *gateway.NenyaGateway
	target routing.UpstreamTarget
}

// newUpstreamErrorObserver creates an SSE observer that detects error events
// within a 200 OK stream and records them as circuit breaker failures.
func newUpstreamErrorObserver(gw *gateway.NenyaGateway, target routing.UpstreamTarget) *upstreamErrorObserver {
	return &upstreamErrorObserver{gw: gw, target: target}
}

func (o *upstreamErrorObserver) OnSSEEvent(event stream.SSEEvent) {
	if event.Type != "error" || o.gw == nil {
		return
	}
	o.gw.Logger.Warn("upstream error event detected in stream",
		"model", o.target.Model, "provider", o.target.Provider,
		"error_type", fmt.Sprintf("%v", event.Data["type"]),
		"error_message", fmt.Sprintf("%v", event.Data["message"]))
	o.gw.AgentState.RecordFailure(o.target, 0)
}

// OnStreamClose is called when the SSE stream closes.
func (o *upstreamErrorObserver) OnStreamClose(err error) {}

// makeUsageCallback returns a callback function that records token usage statistics.
// The callback is invoked by the SSE transformer when usage metadata is received.
func (p *Proxy) makeUsageCallback(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string) func(int, int, int, int, int) {
	return func(completion, prompt, total, cacheHit, cacheMiss int) {
		gw.Stats.RecordOutput(target.Model, completion)
		gw.Metrics.RecordTokens("output", target.Model, agentName, target.Provider, completion)
		if cacheHit > 0 {
			gw.Stats.RecordCacheHit(target.Model, cacheHit)
		}
		if cacheMiss > 0 {
			gw.Stats.RecordCacheMiss(target.Model, cacheMiss)
		}
		if gw.CostTracker != nil && (prompt > 0 || completion > 0) {
			if dm, ok := gw.ModelCatalog.Lookup(target.Model); ok && dm.Pricing != nil && !dm.Pricing.IsZero() {
				cost := dm.Pricing.CalculateCost(int64(prompt), int64(completion))
				gw.CostTracker.RecordUsage(target.Model, cost)
			}
		}
	}
}

func (p *Proxy) setupStreamFilterIfEnabled(gw *gateway.NenyaGateway, r *stream.SSETransformingReader) {
	if !gw.Config.Bouncer.RedactOutput {
		return
	}
	if len(gw.SecretPatterns) == 0 && len(gw.BlockedPatterns) == 0 {
		return
	}
	sf := stream.NewStreamFilter(gw.SecretPatterns, gw.BlockedPatterns, gw.Config.Bouncer.RedactionLabel, gw.Config.Bouncer.RedactOutputWindow)
	r.SetStreamFilter(sf)
	gw.Logger.Debug("stream filter active",
		"secret_patterns", len(gw.SecretPatterns),
		"block_patterns", len(gw.BlockedPatterns),
		"window_size", gw.Config.Bouncer.RedactOutputWindow)
}

func (p *Proxy) setupStreamEntropyFilterIfEnabled(gw *gateway.NenyaGateway, r *stream.SSETransformingReader) {
	if gw.EntropyFilter == nil || !gw.Config.Bouncer.RedactOutput {
		return
	}
	ef := stream.NewStreamEntropyFilter(
		gw.EntropyFilter.RedactHighEntropy,
		gw.Config.Bouncer.RedactionLabel,
		gw.Config.Bouncer.RedactOutputWindow,
	)
	r.SetStreamEntropyFilter(ef)
	gw.Logger.Debug("stream entropy filter active",
		"threshold", gw.Config.Bouncer.EntropyThreshold,
		"min_token", gw.Config.Bouncer.EntropyMinToken,
		"window_size", gw.Config.Bouncer.RedactOutputWindow)
}

func (p *Proxy) setupContentBuilderIfNeeded(gw *gateway.NenyaGateway, agentName string, r *stream.SSETransformingReader) *contentBuilder {
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
		return nil
	}
	cb := newContentBuilder()
	r.SetOnContent(cb.addContent)
	return cb
}

func (p *Proxy) setupStreamWriter(gw *gateway.NenyaGateway, flushWriter *immediateFlushWriter, canFlush bool, w http.ResponseWriter, cacheKey string) (io.Writer, *bytes.Buffer, *sseTeeWriter) {
	dst := io.Writer(flushWriter)
	if !canFlush {
		dst = w
	}

	var captureBuf *bytes.Buffer
	var tee *sseTeeWriter
	if cacheKey != "" && gw.ResponseCache != nil {
		captureBuf = new(bytes.Buffer)
		tee = &sseTeeWriter{
			dst:      flushWriter,
			buf:      captureBuf,
			maxBytes: gw.Config.ResponseCache.MaxEntryBytes,
		}
		dst = tee
	}

	return dst, captureBuf, tee
}

func (p *Proxy) handleStreamCompletion(gw *gateway.NenyaGateway, w http.ResponseWriter, target routing.UpstreamTarget, agentName string, action upstreamAction, cacheKey string, cooldownDuration time.Duration, buf *[]byte, copyErr error, captureBuf *bytes.Buffer, tee *sseTeeWriter, contentBuilder *contentBuilder, stallR *stallReader) {
	p.handleStreamDone(gw, w, target, agentName, action, cacheKey, cooldownDuration, buf, copyErr, captureBuf, tee, contentBuilder, stallR)
}

func (p *Proxy) handleStreamDone(gw *gateway.NenyaGateway, w http.ResponseWriter, target routing.UpstreamTarget, agentName string, action upstreamAction, cacheKey string, cooldownDuration time.Duration, buf *[]byte, copyErr error, captureBuf *bytes.Buffer, tee *sseTeeWriter, contentBuilder *contentBuilder, stallR *stallReader) {
	streamingBufPool.Put(buf)

	if errors.Is(copyErr, stream.ErrStreamBlocked) {
		action.cancel()
		gw.Logger.Warn("stream blocked by execution policy, upstream killed",
			"model", target.Model, "provider", target.Provider)
		gw.Metrics.RecordStreamBlock(target.Model, target.Provider)
		p.writeBlockedSSE(gw, w)
		_ = action.resp.Body.Close()
		return
	}

	if stallR != nil {
		_, _ = stallR.DrainPending(3 * time.Second)
	}

	if errors.Is(copyErr, errStreamStalled) {
		action.cancel()
		_ = action.resp.Body.Close()
		gw.Logger.Warn("stream stalled, aborting upstream",
			"model", target.Model, "provider", target.Provider,
			"idle_timeout", streamIdleTimeout)
		gw.Metrics.RecordStreamStall(target.Model, target.Provider)
		writeSSEError(w, http.StatusOK, "upstream stream stalled: no data received within idle timeout")
		return
	}

	_ = action.resp.Body.Close()

	if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		gw.Logger.Info("client disconnected, aborting upstream stream", "model", target.Model)
	}

	recordStreamResult(gw, target, agentName, cooldownDuration, copyErr)

	if copyErr == nil {
		storeStreamCache(gw, cacheKey, captureBuf, tee)
		p.asyncMCPAutoSave(gw, agentName, contentBuilder)
		return
	}
	if errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		return
	}
	writeSSEError(w, http.StatusOK, "upstream stream interrupted")
}

// recordStreamResult records the outcome of a streaming request to the agent state and metrics.
func recordStreamResult(gw *gateway.NenyaGateway, target routing.UpstreamTarget, agentName string, cooldownDuration time.Duration, copyErr error) {
	if copyErr == nil || errors.Is(copyErr, context.Canceled) || errors.Is(copyErr, context.DeadlineExceeded) {
		if copyErr != nil {
			gw.Logger.Debug("stream ended (client disconnect)", "model", target.Model, "provider", target.Provider)
		}
		gw.AgentState.RecordSuccess(target.CoolKey)
	} else {
		gw.Logger.Warn("stream copy error (upstream)",
			"model", target.Model, "provider", target.Provider, "err", copyErr)
		gw.AgentState.RecordFailure(target, cooldownDuration)
	}
}

func storeStreamCache(gw *gateway.NenyaGateway, cacheKey string, captureBuf *bytes.Buffer, tee *sseTeeWriter) {
	if cacheKey == "" || gw.ResponseCache == nil || tee == nil || tee.exceeded || captureBuf.Len() <= 0 {
		return
	}
	gw.ResponseCache.Store(cacheKey, captureBuf.Bytes())
	gw.Logger.Debug("response cache stored", "model", captureBuf.Len())
}

// writeBlockedSSE sends a blocked response SSE stream to the client.
// This is used when the execution policy blocks a request.
func (p *Proxy) writeBlockedSSE(gw *gateway.NenyaGateway, w http.ResponseWriter) {
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
		gw.Logger.Error("failed to marshal blocked SSE payload", "err", err)
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", blockJSON)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// autoSaveTryServer runs MCP auto-save on one server. It returns true if the save
// succeeded; false means the caller may try another server.
func (p *Proxy) autoSaveTryServer(liveGW *gateway.NenyaGateway, agent *config.AgentConfig, serverName, agentName, assistantContent string) bool {
	client, ok := liveGW.MCPClients[serverName]
	if !ok || !client.Ready() {
		return false
	}

	saveTool := agent.MCP.SaveTool
	if saveTool == "" {
		saveTool = p.discoverToolByPrefix(liveGW, serverName, "add")
		if saveTool == "" {
			saveTool = p.discoverToolByPrefix(liveGW, serverName, "save")
			if saveTool == "" {
				liveGW.Logger.Warn("MCP auto-save: no 'add'/'save' tool found on server",
					"server", serverName, "agent", agentName)
				return false
			}
		}
	}

	baseCtx := p.ShutdownCtx
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	saveCtx, cancel := context.WithTimeout(baseCtx, mcpExecTimeout)
	defer cancel()

	start := time.Now()
	_, err := client.CallTool(saveCtx, saveTool, map[string]any{
		"wing":     agentName,
		"room":     "conversation",
		"content":  assistantContent,
		"added_by": "nenya",
	})
	duration := time.Since(start)
	if err != nil {
		liveGW.Logger.Warn("MCP auto-save failed (best-effort)",
			"server", serverName, "agent", agentName, "err", err,
			"duration_ms", duration.Milliseconds())
		liveGW.Metrics.RecordMCPAutoSave(serverName, agentName, err)
		return false
	}
	liveGW.Logger.Debug("MCP auto-save completed",
		"server", serverName, "agent", agentName,
		"duration_ms", duration.Milliseconds(),
		"content_len", len(assistantContent))
	liveGW.Metrics.RecordMCPAutoSave(serverName, agentName, nil)
	return true
}

func (p *Proxy) asyncMCPAutoSave(gw *gateway.NenyaGateway, agentName string, contentBuilder *contentBuilder) {
	if agentName == "" || contentBuilder == nil {
		return
	}
	agent, ok := gw.Config.Agents[agentName]
	if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
		return
	}

	assistantContent := contentBuilder.build()
	if assistantContent == "" {
		return
	}

	go func() {
		liveGW := p.Gateway()
		if liveGW == nil {
			return
		}
		agent, ok := liveGW.Config.Agents[agentName]
		if !ok || agent.MCP == nil || !agent.MCP.AutoSave {
			return
		}

		for _, serverName := range agent.MCP.Servers {
			if p.autoSaveTryServer(liveGW, &agent, serverName, agentName, assistantContent) {
				return
			}
		}
	}()
}
