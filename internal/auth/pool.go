package auth

import (
	"context"
	"errors"
	"sync"
	"time"

	"nenya/config"
)

type SelectedAccount struct {
	ID         string
	Credential string
}

// AccountPool manages multiple provider accounts with mutex-guarded selection.
// Implements LRU (Least Recently Used) strategy for load distribution.
type AccountPool struct {
	mu       sync.RWMutex
	provider string
	accounts []*config.ProviderAccount
}

// NewAccountPool creates a new account pool for the given provider.
func NewAccountPool(provider string, accounts []*config.ProviderAccount) *AccountPool {
	return &AccountPool{
		provider: provider,
		accounts: accounts,
	}
}

// SelectAccount picks the best account for the given model.
// Uses LRU (least recently used) strategy for load distribution.
// Returns a copy of the selected account's immutable fields; caller
// must use ReportError/ReportSuccess to mutate pool state.
func (p *AccountPool) SelectAccount(ctx context.Context, model string) (*SelectedAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	var available []*config.ProviderAccount
	for _, acc := range p.accounts {
		if acc.Status != config.AccountStatusActive {
			continue
		}
		if now.Before(acc.RateLimitedUntil) {
			continue
		}
		if p.isModelLocked(acc, model) {
			continue
		}
		available = append(available, acc)
	}

	if len(available) == 0 {
		return nil, &NoAvailableAccountError{Provider: p.provider}
	}

	selected := available[0]
	for _, acc := range available[1:] {
		if acc.LastUsed.Before(selected.LastUsed) {
			selected = acc
		}
	}
	selected.LastUsed = now

	return &SelectedAccount{
		ID:         selected.ID,
		Credential: selected.Credential,
	}, nil
}

// isModelLocked checks if the account has a lock for the given model.
func (p *AccountPool) isModelLocked(account *config.ProviderAccount, model string) bool {
	until, locked := account.ModelLocks[model]
	if !locked {
		return false
	}
	return time.Now().Before(until)
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
