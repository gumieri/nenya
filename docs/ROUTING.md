# Nenya Routing System

## Overview

Nenya's routing system dynamically selects the optimal upstream provider for each request based on multiple factors including latency, cost, and model capabilities. This document describes the balanced scoring algorithm used for target selection.

## Balanced Scoring Algorithm

The routing system uses a multi-dimensional scoring approach that considers:

1. **Latency Performance**: Historical median latency data from the LatencyTracker
2. **Cost Efficiency**: Pricing information from the CostTracker
3. **Model Capabilities**: Metadata about each model's supported features
4. **User-Defined Weights**: Configurable importance of latency vs cost in decision making

### Scoring Formula

The final score for each target is calculated as:

```
score = (latency_score * latency_weight) - (cost_score * cost_weight) + capability_boost + score_bonus
```

Where:
- `latency_score` and `cost_score` are normalized values between 0 and 1
- `latency_weight` and `cost_weight` are configurable per-agent weights
- `capability_boost` is a bonus/penalty based on model capabilities matching request requirements
- `score_bonus` is a static boost configured per model

### Normalization

Scores are normalized using min-max normalization:

- **Latency**: `normalized = (max_lat - current_lat) / (max_lat - min_lat)`
- **Cost**: `normalized = (current_cost - min_cost) / (max_cost - min_cost)`

This ensures all factors are weighted equally in the 0-1 range.

## Agent Routing Strategies

The per-agent `strategy` field controls how targets in an agent's model chain are ordered for dispatch:

- `round-robin` (default): Each request rotates to the next target, spreading traffic evenly. Used when providers do not meaningfully benefit from a stable primary.
- `fallback`: The first target is always tried first; the rest form an ordered failover tail. Best when the primary is preferred on cost or quality grounds and alternates are purely for resilience.
- `sticky`: Sessions (identified by agent + system prompt + first user message) are pinned to a provider/model so provider-side prefix-cache warmth is preserved across turns. See [Sticky Session Routing](ARCHITECTURE.md#sticky-session-routing-sessionrouter).

New sessions under `sticky` and `round-robin` still rotate via the agent request counter so concurrent sessions spread across providers. `fallback` always starts at index 0.

Sticky-specific knobs:

- `sticky_session_ttl_seconds` (default 3600, max 86400): idle timeout after which a pin expires and the session re-pins on its next request.

Pin lifecycle: on an existing pin, the pinned target is reordered to the front so it is always tried first. If the pin's target is cooling (rate-limited), billing-exhausted, or no longer context-compatible, the pin is promoted to the first active target (re-pinning is logged at `Info`). Metrics: `nenya_session_active` (gauge, non-expired pins), `nenya_session_pin_changes_total{reason="new|failover|expired"}`.

## Configuration

Agents can configure routing weights in their configuration:

```json
{
  "routing": {
    "latency_weight": 0.7,
    "cost_weight": 0.3
  }
}
```

## Capability Matching

Models are scored based on their `ModelMetadata` capabilities (inferred dynamically via `discovery.InferCapabilities()`):

- `CapToolCalls`: Model supports tool/function calling
- `CapReasoning`: Model optimized for reasoning tasks
- `CapVision`: Model supports image inputs
- `CapContentArrays`: Model supports complex content arrays
- `CapStreamOptions`: Model supports `stream_options.include_usage`
- `CapAutoToolChoice`: Model supports `tool_choice: "auto"`

Requests specify required capabilities, and models receive bonuses for matching capabilities or penalties for mismatches.

## Implementation Details

- **File**: `internal/routing/sort.go`
- **Function**: `SortTargetsByBalanced`
- **Tests**: `internal/routing/sort_test.go`

The sorting function handles edge cases like missing data, single targets, and ensures stable sorting.