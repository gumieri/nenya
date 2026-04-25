# Model Discovery-Based Routing Enhancement Plan

## Implementation Status Overview

### Phase 1 (COMPLETED ✅)

**1. Deep Provider Health Validation**
- ✅ Created `internal/discovery/health.go`
- ✅ Implemented status detection (unreachable/empty/invalid/ok)
- ✅ Integrated with startup and reload processes
- ✅ Health exposed via `/statsz` endpoint
- ✅ Structured logging for provider health

**2. Auto-Generated Agents**
- ✅ Created `internal/discovery/agents.go`
- ✅ Implemented filter functions for all categories
- ✅ Integrated with `gateway.New()`
- ✅ Auto-agents appear in `/v1/models` endpoint
- ✅ User override mechanism working
- ✅ Logging for auto-generation process

### Phase 2 (COMPLETED ✅)

**1. Registry Dynamic Sync**
- ✅ Created `internal/discovery/sync.go`
- ✅ Cache structures and persistence with TTL-based expiration
- ✅ Config extension (`sync_mode`, `sync_ttl_days`, `cache_path`)
- ✅ Load cached models on startup failure
- ✅ Graceful fallback to static registry

**2. Latency-Based Reordering**
- ✅ Created `internal/infra/latency.go`
- ✅ Thread-safe latency tracking with median calculation
- ✅ Per-model sorted sample buffers with incremental binary-search insertion
- ✅ `SortTargetsByLatency` in `internal/routing/targets.go`
- ✅ `auto_reorder_by_latency` config flag
- ✅ ±5% jitter to prevent thundering herd

**3. Advanced Health Checks**
- ✅ Created `internal/discovery/health_advanced.go`
- ✅ Config drift detection and logging
- ✅ Severity levels and startup validation warnings
- ✅ Exposed via `/statsz` endpoint

**4. Context-Aware Filtering**
- ✅ Found implementation at `internal/routing/targets.go:99`
- ✅ Active logic with `autoContextSkip` config
- ✅ Logging for skipped models
- ✅ Working in production

### Phase 3 (IN PROGRESS 🚀)

**1. Model Metadata Enhancement**
Status: 🚧 PLANNED — see detailed plan below

**2. Cost Optimization**
Status: 🚧 PLANNED — see detailed plan below

---

## Phase 3 Detailed Plan

### 3.1 Model Metadata Enhancement

**Goal**: Enrich `DiscoveredModel` with capability flags and support provider-specific metadata.

#### Week 1, Day 1-2: Core Infrastructure

| File | Changes |
|------|---------|
| `internal/discovery/types.go` | Add `ModelMetadata` struct with capability flags (`SupportsVision`, `SupportsToolCalls`, `SupportsReasoning`, `SupportsContentArrays`, `SupportsStreamOptions`) |
| `internal/config/types.go` | Extend `ModelEntry` with optional metadata fields |
| `internal/discovery/parse.go` | Update parsers to extract capability hints from provider responses |

**Key Decisions**:
- Metadata is **opt-in** — models without capability info default to `false`
- Capability detection: OpenAI's `endpoints` array, Anthropic's `capabilities`, Gemini's `supported_generation_methods`
- Static registry entries can add explicit metadata overrides

#### Week 1, Day 3: API Exposure

| File | Changes |
|------|---------|
| `internal/gateway/models.go` | Include metadata in `/v1/models` response |
| `internal/discovery/discovery.go` | Merge discovered metadata with static registry |

#### Week 1, Day 4-5: Capability-Based Routing

| File | Changes |
|------|---------|
| `internal/routing/targets.go` | Add `SupportsCapability(cap string)` filter for agent model lists |
| `internal/config/types.go` | Add `required_capabilities` field to `AgentModel` |

**Example Config**:
```json
{
  "agents": {
    "coding": {
      "models": [
        {
          "provider": "anthropic",
          "model": "claude-3.5-sonnet-20241022",
          "required_capabilities": ["vision"]
        }
      ]
    }
  }
}
```

---

### 3.2 Cost Optimization

**Goal**: Track costs, fetch pricing from providers, and enable cost-aware routing decisions.

#### Week 2, Day 1-2: Pricing Data Layer

| File | Changes |
|------|---------|
| `internal/discovery/pricing.go` | Create: `PricingSource` interface, `OpenRouterPricing`, `StaticPricing`, `FallbackPricing` |
| `internal/discovery/types.go` | Add `PricingEntry` (InputPer1M, OutputPer1M, Currency=USD) |
| `internal/config/types.go` | Add `PricingEntry` to `ModelEntry` for static overrides |

**Pricing Data Sources (Priority Order)**:
1. **OpenRouter API** — includes `price`, `price_input`, `price_output` per model
2. **Anthropic** — static (API doesn't expose pricing)
3. **Static Config Override** — explicit pricing entries in `model_registry`
4. **Fallback** — returns high cost estimate, forces cheapest selection

**Implementation**:
```go
type PricingSource interface {
    GetPricing(modelID string) (PricingEntry, bool)
}

type OpenRouterPricing struct { ... }   // fetches from /v1/models
type StaticPricing struct { ... }       // reads from config
type FallbackPricing struct { ... }     // returns high cost estimate
```

#### Week 2, Day 3-4: Cost Tracking

| File | Changes |
|------|---------|
| `internal/infra/cost_tracker.go` | Create: per-model cost tracking with atomic counters (same pattern as `UsageTracker`) |
| `internal/proxy/handler.go` | Calculate cost at request completion using pricing data |

**Cost Formula** (USD only):
```
cost = (input_tokens / 1_000_000) * input_per_1m + (output_tokens / 1_000_000) * output_per_1m
```

#### Week 2, Day 5: Cost-Aware Routing

| File | Changes |
|------|---------|
| `internal/routing/sort.go` | Create: `ScoreTargets()` with latency/cost/balanced/capability logic |
| `internal/routing/targets.go` | Integrate scoring into `BuildTargetList` |

---

### 3.3 Scoring Engine

**Routing Strategies** (`governance.routing_strategy`):
- `"latency"` — fastest model only (existing behavior)
- `"cost"` — cheapest model only
- `"balanced"` — composite score (default: latency)

**Balanced Score Formula**:
```
final_score = (latency_normalized * latency_weight)
            - (cost_weight * cost_normalized)
            + model.score_bonus
```

Where:
- `latency_normalized` = `model_latency / max_latency_across_targets` (0.0-1.0)
- `cost_normalized` = `model_cost / max_cost_across_targets` (0.0-1.0)
- `score_bonus` = per-model override from config (can be negative)
- Higher score = better (faster + cheaper)
- Ties broken by capability matching, then latency

**Config**:
```json
{
  "governance": {
    "routing_strategy": "balanced",
    "balanced": {
      "cost_weight": 0.3,
      "latency_jitter_pct": 5
    }
  }
}
```

**Per-Model Score Override**:
```json
{
  "model_registry": {
    "claude-3.5-sonnet-20241022": {
      "score_bonus": 0.1,
      "capabilities": ["vision", "tool_calls", "reasoning"],
      "pricing": {
        "input_per_1m": 3.0,
        "output_per_1m": 15.0
      }
    },
    "gemini-2.5-flash": {
      "score_bonus": 0.2,
      "capabilities": ["vision", "tool_calls"],
      "pricing": {
        "input_per_1m": 0.15,
        "output_per_1m": 0.60
      }
    }
  }
}
```

---

### 3.4 Full File Inventory

| File | Action | Purpose |
|------|--------|---------|
| `internal/discovery/types.go` | Update | Add `ModelMetadata`, `PricingEntry` |
| `internal/discovery/parse.go` | Update | Extract capabilities from provider responses |
| `internal/discovery/metadata.go` | Create | Merge discovered + config metadata, capability matching |
| `internal/discovery/pricing.go` | Create | `PricingSource` interface, OpenRouter fetcher, static fallback |
| `internal/config/types.go` | Update | `score_bonus`, `capabilities`, `pricing` on `ModelEntry`; `RoutingStrategyConfig` |
| `internal/config/registry.go` | Update | Pricing + capabilities on static entries |
| `internal/infra/cost_tracker.go` | Create | Per-model cost tracking, atomic counters |
| `internal/routing/sort.go` | Create | `ScoreTargets()` with latency/cost/balanced/capability logic |
| `internal/routing/targets.go` | Update | Integrate scoring into `BuildTargetList` |
| `internal/gateway/models.go` | Update | Include metadata + pricing in `/v1/models` response |
| `docs/CONFIG.md` | Update | New config options documentation |

---

### 3.5 Implementation Timeline

| Week | Focus | Deliverables |
|------|-------|-------------|
| Week 1 | Types + Metadata | `ModelMetadata`, capability parsing, API exposure, capability filtering |
| Week 2 | Pricing + Scoring | `PricingSource`, cost tracker, `ScoreTargets()`, balanced routing |
| Week 3 | Integration + Polish | `/v1/models` enrichment, config docs, tests, validation |

---

### 3.6 Risk Assessment

| Risk | Mitigation |
|------|------------|
| Provider API changes break metadata parsing | Graceful fallback to defaults; log warnings |
| Pricing data stale | TTL-based expiration; manual override in config |
| Cost calculation errors | Per-request logging for debugging |
| Score gaming via `score_bonus` | Document that extreme values can produce suboptimal routing |

---

### 3.7 Dependencies

- Phase 2 sync infrastructure (`internal/discovery/sync.go`) — required for caching pricing
- Latency tracking (`internal/infra/latency.go`) — base for balanced scoring
- ModelCatalog (`internal/discovery/discovery.go`) — merge point for metadata
- UsageTracker (`internal/infra/usage_tracker.go`) — token counts feed cost calculation

---

## Risk Assessment

### Prevention Strategies

**1. Zero Breaking Changes**
- All Phase 2 and 3 features are opt-in via config flags
- Default behavior unchanged from Phase 1
- Users can ignore new features if needed

**2. Graceful Degradation**
- Discovery failures fall back to static registry
- Cache expiration prevents stale data issues
- Validation errors don't interrupt routing
- Missing pricing defaults to high-cost estimate

**3. Comprehensive Testing**
- Unit tests for all new components
- Integration tests with mixed modes
- Manual testing of scoring behavior

**4. Performance Monitoring**
- Benchmark latency tracking overhead
- Measure sorting performance impact
- Validate cache I/O efficiency
- Score calculation is O(n) per target list

---

## Success Criteria

### Phase 2 Completion Checklist

✅ All 4 Phase 2 features implemented
✅ Unit tests passing
✅ Integration tests successful
✅ Documentation updated
✅ Feature flags working correctly
✅ No breaking changes confirmed

### Phase 3 Completion Checklist

⬜ Model metadata with capability flags
⬜ Capability-based filtering in routing
⬜ Pricing fetch from OpenRouter + static fallback
⬜ Per-model cost tracking
⬜ Balanced scoring engine (latency + cost + bonus)
⬜ Per-model `score_bonus` override
⬜ `/v1/models` includes metadata + pricing
⬜ All config options documented
⬜ Unit tests for pricing, scoring, capabilities
⬜ No breaking changes confirmed

### Quality Gates

- **Code Coverage**: Minimum 90% for new code
- **Performance**: < 5% increase in routing latency
- **Memory**: < 2MB additional heap usage
- **Testing**: At least 3 integration test scenarios
- **Documentation**: All new config options documented
