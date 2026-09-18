package auth

import (
	"sync"
	"time"
)

// KeyUsageTracker enforces per-API-key request rate limits and daily token
// budgets, plus per-provider daily token budgets with per-key priority
// tiers ("always" served regardless of remaining budget, "fill" served only
// while headroom exists).
//
// All state is in-memory and mutex-guarded: single-process by design. A
// multi-process deployment would externalize this state (e.g. Redis INCRBY
// with TTL windows) behind the same interface.
//
// Token accounting uses the request's pre-dispatch estimate as a
// non-refundable reservation: failed dispatches consume the reserved
// budget. This is deliberately conservative — it under-counts nothing and
// bounds worst-case spend by the configured limit itself.
type KeyUsageTracker struct {
	mu   sync.Mutex
	keys map[string]*keyUsage
	prov map[string]*providerUsage
	now  func() time.Time // injectable for tests
}

type keyUsage struct {
	rpmWindowStart time.Time
	rpmCount       int
	dayStamp       string
	dayTokens      int
}

type providerUsage struct {
	dayStamp  string
	dayTokens int
}

// NewKeyUsageTracker creates a tracker. now is injectable for tests; nil
// uses time.Now.
func NewKeyUsageTracker(now func() time.Time) *KeyUsageTracker {
	if now == nil {
		now = time.Now
	}
	return &KeyUsageTracker{
		keys: make(map[string]*keyUsage),
		prov: make(map[string]*providerUsage),
		now:  now,
	}
}

// AllowRequest enforces the per-key requests-per-minute limit (fixed
// one-minute window). maxRPM <= 0 means unlimited. Returns false when the
// key exceeded its limit for the current window.
func (t *KeyUsageTracker) AllowRequest(keyName string, maxRPM int) bool {
	if maxRPM <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	u := t.keys[keyName]
	if u == nil {
		u = &keyUsage{}
		t.keys[keyName] = u
	}
	u.rollDay(now)

	if now.Sub(u.rpmWindowStart) >= time.Minute {
		u.rpmWindowStart = now
		u.rpmCount = 0
	}
	if u.rpmCount >= maxRPM {
		return false
	}
	u.rpmCount++
	return true
}

// ChargeKeyTokens charges tokens against the key's daily budget (UTC day).
// dailyBudget <= 0 means unlimited. Returns false — without charging — when
// the charge would exceed the remaining budget.
func (t *KeyUsageTracker) ChargeKeyTokens(keyName string, dailyBudget, tokens int) bool {
	if dailyBudget <= 0 || tokens <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	u := t.keys[keyName]
	if u == nil {
		u = &keyUsage{}
		t.keys[keyName] = u
	}
	u.rollDay(now)

	if u.dayTokens+tokens > dailyBudget {
		return false
	}
	u.dayTokens += tokens
	return true
}

// AllowProviderTokens enforces a provider's daily token budget for a
// request of the given priority tier. tier "fill" is rejected when the
// charge would exceed the remaining budget; tier "always" (the zero value)
// is always served and charges even into exhaustion. providerBudget <= 0
// means unlimited.
func (t *KeyUsageTracker) AllowProviderTokens(providerName, tier string, providerBudget, tokens int) bool {
	if providerBudget <= 0 || tokens <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	u := t.prov[providerName]
	if u == nil {
		u = &providerUsage{}
		t.prov[providerName] = u
	}
	stamp := now.UTC().Format("2006-01-02")
	if u.dayStamp != stamp {
		u.dayStamp = stamp
		u.dayTokens = 0
	}

	if u.dayTokens+tokens > providerBudget && tier != "always" {
		return false
	}
	u.dayTokens += tokens
	return true
}

// Snapshot returns per-key and per-provider usage for /statsz. Values are
// snapshots under the tracker lock; treat them as advisory.
func (t *KeyUsageTracker) Snapshot() map[string]interface{} {
	t.mu.Lock()
	defer t.mu.Unlock()

	stamp := t.now().UTC().Format("2006-01-02")
	keys := make(map[string]interface{}, len(t.keys))
	for name, u := range t.keys {
		dayTokens := u.dayTokens
		if u.dayStamp != stamp {
			dayTokens = 0
		}
		keys[name] = map[string]int{
			"rpm_count":  u.rpmCount,
			"day_tokens": dayTokens,
		}
	}
	providers := make(map[string]interface{}, len(t.prov))
	for name, u := range t.prov {
		dayTokens := u.dayTokens
		if u.dayStamp != stamp {
			dayTokens = 0
		}
		providers[name] = map[string]int{"day_tokens": dayTokens}
	}
	return map[string]interface{}{"keys": keys, "providers": providers}
}

// rollDay resets the key's daily counter when the UTC day changed. Caller
// holds t.mu.
func (u *keyUsage) rollDay(now time.Time) {
	stamp := now.UTC().Format("2006-01-02")
	if u.dayStamp != stamp {
		u.dayStamp = stamp
		u.dayTokens = 0
	}
}
