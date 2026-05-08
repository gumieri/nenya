package resilience

import (
	"math"
	"sync"
	"time"
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
}

// CircuitBreaker manages per-key circuit breakers with configurable
// failure/success thresholds and automatic recovery.
//
// Thread-safety: All methods are safe to call concurrently. Internal state
// is protected by a sync.Mutex. Each key has an independent circuit.
type CircuitBreaker struct {
	mu                  sync.Mutex
	circuits            map[string]*circuit
	failureThreshold    uint32
	successThreshold    uint32
	halfOpenMaxRequests uint32
	cooldown            time.Duration
	onStateChange       func(key string, from, to State)
	onStateChangeMetric func(key, from, to string)
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
	}
}

// onStateChangeMetric is called while holding the circuit mutex.
// The callback must be fast and non-blocking (no I/O, no locks).
func (cb *CircuitBreaker) SetStateChangeMetricCallback(fn func(key, from, to string)) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.onStateChangeMetric = fn
}

func (cb *CircuitBreaker) getOrCreate(key string) *circuit {
	c, ok := cb.circuits[key]
	if !ok {
		c = &circuit{
			state:      StateClosed,
			generation: 1,
			expiry:     time.Time{},
		}
		cb.circuits[key] = c
	}
	return c
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

	switch c.state {
	case StateClosed:
		incUint32(&c.counts.Requests)
		incUint32(&c.counts.TotalFailures)
		incUint32(&c.counts.ConsecutiveFailures)
		c.counts.ConsecutiveSuccesses = 0
		c.lastChange = now

		if c.counts.ConsecutiveFailures >= cb.failureThreshold {
			cd := cb.cooldown
			if len(cooldownOverride) > 0 && cooldownOverride[0] > 0 {
				cd = cooldownOverride[0]
			}
			cb.setState(c, StateOpen, key)
			c.expiry = now.Add(cd)
		}

	case StateHalfOpen:
		cd := cb.cooldown
		if len(cooldownOverride) > 0 && cooldownOverride[0] > 0 {
			cd = cooldownOverride[0]
		}
		cb.setState(c, StateOpen, key)
		c.expiry = now.Add(cd)
		c.lastChange = now

	case StateOpen:
		if len(cooldownOverride) > 0 && cooldownOverride[0] > 0 {
			newExpiry := now.Add(cooldownOverride[0])
			if newExpiry.After(c.expiry) {
				c.expiry = newExpiry
			}
		}
	}
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
		} else {
			c.lastChange = now
		}
	}
}

// ForceOpen forces the circuit breaker for the given key into the Open state
// for the specified cooldown duration. Used for manual circuit breaking
// (e.g., on HTTP 429 rate limit errors).
func (cb *CircuitBreaker) ForceOpen(key string, cooldown time.Duration) {
	if key == "" || cooldown <= 0 {
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()
	cb.setState(c, StateOpen, key)
	c.expiry = now.Add(cooldown)
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
