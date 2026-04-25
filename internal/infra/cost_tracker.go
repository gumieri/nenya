package infra

import (
	"math"
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

func (ct *CostTracker) RecordUsage(model string, costUSD float64) {
	microUSD := int64(math.Round(costUSD * 1e6))
	if microUSD == 0 && costUSD != 0 {
		microUSD = 1
	}
	ct.mu.Lock()
	if ct.costs[model] == nil {
		ct.costs[model] = new(atomic.Int64)
	}
	ct.costs[model].Add(microUSD)
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

func (ct *CostTracker) GetCostMicroUSD(model string) int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if c, ok := ct.costs[model]; ok {
		return c.Load()
	}
	return 0
}

func (ct *CostTracker) GetCostUSD(model string) float64 {
	return float64(ct.GetCostMicroUSD(model)) / 1e6
}

func (ct *CostTracker) GetErrorCount(model string) int64 {
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if c, ok := ct.errors[model]; ok {
		return c.Load()
	}
	return 0
}

func (ct *CostTracker) GetAllCostsMicroUSD() map[string]int64 {
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
	TotalCostMicroUSD int64            `json:"total_cost_micro_usd"`
	ModelCosts        map[string]int64 `json:"model_costs_micro_usd"`
	ModelErrors       map[string]int64 `json:"model_errors"`
}

func (ct *CostTracker) Snapshot() CostSnapshot {
	modelCosts := ct.GetAllCostsMicroUSD()
	modelErrors := ct.GetAllErrors()
	var total int64
	for _, c := range modelCosts {
		total += c
	}
	return CostSnapshot{
		TotalCostMicroUSD: total,
		ModelCosts:        modelCosts,
		ModelErrors:       modelErrors,
	}
}
