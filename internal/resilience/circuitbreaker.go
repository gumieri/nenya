package resilience

import (
	"math"
	"sync"
	"time"
	"unicode/utf8"
)

// incUint32 safely increments a uint32 counter, capping at math.MaxUint32
// to prevent integer overflow (CWE-190).
func incUint32(v *uint32) uint32 {
	if *v == math.MaxUint32 {
		return *v
	}
	*v++
	return *v
}

// State represents the current state of a circuit breaker.
type State int

const (
	// StateClosed is the normal operating state where requests pass through.
	StateClosed State = iota
	// StateOpen is the failure state where requests are blocked until cooldown expires.
	StateOpen
	// StateHalfOpen is the probe state where limited requests are allowed to test recovery.
	StateHalfOpen
)

// String returns a human-readable representation of the state.
func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Counts holds the request statistics for a single circuit.
type Counts struct {
	Requests             uint32 // Total requests attempted
	TotalSuccesses       uint32 // Total successful requests
	TotalFailures        uint32 // Total failed requests
	ConsecutiveSuccesses uint32 // Consecutive successful requests
	ConsecutiveFailures  uint32 // Consecutive failed requests
}

type circuit struct {
	state            State
	generation       uint64
	counts           Counts
	expiry           time.Time
	halfOpenInflight uint32
	lastChange       time.Time
	// lastUsed tracks the most recent getOrCreate touch for the registry's
	// idle-TTL eviction (NENYA-31).
	lastUsed time.Time

	// New fields for error-semantic tracking
	LastErrorClass  ErrorClass
	LastErrorBody   string
	LastErrorStatus int
}

// CircuitBreaker manages per-key circuit breakers with configurable
// failure/success thresholds and automatic recovery.
//
// Thread-safety: All methods are safe to call concurrently. Internal state
// is protected by a sync.Mutex. Each key has an independent circuit.
//
// Bounding (NENYA-31): the registry is bounded — keys are agent+provider+
// model strings where the model name is caller-supplied, so the map would
// otherwise grow without limit under adversarial or churn-heavy names. When
// the cap is reached, a lazy sweep evicts circuits that are Closed and
// idle beyond the TTL (never Open/HalfOpen ones — failing open state would
// lose an active investigation), then the least-recently-used Closed
// circuits. modelLocks entries past their expiry are pruned in the same
// sweep. Memory is therefore bounded by cap + the number of simultaneously
// non-Closed circuits.
type CircuitBreaker struct {
	mu                  sync.Mutex
	circuits            map[string]*circuit
	failureThreshold    uint32
	successThreshold    uint32
	halfOpenMaxRequests uint32
	cooldown            time.Duration
	onStateChange       func(key string, from, to State)
	onStateChangeMetric func(key, from, to string)

	modelLocks map[string]time.Time
	backoff    *BackoffTracker
	classifier func(status int, body string, level int) CooldownDecision

	// minQuotaCooldown is a floor applied to quota-class cooldowns
	// (NENYA-41): upstreams sometimes send sub-second Retry-After values on
	// quota errors, and honoring them literally produces zero-wait retry
	// storms against the same exhausted account. 0 disables the floor.
	minQuotaCooldown time.Duration

	// Registry bounding (NENYA-31). maxCircuits caps the circuit map
	// (default 1024); idleTTL is how long a Closed circuit must be
	// untouched before it becomes eviction-eligible (default 10 min).
	// evictions counts evicted circuits for observability.
	maxCircuits int
	idleTTL     time.Duration
	evictions   uint64
}

// NewCircuitBreaker creates a CircuitBreaker with the given thresholds.
// Zero or negative values are replaced with sensible defaults.
func NewCircuitBreaker(failureThreshold, successThreshold int, halfOpenMaxRequests uint32, cooldown time.Duration, onStateChange func(string, State, State)) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if successThreshold <= 0 {
		successThreshold = 1
	}
	if halfOpenMaxRequests == 0 {
		halfOpenMaxRequests = 3
	}
	if cooldown <= 0 {
		cooldown = 60 * time.Second
	}

	return &CircuitBreaker{
		circuits:            make(map[string]*circuit),
		failureThreshold:    uint32(failureThreshold),
		successThreshold:    uint32(successThreshold),
		halfOpenMaxRequests: halfOpenMaxRequests,
		cooldown:            cooldown,
		onStateChange:       onStateChange,
		modelLocks:          make(map[string]time.Time),
		backoff:             NewBackoffTracker(),
		classifier:          classifyHTTPError,
		maxCircuits:         DefaultMaxCircuits,
		idleTTL:             DefaultCircuitIdleTTL,
	}
}

// Registry bounding defaults (NENYA-31), mirroring the evidence design:
// 1024 entries, 10-minute idle TTL for eviction-eligible Closed circuits.
const (
	DefaultMaxCircuits    = 1024
	DefaultCircuitIdleTTL = 10 * time.Minute
)

// SetRegistryLimits overrides the circuit-registry bounding. Non-positive
// values keep the current setting.
func (cb *CircuitBreaker) SetRegistryLimits(maxCircuits int, idleTTL time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if maxCircuits > 0 {
		cb.maxCircuits = maxCircuits
	}
	if idleTTL > 0 {
		cb.idleTTL = idleTTL
	}
}

// RegistryStats exposes the registry size and cumulative evictions for
// /statsz and tests.
func (cb *CircuitBreaker) RegistryStats() (size int, evictions uint64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	return len(cb.circuits), cb.evictions
}

// evictForCapacity makes room for one more circuit when the registry is at
// cap. Eviction order: Closed circuits idle beyond the TTL first (oldest
// use first), then the least-recently-used Closed circuits. Open/HalfOpen
// circuits are never evicted — losing an active breaker would hide an
// in-progress investigation. Expired modelLocks are pruned in the same
// pass. Must be called with cb.mu held.
func (cb *CircuitBreaker) evictForCapacity(now time.Time) {
	if len(cb.circuits) < cb.maxCircuits {
		return
	}
	cb.pruneExpiredLocksLocked(now)

	// Pass 1: idle Closed circuits (unused past the TTL).
	var oldestClosedKey string
	var oldestUse time.Time
	for key, c := range cb.circuits {
		if c.state != StateClosed || c.halfOpenInflight != 0 {
			continue
		}
		if now.Sub(c.lastUsed) > cb.idleTTL {
			delete(cb.circuits, key)
			cb.evictions++
			cb.backoff.Reset(key)
			delete(cb.modelLocks, key)
			continue
		}
		if oldestClosedKey == "" || c.lastUsed.Before(oldestUse) {
			oldestClosedKey, oldestUse = key, c.lastUsed
		}
	}
	if len(cb.circuits) < cb.maxCircuits {
		return
	}
	// Pass 2: still at cap — shed the least-recently-used Closed circuit.
	// If every circuit is Open/HalfOpen (pathological), creating anyway is
	// safer than failing requests; non-Closed circuits cool down and become
	// evictable, so growth is self-limiting.
	if oldestClosedKey != "" {
		delete(cb.circuits, oldestClosedKey)
		cb.evictions++
		cb.backoff.Reset(oldestClosedKey)
	}
}

// pruneExpiredLocksLocked drops modelLocks entries whose cooldown has
// passed. Must be called with cb.mu held.
func (cb *CircuitBreaker) pruneExpiredLocksLocked(now time.Time) {
	for key, until := range cb.modelLocks {
		if now.After(until) {
			delete(cb.modelLocks, key)
		}
	}
}

func (cb *CircuitBreaker) getOrCreate(key string) *circuit {
	now := time.Now()
	cb.evictForCapacity(now)
	c, ok := cb.circuits[key]
	if !ok {
		c = &circuit{
			state:      StateClosed,
			generation: 1,
			expiry:     time.Time{},
			lastUsed:   now,
		}
		cb.circuits[key] = c
	} else {
		c.lastUsed = now
	}
	return c
}

// SetMinQuotaCooldown sets the floor applied to quota-class cooldowns
// (NENYA-41). Zero or negative values disable the floor.
func (cb *CircuitBreaker) SetMinQuotaCooldown(d time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if d < 0 {
		d = 0
	}
	cb.minQuotaCooldown = d
}

// SetBackoffIncrementCallback sets a callback that is invoked when the backoff level increments.
// The callback receives the circuit key and the new backoff level.
//
// WARNING: The callback is invoked synchronously while holding the circuit breaker's mutex.
// To avoid deadlocks, the callback MUST be fast and non-blocking. It MUST NOT call back into
// any CircuitBreaker methods that acquire the mutex (e.g., Allow, RecordFailureWithStatus, etc.).
// The callback is safe for metrics emission, logging, or simple state updates only.
func (cb *CircuitBreaker) SetBackoffIncrementCallback(fn func(key string, level int)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.backoff = NewBackoffTrackerWithCallback(fn)
}

// SetStateChangeMetricCallback sets a callback that is invoked when the circuit state changes.
// The callback receives the circuit key and the old/new state strings.
// The callback must be fast and non-blocking (no I/O, no locks) as it is invoked
// while holding the circuit mutex.
func (cb *CircuitBreaker) SetStateChangeMetricCallback(fn func(key, from, to string)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.onStateChangeMetric = fn
}

func (cb *CircuitBreaker) setState(c *circuit, newState State, key string) {
	if c.state == newState {
		return
	}

	from := c.state
	c.state = newState
	c.generation++

	c.counts.Requests = 0
	c.counts.TotalSuccesses = 0
	c.counts.TotalFailures = 0
	c.counts.ConsecutiveSuccesses = 0
	c.counts.ConsecutiveFailures = 0
	c.halfOpenInflight = 0

	now := time.Now()
	c.lastChange = now

	switch newState {
	case StateClosed:
		c.expiry = time.Time{}
	case StateOpen:
		c.expiry = now.Add(cb.cooldown)
	case StateHalfOpen:
		c.expiry = time.Time{}
	}

	if cb.onStateChange != nil {
		cb.onStateChange(key, from, newState)
	}
	if cb.onStateChangeMetric != nil {
		cb.onStateChangeMetric(key, from.String(), newState.String())
	}
}

// Allow checks whether a request should be permitted for the given key.
// Returns true if the request is allowed, advancing the request count.
// For Open state, transitions to HalfOpen after cooldown expires.
// For HalfOpen, respects the halfOpenMaxRequests limit.
func (cb *CircuitBreaker) Allow(key string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		incUint32(&c.counts.Requests)
		c.lastChange = now
		return true

	case StateOpen:
		if now.After(c.expiry) {
			cb.setState(c, StateHalfOpen, key)
			incUint32(&c.counts.Requests)
			incUint32(&c.halfOpenInflight)
			c.lastChange = now
			return true
		}
		return false

	case StateHalfOpen:
		if c.halfOpenInflight >= cb.halfOpenMaxRequests {
			return false
		}
		incUint32(&c.counts.Requests)
		incUint32(&c.halfOpenInflight)
		c.lastChange = now
		return true
	}

	return false
}

// RecordFailure records a failed request for the given key.
// May trigger a state transition to Open based on consecutive failures.
// For Open state, optionally extends the cooldown with cooldownOverride.
func (cb *CircuitBreaker) RecordFailure(key string, cooldownOverride ...time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	cd := cb.cooldown
	if len(cooldownOverride) > 0 && cooldownOverride[0] > 0 {
		cd = cooldownOverride[0]
	}

	switch c.state {
	case StateClosed:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalFailures)
		incUint32(&c.counts.ConsecutiveFailures)
		c.counts.ConsecutiveSuccesses = 0
		c.lastChange = now

		if c.counts.ConsecutiveFailures >= cb.failureThreshold {
			cb.setState(c, StateOpen, key)
			c.expiry = now.Add(cd)
		}

	case StateHalfOpen:
		cb.setState(c, StateOpen, key)
		c.expiry = now.Add(cd)
		c.lastChange = now

	case StateOpen:
		newExpiry := now.Add(cd)
		if newExpiry.After(c.expiry) {
			c.expiry = newExpiry
		}
	}
}

// RecordOpaqueFailure marks a request failure for the circuit breaker's
// request counters without counting it toward the failure threshold. It is
// used by probabilistic/heuristic retry signals (e.g. an opaque 4xx body) that
// must not bench an otherwise healthy provider: a client repeatedly provoking
// an ambiguous body would otherwise trip the circuit for all users.
func (cb *CircuitBreaker) RecordOpaqueFailure(key string) {
	if key == "" {
		return
	}
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	if c.state == StateHalfOpen {
		// A probe slot was consumed by Allow before the dispatch. An opaque
		// failure is neither success nor failure, so release the slot without
		// changing the circuit's state (otherwise repeated opaque failures
		// permanently wedge the half-open probe budget).
		if c.halfOpenInflight > 0 {
			c.halfOpenInflight--
		}
		return
	}
	if c.state != StateClosed {
		return
	}
	incUint32(&c.counts.Requests)
	c.lastChange = time.Now()
}

// RecordFailureWithStatus records a failed request with HTTP status and body,
// applying error-semantic classification and model locks.
func (cb *CircuitBreaker) RecordFailureWithStatus(key string, status int, body string) CooldownDecision {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	decision := cb.classifier(status, body, cb.backoff.GetLevel(key))

	// Quota floor (NENYA-41): upstreams sometimes send sub-second Retry-After
	// values on quota errors; honoring them literally produces zero-wait
	// retry storms against the same exhausted account.
	decision.Cooldown = cb.applyQuotaFloor(decision)

	// Note: GetLevel and Increment acquire backoff.mu separately, so another
	// goroutine could modify the level between classification and increment.
	// This benign race is acceptable: at worst, a request uses a slightly stale
	// backoff level for its cooldown calculation. The final level after Increment
	// is always correct.

	cb.applyModelLock(key, decision, now)

	c.LastErrorClass = decision.Class
	c.LastErrorBody = truncate(body, 200)
	c.LastErrorStatus = status

	switch c.state {
	case StateClosed:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalFailures)
		incUint32(&c.counts.ConsecutiveFailures)
		c.counts.ConsecutiveSuccesses = 0
		c.lastChange = now

		if c.counts.ConsecutiveFailures >= cb.failureThreshold {
			cb.setState(c, StateOpen, key)
		}

	case StateHalfOpen:
		cb.setState(c, StateOpen, key)

	case StateOpen:
		// Reset expiry from now (not previous expiry) to apply new backoff level.
		// The expiry is only extended, never shortened.
		newExpiry := now.Add(decision.Cooldown)
		if newExpiry.After(c.expiry) {
			c.expiry = newExpiry
		}
	}

	return decision
}

// applyQuotaFloor returns the decision cooldown raised to the configured
// quota-class floor when applicable (NENYA-41). Caller holds cb.mu.
func (cb *CircuitBreaker) applyQuotaFloor(decision CooldownDecision) time.Duration {
	if decision.Class != ErrorClassQuota || cb.minQuotaCooldown <= 0 || decision.Cooldown >= cb.minQuotaCooldown {
		return decision.Cooldown
	}
	return cb.minQuotaCooldown
}

// applyModelLock extends the key's model lock to cover the decision cooldown
// and advances the backoff tracker when the decision says so. Monotonic
// (NENYA-41): a later, softer failure must never shorten an active lock —
// deadlines only extend. Caller holds cb.mu.
func (cb *CircuitBreaker) applyModelLock(key string, decision CooldownDecision, now time.Time) {
	if !decision.ShouldLock {
		return
	}
	if until := now.Add(decision.Cooldown); until.After(cb.modelLocks[key]) {
		cb.modelLocks[key] = until
	}
	if decision.IncrementBackoff {
		if _, callback := cb.backoff.Increment(key); callback != nil {
			callback()
		}
	}
}

// truncate shortens s to at most maxLen runes, appending "..." if truncated.
// It is rune-safe and will not split multi-byte UTF-8 sequences.
func truncate(s string, maxLen int) string {
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	runes := 0
	for i := range s {
		if runes >= maxLen {
			return s[:i] + "..."
		}
		runes++
	}
	return s
}

// RecordSuccess records a successful request for the given key.
// May trigger a state transition from HalfOpen to Closed based on success threshold.
func (cb *CircuitBreaker) RecordSuccess(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalSuccesses)
		incUint32(&c.counts.ConsecutiveSuccesses)
		c.counts.ConsecutiveFailures = 0
		c.lastChange = now

	case StateHalfOpen:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalSuccesses)
		incUint32(&c.counts.ConsecutiveSuccesses)
		c.counts.ConsecutiveFailures = 0
		if c.halfOpenInflight > 0 {
			c.halfOpenInflight--
		}

		if c.counts.ConsecutiveSuccesses >= cb.successThreshold {
			cb.setState(c, StateClosed, key)
			cb.backoff.Reset(key)
			delete(cb.modelLocks, key)
		} else {
			c.lastChange = now
		}
	}
}

// RecordSuccessWithModel records a successful request and clears the model
// lock and backoff. The key parameter is the circuit breaker key
// (agent:provider:model) and model is used for lock/backoff tracking.
func (cb *CircuitBreaker) RecordSuccessWithModel(key, model string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalSuccesses)
		incUint32(&c.counts.ConsecutiveSuccesses)
		c.counts.ConsecutiveFailures = 0
		c.lastChange = now

	case StateHalfOpen:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalSuccesses)
		incUint32(&c.counts.ConsecutiveSuccesses)
		c.counts.ConsecutiveFailures = 0
		if c.halfOpenInflight > 0 {
			c.halfOpenInflight--
		}

		if c.counts.ConsecutiveSuccesses >= cb.successThreshold {
			cb.setState(c, StateClosed, key)
			cb.backoff.Reset(model)
			delete(cb.modelLocks, model)
		} else {
			c.lastChange = now
		}
	}
}

// ReleaseHalfOpen releases a half-open inflight slot for the given key without
// recording success or failure. Use when a request was allowed in HalfOpen state
// but was canceled before dispatch (e.g., client disconnect). Safe to call when
// the circuit doesn't exist, is not HalfOpen, or has no inflight slots — all are
// no-ops.
func (cb *CircuitBreaker) ReleaseHalfOpen(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c, ok := cb.circuits[key]
	if !ok || c.state != StateHalfOpen || c.halfOpenInflight == 0 {
		return
	}
	c.halfOpenInflight--
}

// ForceOpen forces the circuit breaker for the given key into the Open state
// for the specified cooldown duration. Used for manual circuit breaking
// (e.g., on HTTP 429 rate limit errors). Monotonic (NENYA-41): a later call
// with a shorter cooldown never shortens an active one.
func (cb *CircuitBreaker) ForceOpen(key string, cooldown time.Duration) {
	if key == "" || cooldown <= 0 {
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()
	// Capture the deadline BEFORE setState: setState(Open) stamps a fresh
	// default-cooldown expiry as a side effect, which must not participate in
	// the monotonic comparison — only a genuinely ACTIVE deadline does.
	prevExpiry := c.expiry
	cb.setState(c, StateOpen, key)
	newExpiry := now.Add(cooldown)
	if now.Before(prevExpiry) && prevExpiry.After(newExpiry) {
		// Active longer deadline wins (NENYA-41: deadlines only extend).
		return
	}
	c.expiry = newExpiry
}

// Peek reports whether a request would be allowed for key without producing any
// side effects (no inflight counter increment, no state transition). Use this
// for read-only partitioning (active vs. cooling lists). Call Allow when an
// actual request is about to be dispatched.
func (cb *CircuitBreaker) Peek(key string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		return true
	case StateOpen:
		return now.After(c.expiry)
	case StateHalfOpen:
		return c.halfOpenInflight < cb.halfOpenMaxRequests
	}
	return false
}

// State returns the current state of the circuit breaker for the given key.
// For Open circuits past their cooldown, reports HalfOpen without side effects.
func (cb *CircuitBreaker) State(key string) State {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c, ok := cb.circuits[key]
	if !ok {
		return StateClosed
	}

	if c.state == StateOpen && time.Now().After(c.expiry) {
		return StateHalfOpen
	}

	return c.state
}

// ActiveCount returns the number of circuits currently in the Open state
// (i.e., actively blocking requests). Circuits past cooldown are excluded.
func (cb *CircuitBreaker) ActiveCount() int {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	count := 0
	for _, c := range cb.circuits {
		if c.state == StateOpen && now.Before(c.expiry) {
			count++
		}
	}
	return count
}

// Snapshot returns a map of all circuit keys to their current state names.
// For Open circuits past cooldown, reports "half_open" without side effects.
func (cb *CircuitBreaker) Snapshot() map[string]string {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	snap := make(map[string]string, len(cb.circuits))
	for key, c := range cb.circuits {
		state := c.state
		if c.state == StateOpen && time.Now().After(c.expiry) {
			state = StateHalfOpen
		}
		snap[key] = state.String()
	}
	return snap
}

// IsModelLocked returns true if the model is currently in cooldown.
func (cb *CircuitBreaker) IsModelLocked(model string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	until, locked := cb.modelLocks[model]
	if !locked {
		return false
	}
	return time.Now().Before(until)
}

// GetModelLockUntil returns the cooldown expiry time for a model, or zero if not locked.
func (cb *CircuitBreaker) GetModelLockUntil(model string) time.Time {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	until, locked := cb.modelLocks[model]
	if !locked {
		return time.Time{}
	}
	return until
}

// UnlockModel immediately unlocks a model, clearing its cooldown and backoff.
// Used for manual intervention when a model has recovered before its scheduled expiry.
func (cb *CircuitBreaker) UnlockModel(model string) {
	if model == "" {
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	delete(cb.modelLocks, model)
	cb.backoff.Reset(model)
}

// GetBackoffLevel returns the current backoff level for a model.
// Returns 0 if the backoff tracker is nil or the model has no backoff level.
func (cb *CircuitBreaker) GetBackoffLevel(model string) int {
	if cb.backoff == nil {
		return 0
	}
	return cb.backoff.GetLevel(model)
}

// SnapshotDetailed returns a detailed snapshot including circuit states,
// model locks, and backoff levels.
func (cb *CircuitBreaker) SnapshotDetailed() map[string]interface{} {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	now := time.Now()
	snap := make(map[string]interface{})

	circuits := make(map[string]interface{})
	for key, c := range cb.circuits {
		state := c.state
		if c.state == StateOpen && now.After(c.expiry) {
			state = StateHalfOpen
		}

		var expiryStr string
		if !c.expiry.IsZero() {
			expiryStr = c.expiry.Format(time.RFC3339)
		}

		circuits[key] = map[string]interface{}{
			"state":             state.String(),
			"failure_count":     c.counts.ConsecutiveFailures,
			"success_count":     c.counts.ConsecutiveSuccesses,
			"expiry":            expiryStr,
			"last_error":        c.LastErrorBody,
			"last_error_status": c.LastErrorStatus,
		}
	}
	snap["circuits"] = circuits

	locks := make(map[string]string)
	for model, until := range cb.modelLocks {
		if now.Before(until) {
			locks[model] = until.Format(time.RFC3339)
		}
	}
	snap["model_locks"] = locks

	backoffLevels := make(map[string]int)
	cb.backoff.mu.Lock()
	for model, level := range cb.backoff.levels {
		backoffLevels[model] = level
	}
	cb.backoff.mu.Unlock()
	snap["backoff_levels"] = backoffLevels
	snap["registry_size"] = len(cb.circuits)
	snap["registry_evictions"] = cb.evictions

	return snap
}
