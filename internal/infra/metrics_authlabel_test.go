package infra

import (
	"sync"
	"testing"
)

// authLabelEntry finds the labeled entry for a key_name in an auth counter
// map, returning nil when absent.
func authLabelEntry(t *testing.T, m *sync.Map, keyName string) *labeledEntry {
	t.Helper()
	var found *labeledEntry
	m.Range(func(key, value any) bool {
		if e, ok := value.(*labeledEntry); ok && e.labels["key_name"] == keyName {
			found = e
			return false
		}
		return true
	})
	return found
}

func TestSetAuthLabelRedaction_ToggleOnWipesExistingEntries(t *testing.T) {
	m := NewMetrics()
	m.RecordAuthSuccess("api_key", "monitoring")
	m.IncAuthDenials("monitoring", "disabled")

	if authLabelEntry(t, &m.authSuccess, "monitoring") == nil {
		t.Fatal("expected pre-toggle entry with real key name")
	}

	m.SetAuthLabelRedaction(true)

	if authLabelEntry(t, &m.authSuccess, "monitoring") != nil {
		t.Error("toggle-on must wipe entries carrying real key names (authSuccess)")
	}
	if authLabelEntry(t, &m.authDenials, "monitoring") != nil {
		t.Error("toggle-on must wipe entries carrying real key names (authDenials)")
	}

	// Records after the toggle use the redacted label.
	m.RecordAuthSuccess("api_key", "monitoring")
	e := authLabelEntry(t, &m.authSuccess, RedactedAuthLabel)
	if e == nil {
		t.Fatal("expected post-toggle record under the redacted label")
	}
	if got := e.value.Load(); got != 1 {
		t.Errorf("post-toggle counter = %v, want 1", got)
	}
}

func TestSetAuthLabelRedaction_SameValueCallPreservesCounters(t *testing.T) {
	m := NewMetrics()
	m.SetAuthLabelRedaction(true)
	m.RecordAuthSuccess("api_key", "monitoring")
	m.SetAuthLabelRedaction(true) // idempotent: must not wipe

	e := authLabelEntry(t, &m.authSuccess, RedactedAuthLabel)
	if e == nil {
		t.Fatal("expected entry to survive an idempotent toggle")
	}
	if got := e.value.Load(); got != 1 {
		t.Errorf("counter = %v, want 1 (idempotent call must not reset)", got)
	}
}

func TestSetAuthLabelRedaction_ToggleOffWipesAndRestoresNames(t *testing.T) {
	m := NewMetrics()
	m.SetAuthLabelRedaction(true)
	m.RecordAuthSuccess("api_key", "monitoring")

	m.SetAuthLabelRedaction(false)

	if authLabelEntry(t, &m.authSuccess, RedactedAuthLabel) != nil {
		t.Error("toggle-off must wipe redacted entries")
	}
	m.RecordAuthSuccess("api_key", "monitoring")
	if authLabelEntry(t, &m.authSuccess, "monitoring") == nil {
		t.Error("post-toggle-off records must carry real key names")
	}
}

// TestSetAuthLabelRedaction_ConcurrentToggleVsRecord pins the invariant the
// authMu lock exists for: no record may land under a real key name while
// redaction is on, even under concurrent toggles. Run under -race in CI.
func TestSetAuthLabelRedaction_ConcurrentToggleVsRecord(t *testing.T) {
	m := NewMetrics()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.RecordAuthSuccess("api_key", "monitoring")
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		m.SetAuthLabelRedaction(true)
		if authLabelEntry(t, &m.authSuccess, "monitoring") != nil {
			close(stop)
			wg.Wait()
			t.Fatal("real key-name entry survived while redaction was on")
		}
		m.SetAuthLabelRedaction(false)
	}
	close(stop)
	wg.Wait()
}
