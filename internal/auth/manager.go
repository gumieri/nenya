package auth

import (
	"context"
	"sync"
	"time"

	"github.com/nenya/config"
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
	selected, err := m.SelectAccount(ctx, provider, model)
	if err != nil {
		return "", err
	}
	return selected.Credential, nil
}

// SelectAccount selects an account and returns the full SelectedAccount
// including both the account ID and credential string. Use this when the
// caller needs the account ID for billing tracking or observability.
func (m *AccountManager) SelectAccount(ctx context.Context, provider, model string) (*SelectedAccount, error) {
	return m.SelectAccountExcluding(ctx, provider, model, nil)
}

// SelectAccountExcluding selects the least-recently-used healthy account for
// the model, skipping the excluded account IDs (e.g. accounts known to be
// billing-exhausted). See AccountPool.SelectAccountExcluding.
func (m *AccountManager) SelectAccountExcluding(ctx context.Context, provider, model string, exclude map[string]bool) (*SelectedAccount, error) {
	pool, err := m.GetPool(ctx, provider)
	if err != nil {
		return nil, err
	}
	return pool.SelectAccountExcluding(ctx, model, exclude)
}

// SelectAccountByID returns the specific account identified by accountID for
// a sticky session, skipping LRU iteration. The account is returned only when
// healthy for the given model: present in the pool, active, not in cooldown,
// and not model-locked. Billing exhaustion is not checked here — callers
// holding a BillingTracker must layer that gate on top, keeping this package
// free of billing dependencies.
func (m *AccountManager) SelectAccountByID(ctx context.Context, provider, model, accountID string) (*SelectedAccount, error) {
	pool, err := m.GetPool(ctx, provider)
	if err != nil {
		return nil, err
	}
	return pool.SelectAccountByID(ctx, accountID, model)
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
		account := pool.AccountSnapshot(accountID)
		if account != nil {
			return m.storage.SaveAccount(provider, account)
		}
	}

	return nil
}

// ReportErrorForModel reports an error observed on a specific model
// (NENYA-170): model-scoped signals lock only that model on the account,
// leaving the account's other models servable; account-scoped signals
// bench the credential as before. This is the entry point for
// model-aware callers — the routing layer continues to bench per CoolKey
// independently. Persists the account when storage is configured.
func (m *AccountManager) ReportErrorForModel(provider, accountID, model string, status int, message string) error {
	m.mu.RLock()
	pool, ok := m.pools[provider]
	m.mu.RUnlock()

	if !ok {
		return nil
	}

	if err := pool.ApplyErrorForModel(accountID, model, status, message); err != nil {
		return err
	}

	if m.storage != nil {
		account := pool.AccountSnapshot(accountID)
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
		// AUD-43 (NENYA-232): persist a SNAPSHOT. AccountSnapshot returns the
		// live pointer with the pool lock already released, so SaveAccount
		// would marshal the maps while a concurrent ApplyError/ReportSuccess
		// mutates them — the exact hazard AccountSnapshot's doc names. The
		// ApplyError path above has always used the snapshot; this path
		// simply predates it.
		account := pool.AccountSnapshot(accountID)
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

// DefaultLockSweepInterval is how often a LockSweeper sweeps when no
// explicit interval is given. Model locks are cooldown-scale (seconds to
// minutes), so a one-minute cadence bounds the maps' growth far below any
// operator-visible size while the sweep itself touches only small maps
// under the pool lock.
const DefaultLockSweepInterval = time.Minute

// LockSweeper periodically removes expired model locks (and their paired
// per-model backoff levels) across every pool the manager holds
// (AUD-44 / NENYA-230). Without it, every (account, model) pair that ever
// received a model-scoped lock stayed in Account.ModelLocks /
// ModelBackoffLevels forever — an unbounded-growth leak on long-lived
// processes, paid again on every account-state snapshot.
//
// The goroutine starts in the constructor and runs until Stop; a gateway
// owns one per AccountManager and stops it in Close.
type LockSweeper struct {
	manager  *AccountManager
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

// NewLockSweeper builds a sweeper for the manager and starts its goroutine.
// A non-positive interval selects DefaultLockSweepInterval.
func NewLockSweeper(m *AccountManager, interval time.Duration) *LockSweeper {
	if interval <= 0 {
		interval = DefaultLockSweepInterval
	}
	s := &LockSweeper{
		manager:  m,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go s.run()
	return s
}

// run sweeps until Stop. Lifecycle: exits on stop, closing done.
func (s *LockSweeper) run() {
	defer close(s.done)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.manager.CleanupExpiredLocks()
		}
	}
}

// Stop ends the sweep goroutine and waits for it to exit. Safe to call more
// than once and safe even if the sweeper already stopped.
func (s *LockSweeper) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	<-s.done
}

// Stopped reports whether the sweep goroutine has exited (test seam).
func (s *LockSweeper) Stopped() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}
