package auth

import (
	"context"
	"sync"

	"nenya/config"
)

// AccountStorage defines the interface for persisting account state.
// Implementations can use JSON files, databases, or other storage backends.
type AccountStorage interface {
	// LoadAccounts loads all accounts for a provider.
	LoadAccounts(provider string) ([]*config.ProviderAccount, error)
	// SaveAccount persists an account's state (called after status changes).
	SaveAccount(provider string, account *config.ProviderAccount) error
}

// AccountManager manages multiple account pools for different providers.
// It provides thread-safe access to account pools and handles lazy initialization.
type AccountManager struct {
	mu      sync.RWMutex
	pools   map[string]*AccountPool
	storage AccountStorage
}

// NewAccountManager creates a new account manager with the given storage backend.
func NewAccountManager(storage AccountStorage) *AccountManager {
	return &AccountManager{
		pools:   make(map[string]*AccountPool),
		storage: storage,
	}
}

// RegisterPool registers a pre-built pool for a provider.
// Used during gateway initialization to seed pools from config.
func (m *AccountManager) RegisterPool(provider string, pool *AccountPool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pools[provider] = pool
}

// InitializePool loads accounts for a provider and creates a pool.
// Safe to call multiple times for the same provider.
func (m *AccountManager) InitializePool(ctx context.Context, provider string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.pools[provider]; ok {
		return nil
	}

	accounts := []*config.ProviderAccount{}
	if m.storage != nil {
		loaded, err := m.storage.LoadAccounts(provider)
		if err != nil {
			return err
		}
		if len(loaded) > 0 {
			accounts = loaded
		}
	}

	pool := NewAccountPool(provider, accounts)
	m.pools[provider] = pool
	return nil
}

// GetPool returns the account pool for the given provider.
// Initializes the pool lazily under a write lock if it doesn't exist.
func (m *AccountManager) GetPool(ctx context.Context, provider string) (*AccountPool, error) {
	m.mu.RLock()
	pool, ok := m.pools[provider]
	m.mu.RUnlock()
	if ok {
		return pool, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if pool, ok = m.pools[provider]; ok {
		return pool, nil
	}

	accounts := []*config.ProviderAccount{}
	if m.storage != nil {
		loaded, err := m.storage.LoadAccounts(provider)
		if err != nil {
			return nil, err
		}
		if len(loaded) > 0 {
			accounts = loaded
		}
	}

	pool = NewAccountPool(provider, accounts)
	m.pools[provider] = pool
	return pool, nil
}

// SelectCredential selects an account and returns its credential string.
// This is the primary API for key injection — returns only the credential,
// not a mutable pointer to the internal account struct.
func (m *AccountManager) SelectCredential(ctx context.Context, provider, model string) (string, error) {
	pool, err := m.GetPool(ctx, provider)
	if err != nil {
		return "", err
	}
	selected, err := pool.SelectAccount(ctx, model)
	if err != nil {
		return "", err
	}
	return selected.Credential, nil
}

// ReportError reports an error for an account.
// Updates the account's status, cooldown, and backoff level.
func (m *AccountManager) ReportError(provider, accountID string, status int, message string) error {
	m.mu.RLock()
	pool, ok := m.pools[provider]
	m.mu.RUnlock()

	if !ok {
		return nil
	}

	if err := pool.ApplyError(accountID, status, message); err != nil {
		return err
	}

	if m.storage != nil {
		account := pool.GetAccount(accountID)
		if account != nil {
			return m.storage.SaveAccount(provider, account)
		}
	}

	return nil
}

// ReportSuccess reports a successful request for an account.
// Resets backoff and cooldown, reactivating the account if it was in error state.
func (m *AccountManager) ReportSuccess(provider, accountID string) error {
	m.mu.RLock()
	pool, ok := m.pools[provider]
	m.mu.RUnlock()

	if !ok {
		return nil
	}

	if err := pool.ReportSuccess(accountID); err != nil {
		return err
	}

	if m.storage != nil {
		account := pool.GetAccount(accountID)
		if account != nil {
			return m.storage.SaveAccount(provider, account)
		}
	}

	return nil
}

// ListProviders returns all provider names that have pools initialized.
func (m *AccountManager) ListProviders() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	providers := make([]string, 0, len(m.pools))
	for name := range m.pools {
		providers = append(providers, name)
	}
	return providers
}

// ListAccounts returns all accounts for a given provider.
func (m *AccountManager) ListAccounts(provider string) []*config.ProviderAccount {
	m.mu.RLock()
	pool, ok := m.pools[provider]
	m.mu.RUnlock()

	if !ok {
		return nil
	}

	return pool.ListAccounts()
}

// CleanupExpiredLocks removes expired model locks across all pools.
// Safe to call concurrently — acquires manager RLock (protects pool map)
// and each pool's own write lock independently.
func (m *AccountManager) CleanupExpiredLocks() int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	total := 0
	for _, pool := range m.pools {
		total += pool.ExpiredLocks()
	}
	return total
}
