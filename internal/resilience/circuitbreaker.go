package resilience

import (
	"sync"
	"time"
)

type State int

const (
	StateClosed State = iota
	StateOpen
	StateHalfOpen
)

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

type Counts struct {
	Requests             uint32
	TotalSuccesses       uint32
	TotalFailures        uint32
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
}

type circuit struct {
	state            State
	generation       uint64
	counts           Counts
	expiry           time.Time
	halfOpenInflight uint32
	lastChange       time.Time
}

type CircuitBreaker struct {
	mu                  sync.Mutex
	circuits            map[string]*circuit
	failureThreshold    uint32
	successThreshold    uint32
	halfOpenMaxRequests uint32
	cooldown            time.Duration
	onStateChange       func(key string, from, to State)
}

func NewCircuitBreaker(failureThreshold, successThreshold int, halfOpenMaxRequests uint32, cooldown time.Duration, onStateChange func(string, State, State)) *CircuitBreaker {
	if failureThreshold <= 0 {
		failureThreshold = 5
	}
	if successThreshold <= 0 {
		successThreshold = 1
	}
	if halfOpenMaxRequests == 0 {
		halfOpenMaxRequests = 1
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
}

func (cb *CircuitBreaker) Allow(key string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		c.counts.Requests++
		c.lastChange = now
		return true

	case StateOpen:
		if now.After(c.expiry) {
			cb.setState(c, StateHalfOpen, key)
			c.counts.Requests++
			c.halfOpenInflight++
			c.lastChange = now
			return true
		}
		return false

	case StateHalfOpen:
		if c.halfOpenInflight >= cb.halfOpenMaxRequests {
			return false
		}
		c.counts.Requests++
		c.halfOpenInflight++
		c.lastChange = now
		return true
	}

	return false
}

func (cb *CircuitBreaker) RecordFailure(key string, cooldownOverride ...time.Duration) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		c.counts.Requests++
		c.counts.TotalFailures++
		c.counts.ConsecutiveFailures++
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

func (cb *CircuitBreaker) RecordSuccess(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		c.counts.Requests++
		c.counts.TotalSuccesses++
		c.counts.ConsecutiveSuccesses++
		c.counts.ConsecutiveFailures = 0
		c.lastChange = now

	case StateHalfOpen:
		c.counts.Requests++
		c.counts.TotalSuccesses++
		c.counts.ConsecutiveSuccesses++
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
