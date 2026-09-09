package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nenya/config"
)

// SelectedAccount is the immutable snapshot returned by account selection:
// the account ID and its credential copy. Callers must not mutate pool state
// directly — use ReportError/ReportSuccess instead.
type SelectedAccount struct {
	ID         string
	Credential string
}

// AccountPool manages multiple provider accounts with mutex-guarded selection.
// Implements weighted least-recently-used selection (WFQ-style, NENYA-40):
// each pick advances the chosen account's LastUsed by weightTick/weight, so
// min-LastUsed selection distributes traffic proportionally to configured
// weights while remaining fair-by-construction under candidate filtering.
type AccountPool struct {
	mu       sync.RWMutex
	provider string
	accounts []*config.ProviderAccount
	// lastPicked is the accounts index of the previous pick (-1 when unset).
	// It rotates the min-scan start so equal virtual clocks are served in
	// round-robin succession rather than always favoring the earliest
	// configured account — re-entering accounts often tie exactly with the
	// pack at the recency floor, and a fixed tie-break would systematically
	// bias traffic toward slice-front accounts (NENYA-40 review).
	lastPicked int
}

// weightTick is the virtual-clock quantum an account's LastUsed advances per
// pick at weight 1. With all weights equal, selection is classic LRU; higher
// weights advance the clock more slowly and therefore win proportionally more
// picks. The value only sets relative pacing: absolute divergence from wall
// time is bounded on the lagging side by clampVirtualClock and on the leading
// side by renormalizeVirtualClocks, so persisted LastUsed values stay
// wall-recognizable.
const weightTick = time.Second

// effectiveWeight normalizes the configured weight: values below 1 select
// the default share of 1.
func effectiveWeight(account *config.ProviderAccount) int {
	if account.Weight < 1 {
		return 1
	}
	return account.Weight
}

// clampVirtualClock bounds how far an account's selection clock may lag
// behind wall time. Accounts absent from rotation (cooldowns, exclusions,
// freshly loaded persistence state) re-enter selection on equal footing
// within one tick instead of hoarding a backlog of picks proportional to
// their accumulated idle time. Clocks ahead of wall time are handled by
// renormalizeVirtualClocks. Caller must hold p.mu (write).
func clampVirtualClock(account *config.ProviderAccount, now time.Time) {
	if floor := now.Add(-weightTick); account.LastUsed.Before(floor) {
		account.LastUsed = floor
	}
}

// advanceVirtualClock consumes one pick: the account's selection clock
// advances by weightTick/weight. This is the only selection state in the
// pool — it lives on the account identity, never on a filtered slice
// position, so retry exclusions and cooldowns cannot re-seat rotation
// (the CLIProxyAPI 11331x skew class, NENYA-40). The tick is floored at
// 1ns so absurdly large weights degrade to a hot account rather than a
// frozen clock that would capture 100% of picks.
// Caller must hold p.mu (write).
func advanceVirtualClock(account *config.ProviderAccount) {
	tick := weightTick / time.Duration(effectiveWeight(account))
	if tick <= 0 {
		tick = time.Nanosecond
	}
	account.LastUsed = account.LastUsed.Add(tick)
}

// renormalizeVirtualClocks translates every account clock by a common offset
// so the least-recently-used available account sits at the recency floor
// (now-weightTick). Translation preserves relative order, so selection
// outcomes are unchanged, while keeping absolute clocks within a bounded
// window of wall time: without it, sustained pick rates push all clocks
// ahead of wall time without limit (min clock rises one tick per pick), and
// an account re-entering after filtering would then monopolize picks
// proportional to uptime until it caught up. Caller must hold p.mu (write).
func (p *AccountPool) renormalizeVirtualClocks(minAvailable, now time.Time) {
	if shift := minAvailable.Sub(now.Add(-weightTick)); shift > 0 {
		for _, acc := range p.accounts {
			acc.LastUsed = acc.LastUsed.Add(-shift)
		}
	}
}

// NewAccountPool creates a new account pool for the given provider.
// Accounts must be non-nil entries; nil entries are not supported.
func NewAccountPool(provider string, accounts []*config.ProviderAccount) *AccountPool {
	return &AccountPool{
		provider:   provider,
		accounts:   accounts,
		lastPicked: -1,
	}
}

// SelectAccount picks the best account for the given model.
// Uses weighted least-recently-used selection (WFQ-style); see AccountPool.
// Returns a copy of the selected account's immutable fields; caller
// must use ReportError/ReportSuccess to mutate pool state.
func (p *AccountPool) SelectAccount(ctx context.Context, model string) (*SelectedAccount, error) {
	return p.SelectAccountExcluding(ctx, model, nil)
}

// SelectAccountExcluding picks the least-recently-used healthy account for
// the given model, skipping accounts in the exclude set (by ID) on top of the
// standard health gates (active, not rate-limited, not model-locked). Used by
// callers with out-of-band health knowledge (e.g. billing exhaustion) to
// retry sibling selection without re-selecting a known-bad account.
//
// Fairness under filtering (NENYA-40): selection is min-LastUsed over the
// *identity* of each account, never a position within the filtered slice, so
// shrinking candidate sets (retries, cooldowns, exclusions) cannot re-seat
// rotation. Ties break deterministically by configured order.
//
// The ctx parameter is currently unused; it is kept for signature symmetry
// with SelectAccount and future storage-aware selection.
// availabilityMask computes which accounts pass the health gates for the
// given model: not excluded, active, not rate-limited, not model-locked.
// Passing accounts have their selection clocks clamped (caller must hold
// p.mu, write).
func (p *AccountPool) availabilityMask(model string, now time.Time, exclude map[string]bool) []bool {
	available := make([]bool, len(p.accounts))
	for i, acc := range p.accounts {
		if exclude[acc.ID] {
			continue
		}
		if acc.Status != config.AccountStatusActive {
			continue
		}
		if now.Before(acc.RateLimitedUntil) {
			continue
		}
		if p.isModelLocked(acc, model, now) {
			continue
		}
		clampVirtualClock(acc, now)
		available[i] = true
	}
	return available
}

func (p *AccountPool) SelectAccountExcluding(ctx context.Context, model string, exclude map[string]bool) (*SelectedAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	available := p.availabilityMask(model, now, exclude)

	// Rotated min-scan: begin after the last pick so ties rotate fairly.
	start := 0
	if p.lastPicked >= 0 && p.lastPicked < len(p.accounts) {
		start = (p.lastPicked + 1) % len(p.accounts)
	}
	selIdx := -1
	for off := 0; off < len(p.accounts); off++ {
		idx := (start + off) % len(p.accounts)
		if !available[idx] {
			continue
		}
		if selIdx == -1 || p.accounts[idx].LastUsed.Before(p.accounts[selIdx].LastUsed) {
			selIdx = idx
		}
	}
	if selIdx == -1 {
		return nil, &NoAvailableAccountError{Provider: p.provider}
	}

	selected := p.accounts[selIdx]
	p.lastPicked = selIdx
	advanceVirtualClock(selected)

	minClock := selected.LastUsed
	for i, acc := range p.accounts {
		if available[i] && acc.LastUsed.Before(minClock) {
			minClock = acc.LastUsed
		}
	}
	p.renormalizeVirtualClocks(minClock, now)

	return &SelectedAccount{
		ID:         selected.ID,
		Credential: selected.Credential,
	}, nil
}

// SelectAccountByID selects a specific account by ID, bypassing LRU iteration.
// Used by sticky session routing to pin a session to the account it was
// previously served by. The account is returned only when healthy for the
// given model: present in the pool, active, not rate-limited (cooldown), and
// not model-locked — each failure mode is reported via NoAvailableAccountError
// with a distinct Reason. On success LastUsed is updated, keeping LRU
// selection fair for unpinned sessions. Returns a copy of the selected
// account's immutable fields; caller must use ReportError/ReportSuccess to
// mutate pool state. Billing exhaustion is not checked here — callers holding
// a BillingTracker must layer that gate on top to keep this package
// billing-agnostic. The ctx parameter is currently unused; it is kept for
// signature symmetry with SelectAccount and future storage-aware selection.
func (p *AccountPool) SelectAccountByID(ctx context.Context, accountID, model string) (*SelectedAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	acc := p.findAccount(accountID)
	if acc == nil {
		return nil, &NoAvailableAccountError{Provider: p.provider, Reason: ReasonNotFound}
	}
	if acc.Status != config.AccountStatusActive {
		return nil, &NoAvailableAccountError{Provider: p.provider, Reason: ReasonInactive}
	}
	if now.Before(acc.RateLimitedUntil) {
		return nil, &NoAvailableAccountError{Provider: p.provider, Reason: ReasonCooling}
	}
	if p.isModelLocked(acc, model, now) {
		return nil, &NoAvailableAccountError{Provider: p.provider, Reason: ReasonModelLocked}
	}
	// Sticky picks push the account out of the unpinned rotation by moving
	// its clock to at least wall-now (max, never backwards): virtual clocks
	// of active accounts may already run ahead of wall time under sustained
	// traffic, and rewinding those would unfairly re-seat rotation.
	if now.After(acc.LastUsed) {
		acc.LastUsed = now
	}

	return &SelectedAccount{
		ID:         acc.ID,
		Credential: acc.Credential,
	}, nil
}

// isModelLocked checks if the account has a live lock for the given model at
// the provided instant.
func (p *AccountPool) isModelLocked(account *config.ProviderAccount, model string, now time.Time) bool {
	until, locked := account.ModelLocks[model]
	if !locked {
		return false
	}
	return now.Before(until)
}

// ApplyError applies an error state to the account.
// Updates LastError, RateLimitedUntil, BackoffLevel, and Status based on error classification.
func (p *AccountPool) ApplyError(accountID string, status int, message string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	account := p.findAccount(accountID)
	if account == nil {
		return nil
	}

	account.LastError = &config.ErrorRecord{
		Status:    status,
		Message:   message,
		Timestamp: time.Now(),
	}

	// Classify error and set cooldown/backoff
	decision := ClassifyError(status, message, account.BackoffLevel)

	if decision.ShouldFallback {
		account.BackoffLevel = decision.NewBackoffLevel
		if decision.CooldownMs > 0 {
			account.RateLimitedUntil = time.Now().Add(time.Duration(decision.CooldownMs) * time.Millisecond)
		}
		account.Status = config.AccountStatusError
	}

	return nil
}

// findAccount finds an account by ID under the lock.
func (p *AccountPool) findAccount(id string) *config.ProviderAccount {
	for _, acc := range p.accounts {
		if acc.ID == id {
			return acc
		}
	}
	return nil
}

// ReportSuccess reports a successful request for an account.
// Resets BackoffLevel and clears RateLimitedUntil if the account was in error state.
func (p *AccountPool) ReportSuccess(accountID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	account := p.findAccount(accountID)
	if account == nil {
		return nil
	}

	account.BackoffLevel = 0
	account.RateLimitedUntil = time.Time{}
	if account.Status == config.AccountStatusError {
		account.Status = config.AccountStatusActive
	}
	return nil
}

// GetAccount returns an account by ID without updating LastUsed.
// The returned pointer references internal mutable state — callers MUST NOT
// mutate the struct. Used only for read-only access (e.g., persistence).
func (p *AccountPool) GetAccount(accountID string) *config.ProviderAccount {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, acc := range p.accounts {
		if acc.ID == accountID {
			return acc
		}
	}
	return nil
}

// ListAccounts returns all accounts in the pool.
func (p *AccountPool) ListAccounts() []*config.ProviderAccount {
	p.mu.RLock()
	defer p.mu.RUnlock()

	accounts := make([]*config.ProviderAccount, len(p.accounts))
	copy(accounts, p.accounts)
	return accounts
}

// LockModel locks a model for the account until the specified time.
// Used to prevent concurrent requests to the same model from the same account.
func (p *AccountPool) LockModel(accountID, model string, until time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	account := p.findAccount(accountID)
	if account == nil {
		return errors.New("account not found")
	}

	if account.ModelLocks == nil {
		account.ModelLocks = make(map[string]time.Time)
	}
	account.ModelLocks[model] = until
	return nil
}

// ExpiredLocks removes model locks that have expired.
// Should be called periodically to clean up stale locks.
func (p *AccountPool) ExpiredLocks() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	count := 0
	for _, acc := range p.accounts {
		for model, until := range acc.ModelLocks {
			if now.After(until) {
				delete(acc.ModelLocks, model)
				count++
			}
		}
	}
	return count
}

// ListAccountIDs returns all account IDs in the pool.
func (p *AccountPool) ListAccountIDs(ctx context.Context) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()

	ids := make([]string, len(p.accounts))
	for i, acc := range p.accounts {
		ids[i] = acc.ID
	}
	return ids
}
