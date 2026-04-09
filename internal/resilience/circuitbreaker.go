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

type circuit struct {
	state          State
	failures       []time.Time
	successes      int
	lastChange     time.Time
	cooldownExpiry time.Time
}

func (c *circuit) pruneFailures(now time.Time, window time.Duration) {
	cutoff := now.Add(-window)
	i := 0
	for i < len(c.failures) && !c.failures[i].After(cutoff) {
		i++
	}
	if i > 0 {
		c.failures = c.failures[i:]
	}
}

type CircuitBreaker struct {
	mu               sync.Mutex
	circuits         map[string]*circuit
	failureThreshold int
	successThreshold int
	windowDuration   time.Duration
	defaultCooldown  time.Duration
}

func NewCircuitBreaker(failureThreshold, successThreshold int, windowDuration, defaultCooldown time.Duration) *CircuitBreaker {
	return &CircuitBreaker{
		circuits:         make(map[string]*circuit),
		failureThreshold: failureThreshold,
		successThreshold: successThreshold,
		windowDuration:   windowDuration,
		defaultCooldown:  defaultCooldown,
	}
}

func (cb *CircuitBreaker) getOrCreate(key string) *circuit {
	c, ok := cb.circuits[key]
	if !ok {
		c = &circuit{state: StateClosed, lastChange: time.Now()}
		cb.circuits[key] = c
	}
	return c
}

func (cb *CircuitBreaker) Allow(key string) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		return true
	case StateOpen:
		if now.After(c.cooldownExpiry) {
			c.state = StateHalfOpen
			c.successes = 0
			c.lastChange = now
			return true
		}
		return false
	case StateHalfOpen:
		return true
	}
	return true
}

func (cb *CircuitBreaker) RecordFailure(key string, cooldownOverride ...time.Duration) bool {
	cd := cb.defaultCooldown
	if len(cooldownOverride) > 0 && cooldownOverride[0] > 0 {
		cd = cooldownOverride[0]
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateClosed:
		c.pruneFailures(now, cb.windowDuration)
		c.failures = append(c.failures, now)
		if len(c.failures) >= cb.failureThreshold {
			c.state = StateOpen
			c.cooldownExpiry = now.Add(cd)
			c.lastChange = now
			return true
		}
		return false

	case StateHalfOpen:
		c.state = StateOpen
		c.cooldownExpiry = now.Add(cd)
		c.lastChange = now
		c.successes = 0
		return true

	case StateOpen:
		newExpiry := now.Add(cd)
		if newExpiry.After(c.cooldownExpiry) {
			c.cooldownExpiry = newExpiry
		}
		return false
	}
	return false
}

func (cb *CircuitBreaker) ForceOpen(key string, cooldown time.Duration) {
	if key == "" || cooldown <= 0 {
		return
	}

	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()
	c.state = StateOpen
	c.failures = nil
	c.successes = 0
	c.cooldownExpiry = now.Add(cooldown)
	c.lastChange = now
}

func (cb *CircuitBreaker) RecordSuccess(key string) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c := cb.getOrCreate(key)
	now := time.Now()

	switch c.state {
	case StateHalfOpen:
		c.successes++
		if c.successes >= cb.successThreshold {
			c.state = StateClosed
			c.failures = nil
			c.successes = 0
			c.lastChange = now
		}

	case StateClosed:
		c.pruneFailures(now, cb.windowDuration)
	}
}

func (cb *CircuitBreaker) State(key string) State {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	c, ok := cb.circuits[key]
	if !ok {
		return StateClosed
	}
	if c.state == StateOpen && time.Now().After(c.cooldownExpiry) {
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
		if c.state == StateOpen && now.Before(c.cooldownExpiry) {
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
		if c.state == StateOpen && time.Now().After(c.cooldownExpiry) {
			state = StateHalfOpen
		}
		snap[key] = state.String()
	}
	return snap
}
