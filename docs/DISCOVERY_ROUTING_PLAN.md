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
- ✅ Created `internal/discovery/types.go` — `ModelMetadata` struct with capability flags (`SupportsVision`, `SupportsToolCalls`, `SupportsReasoning`, `SupportsContentArrays`, `SupportsStreamOptions`, `SupportsAutoToolChoice`)
- ✅ Added `Metadata *ModelMetadata` field to `DiscoveredModel` in `discovery.go`
- ✅ Added `HasMetadata()` cached flag to `ModelCatalog` (avoids full-scan on every sort)
- ✅ `ScoreBonus` field on `ModelMetadata` — wired into balanced scoring engine
- ✅ `detectRequestCapabilities()` in `proxy/chat.go` — inspects payload for tool_calls, vision, reasoning, content arrays
- ✅ `capabilityBoost()` in `routing/sort.go` — boosts/penalizes targets based on request vs model capabilities
- ⬜ Parsers (`parse.go`) do not yet extract capability hints from provider responses
- ⬜ `ModelEntry` in `config/entry.go` not yet extended with metadata/pricing fields
- ⬜ `MergeCatalog` drops metadata on override/static paths — needs propagation
- ⬜ `required_capabilities` filter on `AgentModel` not yet implemented
- ⬜ `/v1/models` response does not yet include metadata

**2. Cost Optimization**
- ✅ Created `internal/discovery/pricing.go` — `PricingEntry`, `PricingFetcher` (OpenRouter), `MergePricing`, `CalculateCost`
- ✅ Fixed OpenRouter pricing JSON parsing (nested struct for `pricing.prompt`/`pricing.completion`)
- ✅ Response body limited to 10MB via `LimitReader`
- ✅ `fmt.Sscanf` errors checked — skips models with unparseable pricing
- ✅ Created `internal/infra/cost_tracker.go` — per-model atomic counters (microUSD internal storage), `RecordUsage(costUSD)`, `GetCostMicroUSD()`, `GetCostUSD()`, `Snapshot()`, `RecordError()`
- ✅ `CostTracker` wired into `NenyaGateway` and passed to `SortTargetsByBalanced`
- ✅ Created `internal/routing/sort.go` — `SortTargetsByBalanced` with weighted scoring (latency + cost + bonus + capability boost)
- ✅ Config fields added: `GovernanceConfig.RoutingStrategy`, `RoutingLatencyWeight`, `RoutingCostWeight`
- ✅ Unit tests: latency-only, cost-only, score bonus, capability boost, mismatch penalty, zero-weights no-op, single/empty targets, `TestCapabilityBoost`
- ⬜ `PricingFetcher.FetchOpenRouterPricing` not yet called at startup or on reload
- ⬜ `MergePricing` defined but not wired into discovery pipeline
- ⬜ `RecordUsage` not yet called from proxy request completion (cost not tracked per-request)
- ⬜ `PricingSource` interface not implemented (only concrete `PricingFetcher`)
- ⬜ Static pricing entries in `config/registry.go` not yet added
- ⬜ `budget_limit` / `max_cost_per_request` not yet implemented

---

## Phase 3 Detailed Plan

### 3.1 Model Metadata Enhancement

**Goal**: Enrich `DiscoveredModel` with capability flags and support provider-specific metadata.

#### Week 1, Day 1-2: Core Infrastructure

| File | Changes | Status |
|------|---------|--------|
| `internal/discovery/types.go` | Add `ModelMetadata` struct with capability flags | ✅ Done |
| `internal/discovery/discovery.go` | Add `Metadata` field to `DiscoveredModel`, `HasMetadata()` cached flag | ✅ Done |
| `internal/config/entry.go` | Extend `ModelEntry` with optional metadata fields | ⬜ Not started |
| `internal/discovery/parse.go` | Update parsers to extract capability hints from provider responses | ⬜ Not started |

**Key Decisions**:
- Metadata is **opt-in** — models without capability info default to `false`
- Capability detection: OpenAI's `endpoints` array, Anthropic's `capabilities`, Gemini's `supported_generation_methods`
- Static registry entries can add explicit metadata overrides

#### Week 1, Day 3: API Exposure

| File | Changes | Status |
|------|---------|--------|
| `internal/gateway/models.go` | Include metadata in `/v1/models` response | ⬜ Not started |
| `internal/discovery/merge.go` | Propagate metadata through override/static merge paths | ⬜ Not started |

#### Week 1, Day 4-5: Capability-Based Routing

| File | Changes | Status |
|------|---------|--------|
| `internal/proxy/chat.go` | `detectRequestCapabilities()` — inspect payload for tools/vision/reasoning | ✅ Done |
| `internal/routing/sort.go` | `capabilityBoost()` — boost/penalize targets by capability match | ✅ Done |
| `internal/routing/targets.go` | Add `SupportsCapability(cap string)` filter for agent model lists | ⬜ Not started |
| `internal/config/types.go` | Add `required_capabilities` field to `AgentModel` | ⬜ Not started |

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

| File | Changes | Status |
|------|---------|--------|
| `internal/discovery/pricing.go` | `PricingEntry`, `PricingFetcher` (OpenRouter), `MergePricing`, `CalculateCost` | ✅ Done |
| `internal/discovery/pricing.go` | Nested JSON struct for OpenRouter pricing fields | ✅ Done |
| `internal/discovery/pricing.go` | 10MB body limit, `fmt.Sscanf` error handling | ✅ Done |
| `internal/discovery/types.go` | Add `PricingEntry` (InputPer1M, OutputPer1M, Currency=USD) | ✅ Done |
| `internal/config/entry.go` | Add `PricingEntry` to `ModelEntry` for static overrides | ⬜ Not started |
| `internal/config/registry.go` | Add pricing to static registry entries | ⬜ Not started |
| `internal/discovery/pricing.go` | Implement `PricingSource` interface with Static + Fallback variants | ⬜ Not started |

**Pricing Data Sources (Priority Order)**:
1. **OpenRouter API** — includes `price`, `price_input`, `price_output` per model
2. **Anthropic** — static (API doesn't expose pricing)
3. **Static Config Override** — explicit pricing entries in `model_registry`
4. **Fallback** — returns high cost estimate, forces cheapest selection

**Implementation** (current state):
```go
// Done:
type PricingEntry struct { ... }           // InputCostPer1M, OutputCostPer1M, Currency
type PricingFetcher struct { ... }         // Fetches from OpenRouter /v1/models
func (pf *PricingFetcher) FetchOpenRouterPricing(ctx) (map[string]PricingEntry, error)
func MergePricing(discovered, static map[string]PricingEntry) map[string]PricingEntry
func (p PricingEntry) CalculateCost(inputTokens, outputTokens int64) float64  // returns USD

// Not yet:
type PricingSource interface { GetPricing(modelID string) (PricingEntry, bool) }
type StaticPricing struct { ... }           // reads from config
type FallbackPricing struct { ... }         // returns high cost estimate
```

#### Week 2, Day 3-4: Cost Tracking

| File | Changes | Status |
|------|---------|--------|
| `internal/infra/cost_tracker.go` | Per-model cost tracking with atomic counters (microUSD internal) | ✅ Done |
| `internal/infra/cost_tracker.go` | `RecordUsage(costUSD)`, `GetCostMicroUSD()`, `GetCostUSD()`, `Snapshot()` | ✅ Done |
| `internal/infra/cost_tracker.go` | `RecordError()`, `GetErrorCount()`, `GetAllErrors()` | ✅ Done |
| `internal/gateway/gateway.go` | `CostTracker` field added, instantiated in `New()` | ✅ Done |
| `internal/proxy/chat.go` | `CostTracker` passed to `SortTargetsByBalanced` | ✅ Done |
| `internal/proxy/chat.go` | Calculate cost at request completion using pricing data | ⬜ Not started |
| `internal/gateway/gateway.go` | Call `FetchOpenRouterPricing` at startup, merge with static | ⬜ Not started |

**Cost Formula** (USD only):
```
cost = (input_tokens / 1_000_000) * input_per_1m + (output_tokens / 1_000_000) * output_per_1m
```

#### Week 2, Day 5: Cost-Aware Routing

| File | Changes | Status |
|------|---------|--------|
| `internal/routing/sort.go` | `SortTargetsByBalanced` with latency/cost/bonus/capability scoring | ✅ Done |
| `internal/routing/sort.go` | `collectMinMax` closure for normalization bounds | ✅ Done |
| `internal/proxy/chat.go` | Wire `routing_strategy: "balanced"` into target selection | ✅ Done |
| `internal/config/types.go` | `RoutingStrategy`, `RoutingLatencyWeight`, `RoutingCostWeight` fields | ✅ Done |
| `internal/routing/targets.go` | Add `budget_limit` field to agent config | ⬜ Not started |

---

### 3.3 Scoring Engine ✅ DONE

**Routing Strategies** (`governance.routing_strategy`):
- `"latency"` — fastest model only (existing behavior, default)
- `"balanced"` — composite score (latency + cost + bonus + capability boost) ✅ Implemented

**Balanced Score Formula** (implemented in `routing/sort.go:calculateScore`):
```
final_score = (latency_normalized * latency_weight)
            - (cost_normalized * cost_weight)
            + model.score_bonus
            + capability_boost(model_metadata, request_capabilities)
```

Where:
- `latency_normalized` = `(maxLat - model_latency) / (maxLat - minLat)` (0.0-1.0, higher = faster)
- `cost_normalized` = `(model_cost - minCost) / (maxCost - minCost)` (0.0-1.0, lower = cheaper)
- `score_bonus` = per-model override from `ModelMetadata.ScoreBonus`
- `capability_boost` = +0.1 per matching capability (tool_calls, reasoning, vision, content_arrays), -0.1 per mismatched capability
- Higher score = better (faster + cheaper + more capable)
- Ties broken by raw latency

**Config** (actual field names in `GovernanceConfig`):
```json
{
  "governance": {
    "auto_reorder_by_latency": true,
    "routing_strategy": "balanced",
    "routing_latency_weight": 1.0,
    "routing_cost_weight": 0.3
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

| File | Action | Purpose | Status |
|------|--------|---------|--------|
| `internal/discovery/types.go` | Created | `ModelMetadata` with capability flags + `ScoreBonus` + `ParsedFrom` | ✅ |
| `internal/discovery/pricing.go` | Created | `PricingEntry`, `PricingFetcher` (OpenRouter), `MergePricing`, `CalculateCost` | ✅ |
| `internal/infra/cost_tracker.go` | Created | Per-model cost tracking (microUSD), `RecordUsage(costUSD)`, `Snapshot` | ✅ |
| `internal/routing/sort.go` | Created | `SortTargetsByBalanced`, `calculateScore`, `capabilityBoost`, `SortOptions` | ✅ |
| `internal/routing/sort_test.go` | Created | 9 test cases + `TestCapabilityBoost` table test | ✅ |
| `internal/proxy/chat.go` | Updated | `detectRequestCapabilities()`, wired `CostTracker` + balanced sort | ✅ |
| `internal/config/types.go` | Updated | `RoutingStrategy`, `RoutingLatencyWeight`, `RoutingCostWeight` | ✅ |
| `internal/gateway/gateway.go` | Updated | `CostTracker` field, instantiated in `New()` | ✅ |
| `internal/discovery/discovery.go` | Updated | `Metadata` field on `DiscoveredModel`, `HasMetadata()` cached flag | ✅ |
| `internal/discovery/parse.go` | Update | Extract capabilities from provider responses | ⬜ |
| `internal/discovery/metadata.go` | Create | Merge discovered + config metadata, capability matching | ⬜ |
| `internal/config/entry.go` | Update | `ScoreBonus`, `Capabilities`, `Pricing` on `ModelEntry` | ⬜ |
| `internal/config/registry.go` | Update | Pricing + capabilities on static entries | ⬜ |
| `internal/routing/targets.go` | Update | `required_capabilities` filter, `budget_limit` | ⬜ |
| `internal/gateway/models.go` | Update | Include metadata + pricing in `/v1/models` response | ⬜ |
| `internal/proxy/chat.go` | Update | Call `RecordUsage` at request completion | ⬜ |
| `internal/gateway/gateway.go` | Update | Call `FetchOpenRouterPricing` at startup, merge with static | ⬜ |
| `docs/CONFIGURATION.md` | Update | New config options documentation | ⬜ |

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

⬜ Parsers extract capability hints from provider responses
⬜ `ModelEntry` extended with metadata/pricing/score_bonus fields
⬜ `MergeCatalog` propagates metadata through override/static paths
⬜ `required_capabilities` filter on `AgentModel`
⬜ `/v1/models` includes metadata + pricing
⬜ `PricingSource` interface (Static + Fallback variants)
⬜ `FetchOpenRouterPricing` called at startup, merged with static pricing
⬜ `RecordUsage` called from proxy at request completion (per-request cost tracking)
⬜ `budget_limit` / `max_cost_per_request` enforcement
⬜ All config options documented
✅ `ModelMetadata` struct with capability flags + `ScoreBonus`
✅ `PricingEntry` + `PricingFetcher` (OpenRouter) + `CalculateCost`
✅ `CostTracker` with per-model atomic counters (microUSD)
✅ `SortTargetsByBalanced` scoring engine (latency + cost + bonus + capability)
✅ `detectRequestCapabilities` inspects payload for tools/vision/reasoning
✅ `capabilityBoost` boosts/penalizes by capability match
✅ `HasMetadata()` cached flag on `ModelCatalog`
✅ Config fields: `routing_strategy`, `routing_latency_weight`, `routing_cost_weight`
✅ `CostTracker` wired into gateway and balanced sort
✅ Unit tests for scoring, capabilities, cost sorting
✅ No breaking changes confirmed

### Quality Gates

- **Code Coverage**: Minimum 90% for new code
- **Performance**: < 5% increase in routing latency
- **Memory**: < 2MB additional heap usage
- **Testing**: At least 3 integration test scenarios
- **Documentation**: All new config options documented
