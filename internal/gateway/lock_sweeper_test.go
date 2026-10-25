package gateway

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
)

// TestGatewayLockSweeperLifecycle pins the AUD-44 wiring (NENYA-230): New
// starts the model-lock sweeper, Close stops it. Pre-fix no code path ever
// ran the existing CleanupExpiredLocks, so Account.ModelLocks /
// ModelBackoffLevels grew without bound for the life of the process — every
// (account, model) pair that ever received a model-scoped lock stayed
// forever, and was re-paid on every account-state snapshot.
func TestGatewayLockSweeperLifecycle(t *testing.T) {
	cfg := testutil.MinimalConfig()
	secrets := &config.SecretsConfig{ClientToken: "test-token"}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	gw := New(context.Background(), *cfg, secrets, logger)
	if gw.lockSweeper == nil {
		t.Fatal("New must start the model-lock sweeper (AUD-44): without it ModelLocks/BackoffLevels grow unbounded")
	}
	if gw.lockSweeper.Stopped() {
		t.Fatal("a freshly built gateway's sweeper must be running")
	}

	gw.Close()
	if !gw.lockSweeper.Stopped() {
		t.Fatal("Close must stop the model-lock sweeper")
	}
	// Close must stay idempotent: the reload drain path and Shutdown can both
	// reach it.
	gw.Close()
}
