# Billing-Aware Routing

Nenya's billing-aware routing lets you track spend, prevent quota exhaustion, and optimize traffic for providers with free/credit tiers.

## Overview

The system integrates three layers:

1. **Config** — Declare provider billing models (subscription, credit, free, mixed) and quotas
2. **Runtime** — Track spend, detect exhaustion, filter exhausted providers, apply scoring bonuses
3. **Observability** — Per-account spend, exhaustion flags, Prometheus metrics, /statsz endpoint

This enables scenarios like:
- Using only free models from a provider when credits run out
- Prioritizing free models on mixed providers (via scoring bonus)
- Preventing failed requests by auto-filtering exhausted accounts
- Budget enforcement per agent (`agent.budget_limit_usd`)

## Concepts

### Billing Models

| Model | Description | Scoring Effect | Filtering |
|-------|-------------|----------------|-----------|
| `subscription` | Monthly subscription, no quota tracking | Uses configured cost weight | No exhaustion filtering |
| `credit` | Prepaid credits, quota tracked | Uses configured cost weight | Exhausted accounts filtered |
| `free` | Free tier provider | Forced economy mode (×1.5 cost weight) | Paid models filtered out (if `free_only: true`) |
| `mixed` | Both free and paid models | Free models get +0.4 scoring bonus | No hard filtering |

### Free Model Detection

For `free_only` and `mixed` providers, Nenya determines if a model is free using a **three-tier priority**:

1. **Explicit config list** — `billing.free_models: ["model-a", "model-b"]`
2. **Name heuristic** — Model ID contains `-free`, `:free`, or `/free` suffix
3. **Catalog pricing** — `InputCostPer1M` and `OutputCostPer1M` ≤ $0.0001

The **first tier that matches determines the result**. This design lets providers declare explicit free models while falling back to naming conventions and pricing data for the rest.

**Important:** For `isModelFreeInProvider` (scoring), the priority is **config list → name suffix → pricing** (optimistic). For `isPaidModelOnFreeOnlyProvider` (filtering), the priority is **config list → pricing → name suffix** (conservative). The difference is intentional — false positives on a +0.4 scoring bonus are harmless, but routing to a paid model on a `free_only` provider wastes money.

### Quota Sources

| Source | Description | Update Frequency |
|--------|-------------|------------------|
| `none` | No quota tracking (uses `included_usd` only) | Manual reset via `BillingTracker.ResetSpend()` |
| `api` | Fetch quota from provider's quota URL | Polls at `quota_interval` (default 1h) |
| `headers` | Extract quota from response headers | After every successful response |

### Exhaustion

An account is marked **exhausted** when:
- Quota fetch returns `balance ≤ 0`
- Response headers indicate exhaustion (via `quota_extraction.headers` mode)
- Manual call to `BillingTracker.MarkExhausted()`

Exhausted accounts are **auto-filtered from routing** before scoring, preventing failed requests. Call `ResetSpend()` to clear exhaustion and reset spend counters.

### Agent Budget Limits

Agents can enforce per-agent spend limits via `budget_limit_usd`. This is checked in the retry loop and rejects targets if spend ≥ limit, even if the provider account itself is not exhausted.

## Config Reference

### Provider Billing Config

```json
{
  "providers": {
    "openrouter": {
      "billing": {
        "model": "mixed",
        "period": "monthly",
        "period_hours": 730,
        "included_usd": 10.0,
        "balance_usd": 0,
        "quota_source": "headers",
        "quota_extraction": {
          "mode": "headers",
          "remaining_header": "X-RateLimit-Remaining",
          "limit_header": "X-RateLimit-Limit",
          "reset_header": "X-RateLimit-Reset"
        },
        "free_models": ["gpt-4o-mini-free", "gemini-flash:free"]
      }
    },
    "zai": {
      "billing": {
        "model": "credit",
        "quota_source": "api",
        "quota_url": "https://api.zai.com/v1/billing/quota",
        "quota_interval": "1h",
        "quota_timeout_seconds": 10,
        "quota_backoff_max_seconds": 300,
        "quota_extraction": {
          "mode": "simple_json",
          "balance_path": "credits_remaining",
          "reset_field": "credits_reset_at",
          "reset_unit": "unix_seconds"
        }
      }
    },
    "local-free-provider": {
      "billing": {
        "model": "free",
        "free_only": true,
        "free_models": ["llama-3.2:free"]
      }
    }
  }
}
```

### Field Descriptions

| Field | Type | Description |
|-------|------|-------------|
| `model` | `string` | Billing model: `subscription`, `credit`, `free`, `mixed` |
| `period` | `string` | Billing period: `weekly`, `monthly` (for documentation) |
| `period_hours` | `int` | Period length in hours (for period reset automation) |
| `included_usd` | `float64` | Included credit amount for computing utilization ratio |
| `balance_usd` | `float64` | Static balance (used only if `quota_source: none`) |
| `quota_source` | `string` | Quota source: `none`, `api`, `headers` |
| `quota_url` | `string` | URL to fetch quota (for `api` source) |
| `quota_interval` | `string` | Poll interval (e.g., `1h`, `30m`) |
| `quota_timeout_seconds` | `int` | Timeout for quota fetch (default 10s) |
| `quota_backoff_max_seconds` | `int` | Max backoff delay on consecutive quota fetch failures (default 300s) |
| `quota_extraction` | `object` | Extraction config (see below) |
| `free_only` | `bool` | Strip paid models from target list (only for `model: free`) |
| `free_models` | `[]string` | Explicit list of free model IDs (for scoring bonus) |

#### Quota Extraction Config

**Mode: `simple_json`**
```json
{
  "mode": "simple_json",
  "balance_path": "data.credits_remaining",
  "reset_field": "data.credits_reset_at",
  "reset_unit": "unix_seconds"
}
```
- `balance_path` — JSON pointer to balance field
- `reset_field` — JSON pointer to reset timestamp
- `reset_unit` — `unix_seconds` or `rfc3339`

**Mode: `max_from_array`**
```json
{
  "mode": "max_from_array",
  "array_path": "data.accounts",
  "value_field": "credits_remaining",
  "value_divide_by": 100,
  "reset_field": "reset_at",
  "level_field": "tier"
}
```
- `array_path` — JSON pointer to array
- `value_field` — Field to extract from each element (take max)
- `value_divide_by` — Divide extracted value by this (e.g., cents to dollars)
- `reset_field` — Field for reset timestamp
- `level_field` — Field to store in `QuotaInfo.Level`

**Mode: `headers`**
```json
{
  "mode": "headers",
  "remaining_header": "X-Remaining-Credits",
  "limit_header": "X-Max-Credits",
  "reset_header": "X-Reset-Time"
}
```
- `remaining_header` — Header for remaining balance
- `limit_header` — Header for total limit
- `reset_header` — Header for reset timestamp

### Agent Budget Config

```json
{
  "agents": {
    "my-agent": {
      "models": [...],
      "budget_limit_usd": 50.0
    }
  }
}
```

The `budget_limit_usd` field enforces per-agent spend limits in the retry loop, independent of provider-level exhaustion.

## Routing Behavior

The routing pipeline uses **four stages** for billing-aware decisions:

### Stage 1: Target Building

- `BuildTargetList()` builds targets from agent config
- For `free_only: true` providers, paid models are stripped using `isPaidModelOnFreeOnlyProvider()`
- Priority: explicit list → catalog pricing → name suffix (conservative)

### Stage 2: Exhaustion Filtering

- `filterExhaustedTargets()` removes targets where `BillingTracker.IsExhausted()` returns true
- Runs before scoring to prevent routing to exhausted accounts
- Logs a debug message for each skipped target

### Stage 3: Scoring

- `SortTargetsByBalanced()` applies billing-aware cost weights:
  - `free_only` providers: forced economy mode (×1.5 cost weight)
  - `mixed` providers: free models get +0.4 scoring bonus via `isModelFreeInProvider()`
  - `subscription`/`credit` providers: use configured cost weight
- Priority for free detection in scoring: explicit list → name suffix → catalog pricing (optimistic)

### Stage 4: Retry Loop Enforcement

- `prepareAndSend()` checks `agent.BudgetLimitUSD` before dispatch
- If spend ≥ limit, skips target and emits `nenya_budget_limit_rejected` metric
- This is per-agent, independent of provider-level exhaustion

## Exhaustion & Spend Tracking

### Recording Spend

Spend is recorded after every successful request via `recordCostAndBilling()`:

```go
gw.BillingTracker.RecordSpend(ctx, billing.SpendEntry{
    ProviderName: target.Provider,
    AccountName:  target.AccountName,
    InputTokens:  inputTokens,
    OutputTokens: outputTokens,
    CostUSD:      cost,
    Timestamp:    time.Now(),
})
```

### Marking Exhausted

Exhaustion is marked by:
1. **Quota fetcher** — when API returns balance ≤ 0
2. **Response headers** — when `quota_extraction.headers` detects exhaustion
3. **Manual call** — `BillingTracker.MarkExhausted(ctx, provider, account, reason)`

### Resetting Spend

To reset spend and exhaustion for a new billing period:

```go
gw.BillingTracker.ResetSpend(ctx, provider, account)
```

This:
- Sets per-account spend to 0
- Clears `IsExhausted` flag and `ExhaustedAt` timestamp
- Sets `LastResetAt` to current time
- Recomputes global `TotalSpendUSD` from all per-account spends (concurrent-safe)

### /statsz Endpoint

The `/statsz` endpoint includes billing data:

```json
{
  "billing": {
    "accounts": [
      {
        "provider": "openrouter",
        "account": "default",
        "total_spend": 12.34,
        "is_exhausted": false
      },
      {
        "provider": "zai",
        "account": "account1",
        "total_spend": 5.67,
        "is_exhausted": true
      }
    ],
    "total_spend_usd": 18.01,
    "exhausted_count": 1,
    "total_requests": 1234
  }
}
```

### Utilization Ratio

Compute utilization against the included limit:

```go
ratio := gw.BillingTracker.GetUtilizationRatio(provider, account, includedUSD)
// Returns 0.0-1.0 (spend / limit)
// Returns 0.0 if limit <= 0
```

## Metrics

Billing metrics are emitted via Prometheus:

| Metric | Labels | Description |
|--------|--------|-------------|
| `nenya_billing_spend_usd` | `provider`, `account` | Total spend in cents |
| `nenya_billing_exhausted` | `provider`, `account` | Number of times account was marked exhausted |
| `nenya_budget_limit_rejected` | `model` | Number of targets rejected due to agent budget limit |

## Quota Fetcher Timeouts

The `QuotaFetcher` uses per-provider timeouts configured via `quota_timeout_seconds`
in the provider's billing config (default: 10s). The timeout is enforced at two
levels:

1. **Context deadline** via `context.WithTimeout` with the per-provider timeout
   plus a 5-second buffer for body reading after headers arrive
2. **No global HTTP client timeout** — the shared `http.Client` has `Timeout: 0`,
   allowing independent timeout control per provider via context

If `quota_timeout_seconds` is set to 0 or omitted, the default of 10s is used.
Negative values are rejected by config validation.

If `quota_backoff_max_seconds` is set to 0 or omitted, the default of 300s (5min) is used.
Negative values are rejected by config validation.

### Backoff Behavior

The `QuotaFetcher` applies dynamic backoff between quota fetch attempts:

| Fetch Result | Behavior | Maximum Delay |
|---|---|---|
| **Success** | Resets failure count, returns to normal poll interval | — |
| **HTTP 429 with `Retry-After`** | Uses server-specified delay | Capped at `quota_backoff_max_seconds` (default 5min) |
| **Network error / other error** | Increments exponential backoff level (`baseMs × 2^level` with ±5% jitter) | Capped at `quota_backoff_max_seconds` (default 5min) |

The backoff uses `resilience.BackoffTracker` (shared across all providers) and
`ComputeExponentialBackoffWithJitter` for jittered delays. Consecutive failures
escalate the delay exponentially; a single success resets the failure count.

Example:
```json
{
  "providers": {
    "slow-quota-provider": {
      "billing": {
        "quota_source": "api",
        "quota_url": "https://api.example.com/quota",
        "quota_timeout_seconds": 30,
        "quota_interval": "5m"
      }
    }
  }
}
```

## Testing

### Unit Tests

Unit tests cover:
- `BillingTracker`: record spend, mark exhausted, concurrent access, reset spend
- `QuotaExtraction`: `simple_json`, `max_from_array`, `headers` modes
- Exhaustion detection: threshold logic, edge cases
- Utilization ratio: divide-by-zero protection

### Integration Tests

Integration tests cover the full billing pipeline:
- `TestFilterExhaustedTargets` — exhaustion filtering with and without nil tracker
- `TestCollectProviderFreeModels` — free models map from config
 - `TestIntegration_BillingPipelineWithExhaustion` — spend → exhaustion → filter → reset flow
- `TestIntegration_ScoringWithFreeModels` — per-model free bonus on mixed providers
- `TestIntegration_ConcurrentStressAndExhaustion` — 100 concurrent goroutines, exhaustion state verification