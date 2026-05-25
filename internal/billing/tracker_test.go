package billing

import (
	"context"
	"testing"
	"time"
)

func TestNewBillingTracker(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	if bt == nil {
		t.Fatal("NewBillingTracker returned nil")
	}
	if bt.TotalRequests.Load() != 0 {
		t.Errorf("Initial requests count should be 0, got %d", bt.TotalRequests.Load())
	}
	if bt.TotalSpendUSD.Load() != 0 {
		t.Errorf("Initial spend should be 0, got %d", bt.TotalSpendUSD.Load())
	}
}

func TestRecordSpend(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	entry := SpendEntry{
		ProviderName: "openai",
		AccountName:  "account1",
		RequestID:    "req1",
		InputTokens:  100,
		OutputTokens: 50,
		CostUSD:      0.01,
		Timestamp:    time.Now(),
	}

	bt.RecordSpend(ctx, entry)

	if bt.TotalRequests.Load() != 1 {
		t.Errorf("Expected 1 request, got %d", bt.TotalRequests.Load())
	}

	// 0.01 USD = 1 cent in the internal representation
	if bt.TotalSpendUSD.Load() != 1 {
		t.Errorf("Expected spend 1 cent, got %d", bt.TotalSpendUSD.Load())
	}

	spend := bt.GetTotalSpend("openai", "account1")
	expectedSpend := 0.01
	if spend != expectedSpend {
		t.Errorf("Expected spend %f, got %f", expectedSpend, spend)
	}
}

func TestRecordMultipleSpend(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	entries := []SpendEntry{
		{
			ProviderName: "openai",
			AccountName:  "account1",
			CostUSD:      0.01,
			Timestamp:    time.Now(),
		},
		{
			ProviderName: "openai",
			AccountName:  "account1",
			CostUSD:      0.02,
			Timestamp:    time.Now(),
		},
		{
			ProviderName: "anthropic",
			AccountName:  "account2",
			CostUSD:      0.03,
			Timestamp:    time.Now(),
		},
	}

	for _, entry := range entries {
		bt.RecordSpend(ctx, entry)
	}

	if bt.TotalRequests.Load() != 3 {
		t.Errorf("Expected 3 requests, got %d", bt.TotalRequests.Load())
	}

	spend1 := bt.GetTotalSpend("openai", "account1")
	expectedSpend1 := 0.03
	if spend1 != expectedSpend1 {
		t.Errorf("Expected spend %f, got %f", expectedSpend1, spend1)
	}

	spend2 := bt.GetTotalSpend("anthropic", "account2")
	expectedSpend2 := 0.03
	if spend2 != expectedSpend2 {
		t.Errorf("Expected spend %f, got %f", expectedSpend2, spend2)
	}
}

func TestMarkExhausted(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	bt.MarkExhausted(ctx, "openai", "account1", "balance exhausted")

	if !bt.IsExhausted("openai", "account1") {
		t.Error("Expected account to be marked exhausted")
	}

	if bt.IsExhausted("openai", "account2") {
		t.Error("Expected account2 to not be exhausted")
	}

	if bt.ExhaustedCount.Load() != 1 {
		t.Errorf("Expected exhausted count 1, got %d", bt.ExhaustedCount.Load())
	}

	accts := bt.GetAllAccounts()
	if len(accts) != 1 {
		t.Fatalf("Expected 1 account, got %d", len(accts))
	}
	if accts[0].ExhaustedAt.IsZero() {
		t.Error("ExhaustedAt should not be zero after MarkExhausted")
	}
}

func TestGetAllAccounts(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	bt.RecordSpend(ctx, SpendEntry{
		ProviderName: "openai",
		AccountName:  "account1",
		CostUSD:      0.01,
		Timestamp:    time.Now(),
	})

	bt.RecordSpend(ctx, SpendEntry{
		ProviderName: "anthropic",
		AccountName:  "account2",
		CostUSD:      0.02,
		Timestamp:    time.Now(),
	})

	accounts := bt.GetAllAccounts()
	if len(accounts) != 2 {
		t.Errorf("Expected 2 accounts, got %d", len(accounts))
	}

	bt.MarkExhausted(ctx, "openai", "account1", "test")

	accounts = bt.GetAllAccounts()
	exhaustedCount := 0
	for _, acc := range accounts {
		if acc.IsExhausted.Load() {
			exhaustedCount++
		}
	}
	if exhaustedCount != 1 {
		t.Errorf("Expected 1 exhausted account, got %d", exhaustedCount)
	}
}

func TestGetTotalSpendNonExistent(t *testing.T) {
	bt := NewBillingTracker(nil, nil)

	spend := bt.GetTotalSpend("nonexistent", "nonexistent")
	if spend != 0 {
		t.Errorf("Expected spend 0 for non-existent account, got %f", spend)
	}
}

func TestIsExhaustedNonExistent(t *testing.T) {
	bt := NewBillingTracker(nil, nil)

	if bt.IsExhausted("nonexistent", "nonexistent") {
		t.Error("Expected non-existent account to not be exhausted")
	}
}

func TestGetUtilizationRatio(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	ratio := bt.GetUtilizationRatio("openai", "account1", 100.0)
	if ratio != 0 {
		t.Errorf("Expected ratio 0 for no spend, got %f", ratio)
	}

	bt.RecordSpend(ctx, SpendEntry{
		ProviderName: "openai",
		AccountName:  "account1",
		CostUSD:      25.00,
	})
	ratio = bt.GetUtilizationRatio("openai", "account1", 100.0)
	if ratio != 0.25 {
		t.Errorf("Expected ratio 0.25, got %f", ratio)
	}

	ratio = bt.GetUtilizationRatio("openai", "account1", 0)
	if ratio != 0 {
		t.Errorf("Expected ratio 0 for zero limit, got %f", ratio)
	}

	ratio = bt.GetUtilizationRatio("openai", "account1", -1)
	if ratio != 0 {
		t.Errorf("Expected ratio 0 for negative limit, got %f", ratio)
	}
}

func TestResetSpend(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	bt.RecordSpend(ctx, SpendEntry{
		ProviderName: "openai",
		AccountName:  "account1",
		CostUSD:      50.00,
	})

	if spend := bt.GetTotalSpend("openai", "account1"); spend != 50.0 {
		t.Fatalf("Expected spend 50, got %f", spend)
	}

	bt.MarkExhausted(ctx, "openai", "account1", "over limit")
	if !bt.IsExhausted("openai", "account1") {
		t.Fatal("Expected account to be exhausted")
	}

	bt.ResetSpend(ctx, "openai", "account1")

	if spend := bt.GetTotalSpend("openai", "account1"); spend != 0 {
		t.Errorf("Expected spend 0 after reset, got %f", spend)
	}
	if bt.IsExhausted("openai", "account1") {
		t.Error("Expected account to not be exhausted after reset")
	}

	bt.ResetSpend(ctx, "nonexistent", "nonexistent")
}

func TestResetSpend_ConcurrentSafety(t *testing.T) {
	bt := NewBillingTracker(nil, nil)
	ctx := context.Background()

	bt.RecordSpend(ctx, SpendEntry{
		ProviderName: "openai",
		AccountName:  "account1",
		CostUSD:      100.00,
	})

	const numGoroutines = 50
	done := make(chan bool, numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer func() { done <- true }()
			if id%3 == 0 {
				bt.RecordSpend(ctx, SpendEntry{
					ProviderName: "openai",
					AccountName:  "account1",
					CostUSD:      0.01,
				})
			} else if id%3 == 1 {
				bt.ResetSpend(ctx, "openai", "account1")
			}
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		<-done
	}

	total := bt.TotalSpendUSD.Load()
	if total < 0 {
		t.Errorf("TotalSpendUSD went negative: %d (underflow detected)", total)
	}
}