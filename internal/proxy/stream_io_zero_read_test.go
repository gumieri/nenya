package proxy

// NENYA-332 pin: copyStream must make progress or fail — a reader violating
// the io.Reader contract by returning (0, nil) forever must not busy-spin.
// The guard bounds consecutive zero-length successful reads.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/nenya/internal/routing"
)

// zeroReadReader violates the io.Reader contract: (0, nil) forever.
type zeroReadReader struct{}

func (zeroReadReader) Read([]byte) (int, error) { return 0, nil }

func TestCopyStreamZeroReadProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := copyStream(ctx, io.Discard, zeroReadReader{}, make([]byte, 64))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("copyStream must fail on a (0,nil)-spinning reader, not exit cleanly")
		}
		if !errors.Is(err, errReaderNoProgress) {
			t.Fatalf("zero-read exhaustion must wrap the internal no-progress sentinel, got: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("copyStream busy-spun on a (0,nil) reader for 500ms — no progress guard")
	}
}

// TestRecordStreamResult_NoProgressSkipsProviderCharge pins the round-2
// classification fix: errReaderNoProgress is an INTERNAL reader-stack
// violation — it must not charge the provider's cooldown/breaker (a false
// failover signal), only telemetry + half-open release.
func TestRecordStreamResult_NoProgressSkipsProviderCharge(t *testing.T) {
	gw := newTestGateway(nil, nil)
	logBuf := &bytes.Buffer{}
	gw.Logger = slog.New(slog.NewTextHandler(logBuf, nil))
	target := routing.UpstreamTarget{Provider: "test-provider", Model: "test-model", CoolKey: "test-provider:test-model"}

	recordStreamResult(gw, streamOutcome{
		target:           target,
		agentName:        "no-progress-agent",
		cooldownDuration: time.Second,
		copyErr:          fmt.Errorf("reader made no progress: %w", errReaderNoProgress),
	})

	logs := logBuf.String()
	if !strings.Contains(logs, "internal: stream reader made no progress") {
		t.Fatalf("the internal no-progress branch must log its own signal; logs:\n%s", logs)
	}
	if strings.Contains(logs, "stream copy error (upstream)") {
		t.Fatalf("the no-progress error must not take the upstream charge path; logs:\n%s", logs)
	}
}
