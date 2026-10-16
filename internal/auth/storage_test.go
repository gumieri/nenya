package auth

import (
	"context"
	"testing"
	"time"

	"github.com/nenya/config"
)

// TestJSONFileStorageDeadlineRoundTrip pins the NENYA-170 persistence
// audit: cooldown deadlines set from time.Now() carry Go's monotonic
// clock reading; the JSON round-trip must preserve the wall deadline
// (RFC3339 strips the monotonic reading by construction) and an
// expired-on-load deadline must reconcile to servable.
func TestJSONFileStorageDeadlineRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := NewJSONFileStorage(dir)

	now := time.Now()
	live := &config.ProviderAccount{
		ID:               "acc-live",
		Credential:       "cred",
		Status:           config.AccountStatusActive,
		RateLimitedUntil: now.Add(time.Hour), // carries the monotonic reading
		ModelLocks: map[string]time.Time{
			"m1": now.Add(30 * time.Minute),
		},
		LastUsed: now,
	}
	if err := s.SaveAccount("gateway", live); err != nil {
		t.Fatal(err)
	}
	expired := &config.ProviderAccount{
		ID:               "acc-expired",
		Credential:       "cred",
		Status:           config.AccountStatusActive,
		RateLimitedUntil: now.Add(-time.Minute), // expired while persisted
	}
	if err := s.SaveAccount("gateway", expired); err != nil {
		t.Fatal(err)
	}

	loaded, err := s.LoadAccounts("gateway")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(loaded))
	}
	byID := map[string]*config.ProviderAccount{}
	for _, acc := range loaded {
		byID[acc.ID] = acc
	}

	got := byID["acc-live"]
	if got == nil || !got.RateLimitedUntil.Equal(live.RateLimitedUntil) {
		t.Fatalf("live deadline did not round-trip: %v", got)
	}
	if ml := got.ModelLocks["m1"]; !ml.Equal(live.ModelLocks["m1"]) {
		t.Fatalf("model lock deadline did not round-trip: %v", ml)
	}

	exp := byID["acc-expired"]
	if exp == nil {
		t.Fatal("expired account missing")
	}
	if !exp.RateLimitedUntil.Before(time.Now()) {
		t.Fatal("expired deadline must load as past wall time")
	}

	// The loaded state must be servable according to wall time: an
	// account whose persisted deadline expired is not cooling.
	pool := NewAccountPool("gateway", loaded)
	if _, err := pool.SelectAccountByID(context.Background(), "acc-expired", "m"); err != nil {
		t.Fatalf("expired-on-load deadline must reconcile to servable: %v", err)
	}
	if _, err := pool.SelectAccountByID(context.Background(), "acc-live", "m"); err == nil {
		t.Fatal("live persisted deadline must still bench the account")
	}
}
