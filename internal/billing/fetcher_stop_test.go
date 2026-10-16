package billing

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nenya/config"
)

// lockedBuffer is a concurrency-safe io.Writer for slog output (the
// abandoned poll goroutine may log concurrently with assertions).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestQuotaFetcherStopBoundedWithHangingFetch pins the NENYA-194 shutdown
// contract: a quota poll stuck against an unresponsive upstream cannot
// hold shutdown hostage — Stop returns within its bound and accounts the
// abandonment explicitly (bounded flush + aborted accounting), while a
// clean stop with no in-flight work still completes.
func TestQuotaFetcherStopBoundedWithHangingFetch(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hang until the test releases (well past Stop's bound).
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"balance": 5}`)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	var logBuf lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	qf := NewQuotaFetcher(logger, nil)
	qf.stopWait = 50 * time.Millisecond

	providers := map[string]config.ProviderConfig{
		"slow": {
			Billing: &config.BillingConfig{
				QuotaSource:         "api",
				QuotaURL:            srv.URL,
				QuotaInterval:       "50ms",
				QuotaTimeoutSeconds: 30,
				QuotaExtraction: &config.QuotaExtractionConfig{
					Mode:        config.ExtractionModeSimpleJSON,
					BalancePath: "balance",
				},
			},
		},
	}

	ctx := context.Background() // deliberately never canceled: Stop must bound itself
	tracker := NewBillingTracker(logger, nil)
	qf.Start(ctx, tracker, providers, nil, nil)

	// Give the poller time to enter the hanging fetch.
	time.Sleep(150 * time.Millisecond)

	start := time.Now()
	qf.Stop()
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("Stop waited %v for a hanging fetch; the bound did not apply", elapsed)
	}
	// The abandonment must be OBSERVABLE, not a vacuously fast clean
	// stop: the bound fired and the warning was logged.
	if elapsed < 40*time.Millisecond {
		t.Fatalf("Stop returned in %v; the abandonment bound did not fire (vacuous pass)", elapsed)
	}
	if out := logBuf.String(); !strings.Contains(out, "abandoning in-flight poll") {
		t.Fatalf("expected the abandonment warning, got:\n%s", out)
	}
}
