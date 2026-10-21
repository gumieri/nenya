package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDrain_InFlightStreamSurvives verifies the two NENYA-181 shutdown
// semantics against NENYA-53's teardown:
//
//  1. New work is rejected PROMPTLY during the drain window (the Shutdown
//     flag flips, ServeHTTP answers 503 + Retry-After before any routing).
//  2. An IN-FLIGHT upstream dispatch keeps its request context through
//     body-read completion: http.Server.Shutdown waits for the active
//     handler instead of canceling it, so the stream runs to its natural
//     end — no half-read body is aborted by the teardown. The drain is
//     bounded (the drain context), and a timed-out drain surfaces in the
//     exit code (cmd/nenya eventLoop).
//
// The upstream change this audit responds to closes downstream connections
// immediately on stop; nenya deliberately drains instead (bounded), which is
// strictly better for in-flight work and is what this test pins.
// Mutation-checked: replacing the graceful Shutdown with an immediate
// connection close fails this test with a truncated stream.
func TestDrain_InFlightStreamSurvives(t *testing.T) {
	// A streaming upstream that trickles chunks for ~300ms, then finishes
	// cleanly — long enough to straddle the drain initiation. firstChunk
	// closes when the dispatch has reached the upstream, i.e. mid-flight.
	firstChunk := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for i := 0; i < 5; i++ {
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"chunk%d\"}}]\n\n", i)
			flusher.Flush()
			if i == 0 {
				close(firstChunk)
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(60 * time.Millisecond):
			}
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	p := newEarlyErrorProxy(t, []string{upstream.URL}, false)
	proxySrv := httptest.NewUnstartedServer(p)
	proxySrv.Start()
	defer proxySrv.Close()
	// drainSrv is the real http.Server: Shutdown waits for active handlers
	// (the semantics under audit), unlike httptest.Server.Close. Its
	// timeouts differ from production buildServer (no ReadTimeout,
	// MaxHeaderBytes default) — irrelevant here: the audited semantics live
	// in http.Server.Shutdown itself, and WriteTimeout=0 matches prod.
	drainSrv := proxySrv.Config

	type streamResult struct {
		body string
		code int
		err  error
	}
	done := make(chan streamResult, 1)
	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
			proxySrv.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"test-agent","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			done <- streamResult{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := proxySrv.Client().Do(req)
		if err != nil {
			done <- streamResult{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		done <- streamResult{body: string(body), code: resp.StatusCode, err: err}
	}()

	// Wait until the stream is mid-flight (the dispatch reached the
	// upstream), then flip the drain flag and start the shutdown.
	select {
	case <-firstChunk:
	case <-time.After(5 * time.Second):
		t.Fatal("the dispatch never went upstream")
	}

	p.Shutdown.Store(true)

	// (1) New work is rejected immediately during the drain window.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("new requests during drain must get 503 + Retry-After, got %d", rec.Code)
	}

	// (2) The drain WAITS for the in-flight handler; the stream runs to its
	// natural end — the context is never canceled mid-body-read.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDrain()
	drainErr := drainSrv.Shutdown(drainCtx)
	if drainErr != nil {
		t.Fatalf("the drain must wait for the in-flight stream, not abandon it: %v", drainErr)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the in-flight stream must survive the drain: %v", r.err)
		}
		if r.code != http.StatusOK {
			t.Fatalf("the in-flight stream must complete normally, got %d: %q", r.code, r.body)
		}
		if !strings.Contains(r.body, "chunk4") || !strings.Contains(r.body, "[DONE]") {
			t.Fatalf("the stream must run to its natural end, truncated at: %q", r.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for the in-flight stream")
	}
}

// TestDrain_BoundedByDrainContext pins the other half of the NENYA-181
// contract: a handler that never returns does not hang the process — the
// drain returns its context's error at the bound while the stuck handler is
// abandoned (the eventLoop surfaces that in the exit code and proceeds to
// bounded gateway teardown).
func TestDrain_BoundedByDrainContext(t *testing.T) {
	release := make(chan struct{})
	handlerStarted := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(handlerStarted)
		<-release // stuck handler: never returns on its own
	}))
	srv.Start()
	// Defers run LIFO: release the stuck handler BEFORE Close (which waits
	// for outstanding requests).
	defer srv.Close()
	defer close(release)

	go func() {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/", nil)
		if err == nil {
			resp, err := srv.Client().Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}
		}
	}()

	select {
	case <-handlerStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never started")
	}

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelDrain()
	if err := srv.Config.Shutdown(drainCtx); err != context.DeadlineExceeded {
		t.Fatalf("the drain must be bounded by its context, got: %v", err)
	}
}
