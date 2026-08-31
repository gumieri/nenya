package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/nenya/internal/infra"
)

const (
	// DefaultSessionCap is the default maximum number of tracked session pins
	// before the least-recently-seen pins are evicted.
	DefaultSessionCap = 512
	// DefaultSessionTTL is the default idle timeout for a session pin.
	DefaultSessionTTL = 1 * time.Hour
)

// SessionState is a single pinned routing target for one session.
// It stores only identifiers (provider, model, account ID) and the agent's
// configured idle TTL — credentials are re-resolved per request via the
// AccountSelector, never retained here.
type SessionState struct {
	Provider string
	Model    string
	Account  string
	ttl      time.Duration
	Since    time.Time
	LastSeen time.Time
}

// SessionRouter pins agent sessions to a provider/model combination to
// preserve provider-side prefix-cache warmth across turns. It is safe for
// concurrent use; all methods are mutex-guarded.
type SessionRouter struct {
	mu       sync.Mutex
	sessions map[string]*SessionState
	cap      int
	ttl      time.Duration
	now      func() time.Time
	metrics  *infra.Metrics
	logger   *slog.Logger
}

// NewSessionRouter creates a SessionRouter with the given LRU cap and idle
// TTL. A non-positive cap or TTL falls back to the package defaults.
func NewSessionRouter(cap int, ttl time.Duration, metrics *infra.Metrics, logger *slog.Logger) *SessionRouter {
	if cap <= 0 {
		cap = DefaultSessionCap
	}
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	return &SessionRouter{
		sessions: make(map[string]*SessionState),
		cap:      cap,
		ttl:      ttl,
		now:      time.Now,
		metrics:  metrics,
		logger:   logger,
	}
}

// SessionKey derives a stable session identifier from the agent name, the
// system prompt, and the first user message. The system prompt and first
// user message are taken from the raw client payload (before nenya's
// content pipeline runs), so gateway-side compaction does not destabilize
// the key within a session.
func SessionKey(agent, systemPrompt, firstUserMessage string) string {
	h := sha256.New()
	h.Write([]byte(agent))
	h.Write([]byte{0})
	h.Write([]byte(systemPrompt))
	h.Write([]byte{0})
	h.Write([]byte(firstUserMessage))
	return hex.EncodeToString(h.Sum(nil))
}

// Pin records the pinned target for a BRAND-NEW session key with the given
// per-agent idle TTL (non-positive falls back to the router default). It
// respects the LRU cap and evicts the least-recently-seen entry when the
// store is full, recording a `new` metric. Callers should use Lookup for
// existing sessions and PromoteIfChanged after a failover.
func (r *SessionRouter) Pin(key, provider, model, account string, ttl time.Duration) {
	if r == nil {
		return
	}
	if ttl <= 0 {
		ttl = r.ttl
	}
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = make(map[string]*SessionState)
	}
	// Timestamps are assigned under the lock so LastSeen reflects lock
	// acquisition order; without this, two goroutines capturing time.Now()
	// before locking can order the LRU incorrectly and evict an entry that
	// was pinned more recently.
	now := r.now()
	entry := &SessionState{
		Provider: provider,
		Model:    model,
		Account:  account,
		ttl:      ttl,
		Since:    now,
		LastSeen: now,
	}
	_, exists := r.sessions[key]
	if !exists && len(r.sessions) >= r.cap {
		r.evictLRU()
	}
	r.sessions[key] = entry
	r.mu.Unlock()

	// Only a brand-new key counts as "new"; overwriting an existing key is a
	// caller bookkeeping detail, not a pin-creation event.
	if !exists {
		r.record("new")
	}
	if r.logger != nil {
		r.logger.Debug("session pin created", "key", key, "provider", provider, "model", model, "account", account, "ttl", ttl)
	}
}

// Lookup returns a copy of the pinned target for the given session key if it
// exists and has not expired by its idle TTL. A successful lookup refreshes
// LastSeen so active sessions stay warm. A returned pin is only advisory:
// pin validity (active vs cooling, context fit) is re-checked against the
// freshly built target list on every request.
func (r *SessionRouter) Lookup(key string) (SessionState, bool) {
	if r == nil {
		return SessionState{}, false
	}
	// The timestamp is captured inside the lock so LastSeen reflects lock
	// acquisition order (same invariant as Pin): capturing it earlier lets
	// two concurrent Lookups move LastSeen backward and corrupt LRU order.
	r.mu.Lock()
	now := r.now()
	entry, ok := r.sessions[key]
	if !ok {
		r.mu.Unlock()
		return SessionState{}, false
	}
	if r.entryExpired(entry, now) {
		delete(r.sessions, key)
		r.mu.Unlock()
		r.record("expired")
		return SessionState{}, false
	}
	entry.LastSeen = now
	state := *entry
	r.mu.Unlock()
	return state, true
}

// Peek returns a copy of the pinned target for the given session key without
// refreshing LastSeen, deleting expired entries, or recording metrics. Used
// for read-only decisions (e.g. account preference during target build) that
// must not extend the pin TTL when the request later fails routing. Expired
// pins are reported as absent.
func (r *SessionRouter) Peek(key string) (SessionState, bool) {
	if r == nil {
		return SessionState{}, false
	}
	// Read-only: capture the timestamp inside the lock for consistency with
	// Lookup even though no state is mutated.
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	entry, ok := r.sessions[key]
	if !ok || r.entryExpired(entry, now) {
		return SessionState{}, false
	}
	return *entry, true
}

// PromoteIfChanged overwrites the pinned target for the given session key
// following a failover, but only when the new provider/model/account differs
// from the currently pinned one. Concurrent promotions of the same effective
// target therefore record a single failover metric. An unchanged pin still
// gets its per-agent idle TTL refreshed in place, so SIGHUP changes to
// sticky_session_ttl_seconds apply to live pins. A brand-new key respects
// the LRU cap like Pin. The ttl argument is the per-agent idle TTL
// (non-positive falls back to the router default). Returns true when the pin
// was changed.
func (r *SessionRouter) PromoteIfChanged(key, provider, model, account string, ttl time.Duration) bool {
	if r == nil {
		return false
	}
	if ttl <= 0 {
		ttl = r.ttl
	}
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = make(map[string]*SessionState)
	}
	// Lock region: the two explicit unlocks below are exhaustive — every
	// path between here and the unlocks falls through to exactly one of them.
	if entry, ok := r.sessions[key]; ok {
		if entry.Provider == provider && entry.Model == model && entry.Account == account {
			entry.ttl = ttl
			r.mu.Unlock()
			return false
		}
	} else if len(r.sessions) >= r.cap {
		r.evictLRU()
	}
	now := r.now()
	r.sessions[key] = &SessionState{
		Provider: provider,
		Model:    model,
		Account:  account,
		ttl:      ttl,
		Since:    now,
		LastSeen: now,
	}
	r.mu.Unlock()

	r.record("failover")
	if r.logger != nil {
		r.logger.Info("session pin promoted after failover", "key", key, "provider", provider, "model", model, "account", account, "ttl", ttl)
	}
	return true
}

// Evict removes the pin for the given session key.
func (r *SessionRouter) Evict(key string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, key)
}

// Len returns the number of currently tracked session pins.
func (r *SessionRouter) Len() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// Active returns the number of session pins that have not expired by their
// idle TTL. It drives the nenya_session_active gauge.
func (r *SessionRouter) Active() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	now := r.now()
	active := 0
	expired := 0
	for key, entry := range r.sessions {
		if r.entryExpired(entry, now) {
			delete(r.sessions, key)
			expired++
			continue
		}
		active++
	}
	r.mu.Unlock()

	for i := 0; i < expired; i++ {
		r.record("expired")
	}
	return active
}

// entryExpired reports whether the entry has exceeded its effective idle TTL
// (the per-entry TTL, falling back to the router default when unset) at the
// given instant. Callers must hold r.mu.
func (r *SessionRouter) entryExpired(entry *SessionState, now time.Time) bool {
	ttl := entry.ttl
	if ttl <= 0 {
		ttl = r.ttl
	}
	return now.Sub(entry.LastSeen) > ttl
}

// evictLRU removes the least-recently-seen entry when the store is at cap.
// Callers must hold r.mu. Metrics and logging happen under the caller's lock
// because eviction is always part of a larger mutation (Pin/PromoteIfChanged).
func (r *SessionRouter) evictLRU() {
	var oldestKey string
	var oldest time.Time
	for key, entry := range r.sessions {
		if oldestKey == "" || entry.LastSeen.Before(oldest) {
			oldestKey = key
			oldest = entry.LastSeen
		}
	}
	if oldestKey != "" {
		delete(r.sessions, oldestKey)
		if r.logger != nil {
			r.logger.Debug("session pin evicted (LRU cap reached)", "cap", r.cap)
		}
		// Distinct from TTL expiry: capacity evictions and idle expirations
		// are different operational signals sharing one counter's labels.
		r.record("evicted")
	}
}

func (r *SessionRouter) record(reason string) {
	if r.metrics != nil {
		r.metrics.RecordSessionPinChange(reason)
	}
}
