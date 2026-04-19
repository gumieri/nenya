package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	providerpkg "nenya/internal/providers"
	"nenya/internal/routing"
)

func TestWriteBlockedSSE(t *testing.T) {
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	rec := httptest.NewRecorder()
	p.writeBlockedSSE(p.Gateway(), rec)

	body := rec.Body.String()
	if !strings.HasPrefix(body, "data: {") {
		t.Fatalf("expected SSE data prefix, got: %s", body)
	}
	if !strings.Contains(body, `"id":"blocked"`) {
		t.Fatalf("expected blocked id in response, got: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("expected finish_reason stop, got: %s", body)
	}
	if !strings.Contains(body, "[Response blocked by execution policy]") {
		t.Fatalf("expected blocked content message, got: %s", body)
	}
	if !strings.Contains(body, "data: [DONE]\n") {
		t.Fatalf("expected SSE [DONE] sentinel, got: %s", body)
	}

	sseLines := strings.Split(strings.TrimSpace(body), "\n")
	if len(sseLines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(sseLines))
	}
	for _, line := range sseLines {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("all non-empty lines must start with 'data: ', got: %s", line)
		}
	}
}

func TestWriteBlockedSSE_Flush(t *testing.T) {
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	rec := httptest.NewRecorder()
	p.writeBlockedSSE(p.Gateway(), rec)

	if !rec.Flushed {
		t.Fatal("expected response to be flushed")
	}
}

func TestStallReader_ReadsNormally(t *testing.T) {
	src := strings.NewReader("hello world")
	sr := newStallReader(src, 50*time.Millisecond)
	defer sr.Stop()

	buf := make([]byte, 11)
	n, err := sr.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 11 {
		t.Fatalf("expected 11 bytes, got %d", n)
	}
	if string(buf) != "hello world" {
		t.Fatalf("expected 'hello world', got '%s'", string(buf))
	}
}

func TestStallReader_StallsAfterTimeout(t *testing.T) {
	blockCh := make(chan struct{})
	src := &blockingReader{ch: blockCh}

	sr := newStallReader(src, 30*time.Millisecond)
	defer sr.Stop()

	time.Sleep(50 * time.Millisecond)

	buf := make([]byte, 1024)
	n, err := sr.Read(buf)
	if !errors.Is(err, errStreamStalled) {
		t.Fatalf("expected errStreamStalled, got: %v (n=%d)", err, n)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes, got %d", n)
	}

	close(blockCh)
}

func TestStallReader_ResetOnRead(t *testing.T) {
	data := "chunk1"
	src := strings.NewReader(data)
	sr := newStallReader(src, 30*time.Millisecond)
	defer sr.Stop()

	buf := make([]byte, 1024)
	_, err := sr.Read(buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	buf2 := make([]byte, 1024)
	n, err := sr.Read(buf2)
	if err != io.EOF {
		t.Fatalf("expected EOF, got: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes at EOF, got %d", n)
	}
}

func TestResponseTransformer_GeminiProvider(t *testing.T) {
	spec, ok := providerpkg.Get("gemini")
	if !ok {
		t.Fatal("expected gemini provider to exist")
	}

	if spec.NewResponseTransformer == nil {
		t.Fatal("expected gemini to have NewResponseTransformer")
	}

	transformer := spec.NewResponseTransformer(nil)
	if transformer == nil {
		t.Fatal("expected non-nil transformer for gemini provider")
	}

	if _, ok := transformer.(*providerpkg.GeminiTransformer); !ok {
		t.Fatal("expected GeminiTransformer type")
	}
}

func TestResponseTransformer_NonGeminiProvider(t *testing.T) {
	spec, ok := providerpkg.Get("openai")
	if !ok {
		t.Fatal("expected openai provider to exist")
	}

	if spec.NewResponseTransformer == nil {
		return
	}

	transformer := spec.NewResponseTransformer(nil)
	if transformer != nil {
		t.Fatal("expected nil transformer for openai provider")
	}
}

func TestResponseTransformer_UnknownProvider(t *testing.T) {
	_, ok := providerpkg.Get("nonexistent")
	if ok {
		t.Fatal("expected false for nonexistent provider")
	}
}

func TestErrStreamStalled(t *testing.T) {
	if errStreamStalled == nil {
		t.Fatal("errStreamStalled should not be nil")
	}
	if errStreamStalled.Error() != "stream stalled: no data received within idle timeout" {
		t.Fatalf("unexpected error message: %s", errStreamStalled.Error())
	}
}

func TestStallReader_StopPreventsStall(t *testing.T) {
	blockCh := make(chan struct{})
	src := &blockingReader{ch: blockCh}

	sr := newStallReader(src, 30*time.Millisecond)
	sr.Stop()

	time.Sleep(50 * time.Millisecond)

	buf := make([]byte, 1024)
	n, err := sr.Read(buf)
	if !errors.Is(err, errStreamStalled) {
		t.Fatalf("expected errStreamStalled even after stop (timer may have fired), got: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 bytes, got %d", n)
	}

	close(blockCh)
}

func TestWriteBlockedSSE_MultipleChunks(t *testing.T) {
	p := &Proxy{}
	p.StoreGateway(newStreamTestGateway())
	rec := httptest.NewRecorder()
	p.writeBlockedSSE(p.Gateway(), rec)

	count := strings.Count(rec.Body.String(), "data: ")
	if count != 2 {
		t.Fatalf("expected 2 SSE data lines, got %d", count)
	}
}

func TestResponseTransformer_GeminiTransformerHasOnExtraContent(t *testing.T) {
	spec, ok := providerpkg.Get("gemini")
	if !ok {
		t.Fatal("expected gemini provider to exist")
	}

	if spec.NewResponseTransformer == nil {
		t.Fatal("expected gemini to have NewResponseTransformer")
	}

	transformer := spec.NewResponseTransformer(nil)
	gt, ok := transformer.(*providerpkg.GeminiTransformer)
	if !ok {
		t.Fatal("expected GeminiTransformer")
	}
	if gt.OnExtraContent == nil {
		t.Fatal("expected OnExtraContent callback to be set")
	}
}

func TestStallReader_ConcurrentReadAndStall(t *testing.T) {
	blockCh := make(chan struct{})
	src := &blockingReader{ch: blockCh}

	sr := newStallReader(src, 20*time.Millisecond)
	defer sr.Stop()

	doneCh := make(chan error, 1)
	go func() {
		buf := make([]byte, 1024)
		_, err := sr.Read(buf)
		doneCh <- err
	}()

	select {
	case err := <-doneCh:
		if !errors.Is(err, errStreamStalled) {
			t.Fatalf("expected errStreamStalled, got: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timeout waiting for stall")
	}

	close(blockCh)
}

// --- streamingBufPool tests ---

func TestStreamingBufPool_ReturnsCorrectSize(t *testing.T) {
	buf := streamingBufPool.Get().(*[]byte)
	defer streamingBufPool.Put(buf)

	if len(*buf) != streamBufferSize {
		t.Fatalf("expected buffer size %d, got %d", streamBufferSize, len(*buf))
	}
}

func TestStreamingBufPool_ReturnsCleanBuffer(t *testing.T) {
	buf := streamingBufPool.Get().(*[]byte)
	copy(*buf, "dirty data from previous use")
	streamingBufPool.Put(buf)

	buf2 := getStreamBuffer()
	defer streamingBufPool.Put(buf2)

	for i, b := range *buf2 {
		if b != 0 {
			t.Fatalf("expected zeroed buffer at byte %d, got %q", i, b)
		}
	}
}

func TestStreamingBufPool_ConcurrentSafe(t *testing.T) {
	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			buf := streamingBufPool.Get().(*[]byte)
			if len(*buf) != streamBufferSize {
				t.Errorf("wrong buffer size: %d", len(*buf))
			}
			streamingBufPool.Put(buf)
		}()
	}

	wg.Wait()
}

// --- immediateFlushWriter tests ---

func TestImmediateFlushWriter_FlushesOnEveryWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newImmediateFlushWriter(rec)

	_, err := fw.Write([]byte("chunk1"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !rec.Flushed {
		t.Fatal("expected flush after first Write")
	}

	_, err = fw.Write([]byte("chunk2"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if rec.Body.String() != "chunk1chunk2" {
		t.Fatalf("expected 'chunk1chunk2', got %q", rec.Body.String())
	}
}

func TestImmediateFlushWriter_PropagatesHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newImmediateFlushWriter(rec)

	fw.Header().Set("X-Custom", "test")
	if rec.Header().Get("X-Custom") != "test" {
		t.Fatal("Header() should delegate to underlying ResponseWriter")
	}
}

func TestImmediateFlushWriter_WriteHeader(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newImmediateFlushWriter(rec)

	fw.WriteHeader(http.StatusCreated)
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected status %d, got %d", http.StatusCreated, rec.Code)
	}
}

func TestImmediateFlushWriter_WriteErrorNoFlush(t *testing.T) {
	broken := &brokenWriter{err: io.ErrClosedPipe}
	fw := newImmediateFlushWriter(broken)

	_, err := fw.Write([]byte("data"))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected io.ErrClosedPipe, got: %v", err)
	}
	if atomic.LoadInt32(&broken.flushCount) > 0 {
		t.Fatal("Flush should not be called when Write fails")
	}
}

// --- copyStream tests ---

func TestCopyStream_NormalCopy(t *testing.T) {
	src := strings.NewReader("hello world")
	dst := &bytesCapture{}

	written, err := copyStream(context.Background(), dst, src, make([]byte, 4))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != 11 {
		t.Fatalf("expected 11 bytes written, got %d", written)
	}
	if dst.String() != "hello world" {
		t.Fatalf("expected 'hello world', got %q", dst.String())
	}
}

func TestCopyStream_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	src := &contextReader{ctx: ctx}
	dst := &bytesCapture{}

	errCh := make(chan error, 1)
	go func() {
		_, err := copyStream(ctx, dst, src, make([]byte, 1024))
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copyStream did not return after context cancellation")
	}
}

func TestCopyStream_UpstreamReadError(t *testing.T) {
	readErr := fmt.Errorf("connection reset")
	src := &errorReader{err: readErr}
	dst := &bytesCapture{}

	_, err := copyStream(context.Background(), dst, src, make([]byte, 1024))
	if err == nil {
		t.Fatal("expected error from upstream read failure")
	}
	if !strings.Contains(err.Error(), "reading from upstream") {
		t.Fatalf("expected 'reading from upstream' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("expected wrapped upstream error, got: %v", err)
	}
}

func TestCopyStream_ClientWriteError(t *testing.T) {
	writeErr := fmt.Errorf("broken pipe")
	src := strings.NewReader("some data that should fail")
	dst := &errorWriter{err: writeErr}

	_, err := copyStream(context.Background(), dst, src, make([]byte, 4))
	if err == nil {
		t.Fatal("expected error from client write failure")
	}
	if !strings.Contains(err.Error(), "writing to client") {
		t.Fatalf("expected 'writing to client' error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "broken pipe") {
		t.Fatalf("expected wrapped write error, got: %v", err)
	}
}

func TestCopyStream_EmptyBuffer(t *testing.T) {
	src := strings.NewReader("data")
	dst := &bytesCapture{}

	written, err := copyStream(context.Background(), dst, src, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != 4 {
		t.Fatalf("expected 4 bytes, got %d", written)
	}
	if dst.String() != "data" {
		t.Fatalf("expected 'data', got %q", dst.String())
	}
}

func TestCopyStream_LargeDataExceedsBuffer(t *testing.T) {
	data := strings.Repeat("A", streamBufferSize*3)
	src := strings.NewReader(data)
	dst := &bytesCapture{}

	written, err := copyStream(context.Background(), dst, src, make([]byte, streamBufferSize))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != int64(len(data)) {
		t.Fatalf("expected %d bytes, got %d", len(data), written)
	}
	if dst.String() != data {
		t.Fatalf("data mismatch: got %d bytes", len(dst.String()))
	}
}

func TestCopyStream_EOFOnRead(t *testing.T) {
	src := strings.NewReader("end")
	dst := &bytesCapture{}

	written, err := copyStream(context.Background(), dst, src, make([]byte, 1024))
	if err != nil {
		t.Fatalf("unexpected error on EOF: %v", err)
	}
	if written != 3 {
		t.Fatalf("expected 3 bytes, got %d", written)
	}
}

// --- integration: copyStream + immediateFlushWriter ---

func TestCopyStream_WithFlushWriter_Integration(t *testing.T) {
	sseData := "data: {\"chunk\":1}\n\ndata: {\"chunk\":2}\n\ndata: [DONE]\n\n"
	src := strings.NewReader(sseData)
	rec := httptest.NewRecorder()
	fw := newImmediateFlushWriter(rec)

	written, err := copyStream(context.Background(), fw, src, make([]byte, 8))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != int64(len(sseData)) {
		t.Fatalf("expected %d bytes, got %d", len(sseData), written)
	}
	if rec.Body.String() != sseData {
		t.Fatalf("SSE data mismatch:\nwant: %q\ngot:  %q", sseData, rec.Body.String())
	}
	if !rec.Flushed {
		t.Fatal("response should be flushed after SSE streaming")
	}
}

func TestCopyStream_ContextCancelKillsSlowUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slowSrc := &slowContextReader{ctx: ctx, chunk: "data: {\"ok\":1}\n\n", delay: 50 * time.Millisecond}
	dst := &bytesCapture{}

	errCh := make(chan error, 1)
	go func() {
		_, err := copyStream(ctx, dst, slowSrc, make([]byte, 1024))
		errCh <- err
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("copyStream did not abort on context cancellation")
	}
}

// --- test helpers ---

type blockingReader struct {
	ch chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.ch
	return 0, io.EOF
}

type bytesCapture struct {
	buf []byte
}

func (b *bytesCapture) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *bytesCapture) String() string {
	return string(b.buf)
}

type brokenWriter struct {
	err        error
	flushCount int32
}

func (w *brokenWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

func (w *brokenWriter) Flush() {
	atomic.AddInt32(&w.flushCount, 1)
}

func (w *brokenWriter) Header() http.Header {
	return http.Header{}
}

func (w *brokenWriter) WriteHeader(statusCode int) {}

type errorReader struct {
	err error
}

func (r *errorReader) Read(p []byte) (int, error) {
	return 0, r.err
}

type errorWriter struct {
	err error
}

func (w *errorWriter) Write(p []byte) (int, error) {
	return 0, w.err
}

type contextReader struct {
	ctx context.Context
}

func (r *contextReader) Read(p []byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

type slowContextReader struct {
	ctx   context.Context
	chunk string
	delay time.Duration
	done  bool
}

func (r *slowContextReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(r.delay):
	}
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	n := copy(p, r.chunk)
	return n, nil
}

func newStreamTestGateway() *gateway.NenyaGateway {
	return &gateway.NenyaGateway{
		Config:     config.Config{},
		Logger:     slog.Default(),
		Stats:      infra.NewUsageTracker(),
		Metrics:    infra.NewMetrics(),
		AgentState: routing.NewAgentState(slog.Default()),
		Providers:  make(map[string]*config.Provider),
	}
}
