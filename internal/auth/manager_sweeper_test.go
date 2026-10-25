package auth

import (
	"testing"
	"time"

	"github.com/nenya/config"
)

// TestLockSweeperRemovesExpiredLocksOnly pins AUD-44 (NENYA-230): the
// sweeper removes expired model locks and their paired backoff levels while
// leaving unexpired ones alone. Pre-fix nothing in the codebase ever called
// CleanupExpiredLocks, so every (account, model) pair that ever received a
// model-scoped lock stayed in memory forever.
func TestLockSweeperRemovesExpiredLocksOnly(t *testing.T) {
	m := NewAccountManager(nil)
	pool := NewAccountPool("prov", []*config.ProviderAccount{{ID: "acc-1"}})
	m.RegisterPool("prov", pool)

	// Seed one expired and one live lock directly on the account.
	pool.mu.RLock()
	acc := pool.accounts[0]
	pool.mu.RUnlock()
	acc.ModelLocks = map[string]time.Time{
		"expired-model": time.Now().Add(-time.Hour),
		"live-model":    time.Now().Add(time.Hour),
	}
	acc.ModelBackoffLevels = map[string]int{
		"expired-model": 3,
		"live-model":    2,
	}

	sweeper := NewLockSweeper(m, 5*time.Millisecond)
	t.Cleanup(sweeper.Stop)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.RLock()
		_, expiredGone := acc.ModelLocks["expired-model"]
		_, liveStillThere := acc.ModelLocks["live-model"]
		_, backoffGone := acc.ModelBackoffLevels["expired-model"]
		pool.mu.RUnlock()
		if !expiredGone && !backoffGone && liveStillThere {
			// Expired pair removed, live lock untouched.
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("sweeper did not converge: locks=%v backoffs=%v", acc.ModelLocks, acc.ModelBackoffLevels)
}

// TestLockSweeperStopJoins pins the lifecycle: Stop ends the goroutine (a
// second Stop is a no-op), and Stopped reports the exit.
func TestLockSweeperStopJoins(t *testing.T) {
	m := NewAccountManager(nil)
	sweeper := NewLockSweeper(m, 5*time.Millisecond)
	if sweeper.Stopped() {
		t.Fatal("a fresh sweeper must be running")
	}
	sweeper.Stop()
	if !sweeper.Stopped() {
		t.Fatal("Stop must end the sweep goroutine")
	}
	sweeper.Stop() // must not panic or block
}

// TestLockSweeperDefaultInterval pins the interval fallback: a non-positive
// interval selects the one-minute default rather than a hot loop.
func TestLockSweeperDefaultInterval(t *testing.T) {
	m := NewAccountManager(nil)
	sweeper := NewLockSweeper(m, 0)
	t.Cleanup(sweeper.Stop)
	if sweeper.interval != DefaultLockSweepInterval {
		t.Fatalf("interval = %v, want the %v default", sweeper.interval, DefaultLockSweepInterval)
	}
}
