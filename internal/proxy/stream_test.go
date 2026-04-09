package proxy

import (
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nenya/internal/config"
	"nenya/internal/gateway"
	"nenya/internal/infra"
	"nenya/internal/routing"
	providerpkg "nenya/internal/providers"
)

func TestWriteBlockedSSE(t *testing.T) {
	p := &Proxy{GW: newStreamTestGateway()}
	rec := httptest.NewRecorder()
	p.writeBlockedSSE(rec)

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
	p := &Proxy{GW: newStreamTestGateway()}
	rec := httptest.NewRecorder()
	p.writeBlockedSSE(rec)

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
	p := &Proxy{GW: newStreamTestGateway()}
	rec := httptest.NewRecorder()
	p.writeBlockedSSE(rec)

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

type blockingReader struct {
	ch chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	<-r.ch
	return 0, io.EOF
}

func newStreamTestGateway() *gateway.NenyaGateway {
	return &gateway.NenyaGateway{
		Config:     config.Config{},
		Logger:     slog.Default(),
		Stats:      infra.NewUsageTracker(),
		Metrics:    infra.NewMetrics(),
		AgentState: routing.NewAgentState(),
		Providers:  make(map[string]*config.Provider),
	}
}

func newStreamTestGatewayWithProviders(providers map[string]*config.Provider) *gateway.NenyaGateway {
	return &gateway.NenyaGateway{
		Config:     config.Config{},
		Logger:     slog.Default(),
		Stats:      infra.NewUsageTracker(),
		Metrics:    infra.NewMetrics(),
		AgentState: routing.NewAgentState(),
		Providers:  providers,
	}
}
