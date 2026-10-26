package auth

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/nenya/config"
)

// recordingStorage captures the account pointer handed to SaveAccount and
// marshals it, as real persistence does.
type recordingStorage struct {
	mu  sync.Mutex
	got *config.ProviderAccount
}

func (s *recordingStorage) LoadAccounts(provider string) ([]*config.ProviderAccount, error) {
	return nil, nil
}

func (s *recordingStorage) SaveAccount(provider string, account *config.ProviderAccount) error {
	_, err := json.Marshal(account) // the racing read: iterates ModelLocks/BackoffLevels
	s.mu.Lock()
	s.got = account
	s.mu.Unlock()
	return err
}

// TestReportSuccessPersistsSnapshotNotLivePointer pins AUD-43 (NENYA-232):
// ReportSuccess must persist a SNAPSHOT of the account, not the live
// pointer. AccountSnapshot returns a deep copy safe to marshal while the
// pool keeps mutating live state
// already released, so marshaling it races any concurrent ApplyError /
// ReportSuccess map write. The ApplyError path has used AccountSnapshot for
// exactly this reason; ReportSuccess predates it.
func TestReportSuccessPersistsSnapshotNotLivePointer(t *testing.T) {
	storage := &recordingStorage{}
	m := NewAccountManager(storage)
	pool := NewAccountPool("prov", []*config.ProviderAccount{{ID: "acc-1"}})
	m.RegisterPool("prov", pool)

	// A model-scoped lock so the marshal actually iterates the maps.
	if err := m.ReportErrorForModel("prov", "acc-1", "glm-5.3", 429, "rate limit exceeded for model glm-5.3"); err != nil {
		t.Fatalf("ApplyErrorForModel: %v", err)
	}

	if err := m.ReportSuccess("prov", "acc-1"); err != nil {
		t.Fatalf("ReportSuccess: %v", err)
	}

	storage.mu.Lock()
	persisted := storage.got
	storage.mu.Unlock()
	if persisted == nil {
		t.Fatal("SaveAccount was never called")
	}
	live := pool.AccountSnapshot("acc-1")
	if persisted == live {
		t.Fatal("SaveAccount received the LIVE account pointer: marshaling it races concurrent map writers; ReportSuccess must persist a snapshot")
	}
}

// TestReportSuccessConcurrentMutationMarshal drives the race window for the
// race detector: map writes on one goroutine, snapshot-marshal persistence on
// another. Run with -race; pre-fix this flags the unsynchronized map read
// inside json.Marshal.
func TestReportSuccessConcurrentMutationMarshal(t *testing.T) {
	storage := &recordingStorage{}
	m := NewAccountManager(storage)
	pool := NewAccountPool("prov", []*config.ProviderAccount{{ID: "acc-1"}})
	m.RegisterPool("prov", pool)

	const iterations = 300
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // mutator: model-scoped lock writes
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = m.ReportErrorForModel("prov", "acc-1", "glm-5.3", 429, "rate limit exceeded for model glm-5.3")
		}
	}()
	go func() { // persister: snapshot + marshal
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = m.ReportSuccess("prov", "acc-1")
		}
	}()
	wg.Wait()
}
