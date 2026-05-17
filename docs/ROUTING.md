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