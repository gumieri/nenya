# Phase 3 Implementation Plan

## 3.1 Model Metadata Enhancement

### Goal
Enrich `DiscoveredModel` with capability flags and support provider-specific metadata.

### Implementation Timeline

#### Week 1: Core Infrastructure
| File | Changes |
|------|---------|
| `internal/discovery/types.go` | Add `ModelMetadata` struct with capability flags (`SupportsVision`, `SupportsToolCalls`, `SupportsReasoning`, `SupportsContentArrays`, `SupportsStreamOptions`) |
| `internal/config/types.go` | Extend `ModelEntry` with optional metadata fields |
| `internal/discovery/parse.go` | Update parsers to extract capability hints from provider responses |

### Key Decisions
1. Metadata is **opt-in** — models without capability info default to `false`
2. Capability detection: OpenAI's `endpoints` array, Anthropic's `capabilities`, Gemini's `supported_generation_methods`, etc.
3. Static registry entries can add explicit metadata overrides

#### Week 2: API Exposure and Capability-Based Routing
| File | Changes |
|------|---------|
| `internal/gateway/models.go` | Include metadata in `/v1/models` response |
| `internal/discovery/discovery.go` | Merge discovered metadata with static registry |
| `internal/routing/targets.go` | Add `SupportsCapability(cap string)` filter for agent model lists |
| `internal/config/types.go` | Add `required_capabilities` field to `AgentModel` |

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

## 3.2 Cost Optimization

### Goal
Track costs and enable cost-aware routing decisions.

### Implementation Timeline

#### Week 1: Cost Data Structures
| File | Changes |
|------|---------|
| `internal/config/types.go` | Add `PricingEntry` struct with `InputCostPer1M`, `OutputCostPer1M`, `Currency` |
| `internal/config/registry.go` | Add pricing to static registry (e.g., `"claude-3.5-sonnet-20241022": {..., "Pricing": {InputCostPer1M: 3.0, OutputCostPer1M: 15.0}}`) |
| `internal/discovery/sync.go` | Extend cache to include pricing data when available |

### Key Decisions
1. Pricing is **static by default** — pulled from provider docs at compile time
2. Optional: fetch pricing from provider APIs if available (OpenRouter has this)
3. Currency assumed USD only

#### Week 2: Cost Tracking Infrastructure
| File | Changes |
|------|---------|
| `internal/infra/cost_tracker.go` | New file: `UsageTracker`-style tracker for costs, per-model atomic counters |
| `internal/proxy/handler.go` | Calculate and record cost at request completion |
| `internal/infra/latency.go` | Extend `StatsSnapshot` to include cost data |

### Cost Calculation Formula
```
cost = (input_tokens / 1_000_000) * input_cost_per_1m + (output_tokens / 1_000_000) * output_cost_per_1m
```

#### Week 3: Cost-Aware Routing
| File | Changes |
|------|---------|
| `internal/routing/targets.go` | Add `budget_limit` field to agent config |
| `internal/routing/sort.go` | Add cost-based sorting mode alongside latency mode |

### Example Config Enhancement
```json
{
  "governance": {
    "routing_strategy": "latency",  // or "cost", "balanced"
    "max_cost_per_request": 0.50
  },
  "agents": {
    "budget-agent": {
      "budget_limit_usd": 10.00,
      "models": ["gemini-2.5-flash"]  // Cheaper options
    }
  }
}
```

### New Config Options
```json
{
  "governance": {
    "routing_strategy": "latency|cost|balanced",
    "max_cost_per_request": 0.50
  },
  "discovery": {
    "fetch_pricing": true
  }
}
```

### Dependencies
- Phase 2 sync infrastructure (`internal/discovery/sync.go`) — required for caching pricing
- Latency tracking (`internal/infra/latency.go`) — base for cost tracker
- ModelCatalog (`internal/discovery/discovery.go`) — merge point for metadata

### Risk Assessment

| Risk | Mitigation |
|------|------------|
| Provider API changes break metadata parsing | Graceful fallback to defaults; log warnings |
| Pricing data stale | TTL-based expiration; manual override in config |
| Cost calculation errors | Per-request logging for debugging |