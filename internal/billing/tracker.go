package billing

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

type SpendEntry struct {
	ProviderName string
	AccountName  string
	RequestID    string
	InputTokens  int
	OutputTokens int
	CostUSD      float64
	Timestamp    time.Time
}

type AccountStatus struct {
	AccountName string
	Provider    string
	TotalSpend  *atomic.Int64
	IsExhausted *atomic.Bool
	ExhaustedAt time.Time
	LastResetAt time.Time
}

type BillingTracker struct {
	logger  *slog.Logger
	mu      sync.RWMutex
	metrics MetricRecorder

	accounts map[string]*AccountStatus

	TotalSpendUSD  atomic.Int64
	TotalRequests  atomic.Uint64
	ExhaustedCount atomic.Uint64
}

type MetricRecorder interface {
	RecordBillingSpend(provider, account string, spendCents int64)
	RecordBillingExhausted(provider, account string)
}

func NewBillingTracker(logger *slog.Logger, metrics MetricRecorder) *BillingTracker {
	return &BillingTracker{
		logger:   logger,
		accounts: make(map[string]*AccountStatus),
		metrics:  metrics,
	}
}

func (bt *BillingTracker) RecordSpend(ctx context.Context, entry SpendEntry) {
	key := entry.ProviderName + ":" + entry.AccountName

	bt.mu.RLock()
	account, exists := bt.accounts[key]
	bt.mu.RUnlock()

	if !exists {
		bt.mu.Lock()
		account = &AccountStatus{
			AccountName: entry.AccountName,
			Provider:    entry.ProviderName,
			TotalSpend:  new(atomic.Int64),
			IsExhausted: new(atomic.Bool),
			ExhaustedAt: time.Time{},
			LastResetAt: time.Time{},
		}
		bt.accounts[key] = account
		bt.mu.Unlock()
	}

	costInCents := int64(entry.CostUSD * 100)
	if costInCents < 0 {
		costInCents = 0
	}
	account.TotalSpend.Add(costInCents)
	bt.TotalSpendUSD.Add(costInCents)
	bt.TotalRequests.Add(1)

	if bt.metrics != nil {
		bt.metrics.RecordBillingSpend(entry.ProviderName, entry.AccountName, costInCents)
	}

	if bt.logger != nil {
		bt.logger.DebugContext(ctx, "spend recorded",
			"provider", entry.ProviderName,
			"account", entry.AccountName,
			"request_id", entry.RequestID,
			"cost_usd", entry.CostUSD,
			"input_tokens", entry.InputTokens,
			"output_tokens", entry.OutputTokens,
		)
	}
}

func (bt *BillingTracker) MarkExhausted(ctx context.Context, provider, account string, reason string) {
	key := provider + ":" + account

	bt.mu.Lock()
	defer bt.mu.Unlock()

	accountStatus, exists := bt.accounts[key]
	if !exists {
		accountStatus = &AccountStatus{
			AccountName: account,
			Provider:    provider,
			TotalSpend:  new(atomic.Int64),
			IsExhausted: new(atomic.Bool),
			ExhaustedAt: time.Time{},
			LastResetAt: time.Time{},
		}
		bt.accounts[key] = accountStatus
	}

	accountStatus.IsExhausted.Store(true)
	accountStatus.ExhaustedAt = time.Now()
	bt.ExhaustedCount.Add(1)

	if bt.metrics != nil {
		bt.metrics.RecordBillingExhausted(provider, account)
	}

	if bt.logger != nil {
		bt.logger.WarnContext(ctx, "billing account marked exhausted",
			"provider", provider,
			"account", account,
			"reason", reason,
		)
	}
}

func (bt *BillingTracker) IsExhausted(provider, account string) bool {
	key := provider + ":" + account

	bt.mu.RLock()
	accountStatus, exists := bt.accounts[key]
	bt.mu.RUnlock()

	if !exists {
		return false
	}
	return accountStatus.IsExhausted.Load()
}

func (bt *BillingTracker) GetTotalSpend(provider, account string) float64 {
	key := provider + ":" + account

	bt.mu.RLock()
	accountStatus, exists := bt.accounts[key]
	bt.mu.RUnlock()

	if !exists {
		return 0
	}
	return float64(accountStatus.TotalSpend.Load()) / 100
}

func (bt *BillingTracker) GetUtilizationRatio(provider, account string, limitUSD float64) float64 {
	if limitUSD <= 0 {
		return 0
	}
	spend := bt.GetTotalSpend(provider, account)
	return spend / limitUSD
}

func (bt *BillingTracker) ResetSpend(ctx context.Context, provider, account string) {
	key := provider + ":" + account

	bt.mu.Lock()
	accountStatus, exists := bt.accounts[key]
	if !exists {
		bt.mu.Unlock()
		return
	}

	prevSpend := accountStatus.TotalSpend.Swap(0)
	// Re-read the global total from all accounts to avoid races
	// with concurrent RecordSpend calls that may have modified
	// the account spend between the Swap and this read.
	var total int64
	for _, acc := range bt.accounts {
		total += acc.TotalSpend.Load()
	}
	bt.TotalSpendUSD.Store(total)
	accountStatus.IsExhausted.Store(false)
	accountStatus.ExhaustedAt = time.Time{}
	accountStatus.LastResetAt = time.Now()
	bt.mu.Unlock()

	if bt.logger != nil {
		bt.logger.InfoContext(ctx, "billing spend reset",
			"provider", provider,
			"account", account,
			"previous_spend_cents", prevSpend,
		)
	}
}

func (bt *BillingTracker) GetAllAccounts() []AccountStatus {
	bt.mu.RLock()
	defer bt.mu.RUnlock()

	result := make([]AccountStatus, 0, len(bt.accounts))
	for _, acc := range bt.accounts {
		status := AccountStatus{
			AccountName: acc.AccountName,
			Provider:    acc.Provider,
			TotalSpend:  acc.TotalSpend,
			IsExhausted: acc.IsExhausted,
			ExhaustedAt: acc.ExhaustedAt,
			LastResetAt: acc.LastResetAt,
		}
		result = append(result, status)
	}
	return result
}
