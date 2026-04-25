# Phase 3 Implementation Plan

## Current Status

**Infrastructure scaffolding is complete.** Types, trackers, scoring engine, and config fields are implemented and tested. Remaining work is **integration/wiring**: connecting the pricing fetcher at startup, recording costs per-request, extending config schema, and populating metadata from provider responses.

---

## 3.1 Model Metadata Enhancement

### Goal
Enrich `DiscoveredModel` with capability flags and support provider-specific metadata.

### Implementation Timeline

#### Week 1: Core Infrastructure
| File | Changes | Status |
|------|---------|--------|
| `internal/discovery/types.go` | Add `ModelMetadata` struct with capability flags (`SupportsVision`, `SupportsToolCalls`, `SupportsReasoning`, `SupportsContentArrays`, `SupportsStreamOptions`) | ✅ Done |
| `internal/discovery/discovery.go` | Add `Metadata *ModelMetadata` field to `DiscoveredModel`; add `HasMetadata()` cached flag to `ModelCatalog` | ✅ Done |
| `internal/config/entry.go` | Extend `ModelEntry` with optional metadata fields (`ScoreBonus`, `Capabilities`, `Pricing`) | ⬜ Not started |
| `internal/discovery/parse.go` | Update parsers to extract capability hints from provider responses | ⬜ Not started |

### Key Decisions
1. Metadata is **opt-in** — models without capability info default to `false`
2. Capability detection: OpenAI's `endpoints` array, Anthropic's `capabilities`, Gemini's `supported_generation_methods`, etc.
3. Static registry entries can add explicit metadata overrides

#### Week 2: API Exposure and Capability-Based Routing
| File | Changes | Status |
|------|---------|--------|
| `internal/proxy/chat.go` | `detectRequestCapabilities()` — inspects payload for tools, vision, reasoning, content arrays | ✅ Done |
| `internal/routing/sort.go` | `capabilityBoost()` — boosts/penalizes targets based on request vs model capabilities | ✅ Done |
| `internal/gateway/models.go` | Include metadata in `/v1/models` response | ⬜ Not started |
| `internal/discovery/merge.go` | Propagate metadata through override/static merge paths (currently dropped) | ⬜ Not started |
| `internal/routing/targets.go` | Add `SupportsCapability(cap string)` filter for agent model lists | ⬜ Not started |
| `internal/config/types.go` | Add `required_capabilities` field to `AgentModel` | ⬜ Not started |

### Example Config Enhancement
```json
{
  "agents": {
    "coding": {
      "models": [
        {"provider": "anthropic", "model": "claude-3.5-sonnet-20241022", "required_capabilities": ["vision"]}
      ]
    }
  }
}
```

---

## 3.2 Cost Optimization

### Goal
Track costs and enable cost-aware routing decisions.

### Implementation Timeline

#### Week 1: Cost Data Structures
| File | Changes | Status |
|------|---------|--------|
| `internal/discovery/pricing.go` | `PricingEntry` struct with `InputCostPer1M`, `OutputCostPer1M`, `Currency`; `CalculateCost()` | ✅ Done |
| `internal/discovery/pricing.go` | `PricingFetcher` — fetches from OpenRouter `/v1/models`, nested JSON struct, 10MB body limit, `fmt.Sscanf` error handling | ✅ Done |
| `internal/discovery/pricing.go` | `MergePricing(discovered, static)` — merges two pricing maps | ✅ Done |
| `internal/config/entry.go` | Add `PricingEntry` to `ModelEntry` for static overrides | ⬜ Not started |
| `internal/config/registry.go` | Add pricing to static registry (e.g., `"claude-3.5-sonnet-20241022": {..., "Pricing": {InputCostPer1M: 3.0, OutputCostPer1M: 15.0}}`) | ⬜ Not started |
| `internal/discovery/pricing.go` | Implement `PricingSource` interface with `StaticPricing` and `FallbackPricing` variants | ⬜ Not started |
| `internal/discovery/sync.go` | Extend cache to include pricing data when available | ⬜ Not started |

### Key Decisions
1. Pricing is **static by default** — pulled from provider docs at compile time
2. Optional: fetch pricing from provider APIs if available (OpenRouter has this)
3. Currency assumed USD only
4. Internal storage uses microUSD (int64 atomic counters) for precision; API accepts/returns USD (float64)

#### Week 2: Cost Tracking Infrastructure
| File | Changes | Status |
|------|---------|--------|
| `internal/infra/cost_tracker.go` | `CostTracker` with per-model atomic counters (microUSD), `RecordUsage(costUSD)`, `GetCostMicroUSD()`, `GetCostUSD()`, `Snapshot()`, `RecordError()` | ✅ Done |
| `internal/gateway/gateway.go` | `CostTracker` field added, instantiated in `New()` | ✅ Done |
| `internal/proxy/chat.go` | Calculate and record cost at request completion using pricing data | ⬜ Not started |
| `internal/gateway/gateway.go` | Call `FetchOpenRouterPricing` at startup, merge with static pricing | ⬜ Not started |

### Cost Calculation Formula
```
cost_usd = (input_tokens / 1_000_000) * input_per_1m + (output_tokens / 1_000_000) * output_per_1m
```
Implemented in `PricingEntry.CalculateCost()` (returns USD). Stored internally as microUSD via `CostTracker.RecordUsage(costUSD)`.

#### Week 3: Cost-Aware Routing
| File | Changes | Status |
|------|---------|--------|
| `internal/routing/sort.go` | `SortTargetsByBalanced` with latency/cost/bonus/capability scoring | ✅ Done |
| `internal/routing/sort.go` | `collectMinMax` closure for normalization bounds per-target | ✅ Done |
| `internal/proxy/chat.go` | Wire `routing_strategy: "balanced"` into target selection | ✅ Done |
| `internal/config/types.go` | `RoutingStrategy`, `RoutingLatencyWeight`, `RoutingCostWeight` fields | ✅ Done |
| `internal/routing/targets.go` | Add `budget_limit` field to agent config | ⬜ Not started |

### Example Config Enhancement
```json
{
  "governance": {
    "auto_reorder_by_latency": true,
    "routing_strategy": "latency",
    "routing_latency_weight": 1.0,
    "routing_cost_weight": 0.3
  },
  "agents": {
    "budget-agent": {
      "budget_limit_usd": 10.00,
      "models": ["gemini-2.5-flash"]
    }
  }
}
```

### New Config Options
```json
{
  "governance": {
    "routing_strategy": "latency|balanced",
    "routing_latency_weight": 1.0,
    "routing_cost_weight": 0.3,
    "max_cost_per_request": 0.50
  },
  "discovery": {
    "fetch_pricing": true
  }
}
```

---

## 3.3 Scoring Engine ✅ DONE

Implemented in `internal/routing/sort.go`.

**Routing Strategies** (`governance.routing_strategy`):
- `"latency"` — fastest model only (existing behavior, default when strategy is empty)
- `"balanced"` — composite score

**Balanced Score Formula**:
```
final_score = (latency_normalized * latency_weight)
            - (cost_normalized * cost_weight)
            + model.score_bonus
            + capability_boost(model_metadata, request_capabilities)
```

Where:
- `latency_normalized` = `(maxLat - model_latency) / (maxLat - minLat)` — higher = faster
- `cost_normalized` = `(model_cost - minCost) / (maxCost - minCost)` — lower = cheaper
- `score_bonus` = per-model override from `ModelMetadata.ScoreBonus`
- `capability_boost` = +0.1 per matching capability, -0.1 per mismatched capability
- Ties broken by raw latency

---

## 3.4 Dependencies

- Phase 2 sync infrastructure (`internal/discovery/sync.go`) — required for caching pricing
- Latency tracking (`internal/infra/latency.go`) — base for balanced scoring
- ModelCatalog (`internal/discovery/discovery.go`) — merge point for metadata
- UsageTracker (`internal/infra/usage_tracker.go`) — token counts feed cost calculation

---

## 3.5 Risk Assessment

| Risk | Mitigation |
|------|------------|
| Provider API changes break metadata parsing | Graceful fallback to defaults; log warnings |
| Pricing data stale | TTL-based expiration; manual override in config |
| Cost calculation errors | Per-request logging for debugging |
| Score gaming via `score_bonus` | Document that extreme values can produce suboptimal routing |
| `MergeCatalog` drops metadata on override/static paths | Must propagate `Metadata` field through all merge paths |
