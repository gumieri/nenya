package infra

import (
	"sync"
	"sync/atomic"
)

type CostTracker struct {
	mu     sync.RWMutex
	costs  map[string]*atomic.Int64
	errors map[string]*atomic.Int64
}

func NewCostTracker() *CostTracker {
	return &CostTracker{
		costs:  make(map[string]*atomic.Int64),
		errors: make(map[string]*atomic.Int64),
	}
}

func (ct *CostTracker) RecordUsage(model string, costCents int64) {
	ct.mu.Lock()
	if ct.costs[model] == nil {
		ct.costs[model] = new(atomic.Int64)
	}
	ct.costs[model].Add(costCents)
	ct.mu.Unlock()
}

func (ct *CostTracker) RecordError(model string) {
	ct.mu.Lock()
	if ct.errors[model] == nil {
		ct.errors[model] = new(atomic.Int64)
	}
	ct.errors[model].Add(1)
	ct.mu.Unlock()
}

func (ct *CostTracker) GetCost(model string) int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if c, ok := ct.costs[model]; ok {
		return c.Load()
	}
	return 0
}

func (ct *CostTracker) GetErrorCount(model string) int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if c, ok := ct.errors[model]; ok {
		return c.Load()
	}
	return 0
}

func (ct *CostTracker) GetAllCosts() map[string]int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	result := make(map[string]int64, len(ct.costs))
	for model, c := range ct.costs {
		result[model] = c.Load()
	}
	return result
}

func (ct *CostTracker) GetAllErrors() map[string]int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	result := make(map[string]int64, len(ct.errors))
	for model, c := range ct.errors {
		result[model] = c.Load()
	}
	return result
}

type CostSnapshot struct {
	TotalCostCents int64            `json:"total_cost_cents"`
	ModelCosts     map[string]int64 `json:"model_costs"`
	ModelErrors    map[string]int64 `json:"model_errors"`
}

func (ct *CostTracker) Snapshot() CostSnapshot {
	return CostSnapshot{
		ModelCosts:  ct.GetAllCosts(),
		ModelErrors: ct.GetAllErrors(),
	}
}
