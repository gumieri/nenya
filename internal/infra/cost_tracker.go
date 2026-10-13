package infra

import (
	"math"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/nenya/internal/util"
)

// CostTracker accumulates per-model upstream spend (in micro-USD) and error
// counts for the cost guard rails and observability consumers. All methods
// are nil-safe: a nil tracker records nothing and reads report zero values.
// Construct with NewCostTracker; the zero value is not usable.
type CostTracker struct {
	mu     sync.RWMutex
	costs  map[string]*atomic.Int64
	errors map[string]*atomic.Int64
}

// NewCostTracker returns a ready-to-use tracker with empty per-model maps.
func NewCostTracker() *CostTracker {
	return &CostTracker{
		costs:  make(map[string]*atomic.Int64),
		errors: make(map[string]*atomic.Int64),
	}
}

// RecordUsage adds costUSD to the per-model spend counter (tracked in
// micro-USD, rounding half-away-from-zero with a sign-preserving 1 micro-USD
// floor for non-zero costs that round to zero). The model name is cloned
// before retention: entries live for the process lifetime and the caller's
// string may alias a request buffer (see cloneLabels in metrics.go,
// NENYA-198). Nil-safe.
func (ct *CostTracker) RecordUsage(model string, costUSD float64) {
	if ct == nil {
		return
	}
	microUSD, ok := util.SafeMicroUSD(costUSD)
	if !ok {
		return
	}
	if microUSD == 0 && costUSD != 0 {
		microUSD = int64(math.Copysign(1, costUSD))
	}
	ct.mu.RLock()
	c, ok := ct.costs[model]
	ct.mu.RUnlock()
	if !ok {
		ct.mu.Lock()
		c, ok = ct.costs[model]
		if !ok {
			c = new(atomic.Int64)
			// Clone on insert only (see cloneLabels in metrics.go, NENYA-198).
			ct.costs[strings.Clone(model)] = c
		}
		ct.mu.Unlock()
	}
	c.Add(microUSD)
}

// RecordError increments the per-model error counter. The model name is
// cloned before retention (NENYA-198). Nil-safe.
func (ct *CostTracker) RecordError(model string) {
	if ct == nil {
		return
	}
	ct.mu.RLock()
	c, ok := ct.errors[model]
	ct.mu.RUnlock()
	if !ok {
		ct.mu.Lock()
		c, ok = ct.errors[model]
		if !ok {
			c = new(atomic.Int64)
			// Clone on insert only (NENYA-198).
			ct.errors[strings.Clone(model)] = c
		}
		ct.mu.Unlock()
	}
	c.Add(1)
}

// GetCostMicroUSD returns the accumulated micro-USD spend for model, or 0
// when unobserved. Nil-safe.
func (ct *CostTracker) GetCostMicroUSD(model string) int64 {
	if ct == nil {
		return 0
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if c, ok := ct.costs[model]; ok {
		return c.Load()
	}
	return 0
}

// GetCostUSD returns the accumulated spend for model in USD. Nil-safe.
func (ct *CostTracker) GetCostUSD(model string) float64 {
	return float64(ct.GetCostMicroUSD(model)) / 1e6
}

// GetErrorCount returns the recorded error count for model, or 0 when
// unobserved. Nil-safe.
func (ct *CostTracker) GetErrorCount(model string) int64 {
	if ct == nil {
		return 0
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	if c, ok := ct.errors[model]; ok {
		return c.Load()
	}
	return 0
}

// GetAllCostsMicroUSD returns a copy of the per-model micro-USD spend map.
// Nil-safe.
func (ct *CostTracker) GetAllCostsMicroUSD() map[string]int64 {
	if ct == nil {
		return map[string]int64{}
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	result := make(map[string]int64, len(ct.costs))
	for model, c := range ct.costs {
		result[model] = c.Load()
	}
	return result
}

// GetAllErrors returns a copy of the per-model error-count map. Nil-safe.
func (ct *CostTracker) GetAllErrors() map[string]int64 {
	if ct == nil {
		return map[string]int64{}
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	result := make(map[string]int64, len(ct.errors))
	for model, c := range ct.errors {
		result[model] = c.Load()
	}
	return result
}

// CostSnapshot is a point-in-time view of CostTracker state.
type CostSnapshot struct {
	TotalCostMicroUSD int64            `json:"total_cost_micro_usd"`
	ModelCosts        map[string]int64 `json:"model_costs_micro_usd"`
	ModelErrors       map[string]int64 `json:"model_errors"`
}

// Snapshot returns a snapshot of all per-model costs and errors taken under a
// single read lock (so the cost and error views are mutually consistent),
// with the total summed from the per-model costs. Nil-safe.
func (ct *CostTracker) Snapshot() CostSnapshot {
	if ct == nil {
		return CostSnapshot{
			ModelCosts:  map[string]int64{},
			ModelErrors: map[string]int64{},
		}
	}
	ct.mu.RLock()
	defer ct.mu.RUnlock()
	modelCosts := make(map[string]int64, len(ct.costs))
	for model, c := range ct.costs {
		modelCosts[model] = c.Load()
	}
	modelErrors := make(map[string]int64, len(ct.errors))
	for model, c := range ct.errors {
		modelErrors[model] = c.Load()
	}
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
