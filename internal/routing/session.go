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
// existing sessions and Promote after a failover.
func (r *SessionRouter) Pin(key, provider, model, account string, ttl time.Duration) {
	if r == nil {
		return
	}
	if ttl <= 0 {
		ttl = r.ttl
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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
	if _, exists := r.sessions[key]; !exists && len(r.sessions) >= r.cap {
		r.evictLRU()
	}
	r.sessions[key] = entry
	r.record("new")
	if r.logger != nil {
		r.logger.Debug("session pin created", "key", key, "provider", provider, "model", model, "account", account, "ttl", ttl)
	}
}

// Promote unconditionally overwrites the pinned target for the given session
// key following a failover: the previous pin was invalidated (cooldown,
// exhaustion, or context growth) and a different target is now sticky. It
// records a failover metric even when the new target equals the current pin —
// prefer PromoteIfChanged, which dedupes no-change promotions; Promote is
// retained as the unconditional variant for tests and external callers. The
// ttl argument is the per-agent idle TTL (non-positive falls back to the
// router default).
func (r *SessionRouter) Promote(key, provider, model, account string, ttl time.Duration) {
	if r == nil {
		return
	}
	if ttl <= 0 {
		ttl = r.ttl
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = make(map[string]*SessionState)
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
	r.record("failover")
	if r.logger != nil {
		r.logger.Info("session pin promoted after failover", "key", key, "provider", provider, "model", model, "account", account, "ttl", ttl)
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
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.sessions[key]
	if !ok {
		return SessionState{}, false
	}
	ttl := entry.ttl
	if ttl <= 0 {
		ttl = r.ttl
	}
	if now.Sub(entry.LastSeen) > ttl {
		delete(r.sessions, key)
		r.record("expired")
		return SessionState{}, false
	}
	entry.LastSeen = now
	return *entry, true
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
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.sessions[key]
	if !ok {
		return SessionState{}, false
	}
	ttl := entry.ttl
	if ttl <= 0 {
		ttl = r.ttl
	}
	if r.now().Sub(entry.LastSeen) > ttl {
		return SessionState{}, false
	}
	return *entry, true
}

// PromoteIfChanged overwrites the pinned target for the given session key
// following a failover, but only when the new provider/model/account differs
// from the currently pinned one. Concurrent promotions of the same effective
// target therefore record a single failover metric. The ttl argument is the
// per-agent idle TTL (non-positive falls back to the router default).
// Returns true when the pin was changed.
func (r *SessionRouter) PromoteIfChanged(key, provider, model, account string, ttl time.Duration) bool {
	if r == nil {
		return false
	}
	if ttl <= 0 {
		ttl = r.ttl
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.sessions[key]; ok &&
		entry.Provider == provider && entry.Model == model && entry.Account == account {
		return false
	}
	if r.sessions == nil {
		r.sessions = make(map[string]*SessionState)
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
	defer r.mu.Unlock()
	now := r.now()
	active := 0
	for key, entry := range r.sessions {
		ttl := entry.ttl
		if ttl <= 0 {
			ttl = r.ttl
		}
		if now.Sub(entry.LastSeen) > ttl {
			delete(r.sessions, key)
			r.record("expired")
			continue
		}
		active++
	}
	return active
}

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
		r.record("expired")
	}
}

func (r *SessionRouter) record(reason string) {
	if r.metrics != nil {
		r.metrics.RecordSessionPinChange(reason)
	}
}
