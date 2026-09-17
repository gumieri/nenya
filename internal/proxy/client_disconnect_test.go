package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClientDisconnect_CancelsUpstreamMidStream is the NENYA-13 integration
// guarantee: a client that disconnects mid-stream must have its inbound
// request context canceled, which in turn cancels the upstream dispatch —
// no orphaned upstream inference (and no billing) keeps running.
func TestClientDisconnect_CancelsUpstreamMidStream(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, canFlush := w.(http.Flusher)
		if !canFlush {
			t.Error("upstream writer is not a flusher")
			return
		}

		// First chunk so the proxy commits headers and starts piping.
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		flusher.Flush()

		// Keep the stream open, ticking slowly; watch for the proxy
		// tearing the upstream request down when the client leaves.
		for i := 0; i < 250; i++ {
			select {
			case <-r.Context().Done():
				once.Do(func() { close(upstreamCanceled) })
				return
			case <-time.After(20 * time.Millisecond):
				_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"tick\"}}]}\n\n")
				flusher.Flush()
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newEarlyErrorProxy(t, []string{upstream.URL}, false)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		proxySrv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Content-Type", "application/json")

	resp, err := proxySrv.Client().Do(req)
	if err != nil {
		t.Fatalf("dispatch through proxy: %v", err)
	}

	// Read the first chunk so headers are committed and piping is active,
	// then abruptly disconnect, exactly like a dying opencode session.
	buf := make([]byte, 128)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		t.Fatalf("read first SSE chunk: %v", err)
	}
	cancelReq()
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	select {
	case <-upstreamCanceled:
		// Upstream observed the cancellation: nothing is orphaned.
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never observed client cancellation — orphaned inference")
	}
}
