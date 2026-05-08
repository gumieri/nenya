# Plan: Token-Count Handling & Payload Trimming

**Status:** Draft  
**Goal:** Add generic token-budget-aware payload trimming, expose per-request token usage, and add a configurable hard-limit fallback to reduce reliance on the Ollama engine for every overflow case.

---

## A. Expose per-request token usage

### Motivation
Currently Nenya tracks cumulative token stats per model (`internal/infra/stats.go`) but does **not** include per-request `usage` fields in the proxy response. Agents that rely on the response `usage` object to decide whether to continue a conversation cannot get this data.

### Steps
1. **Create `TokenSnapshot` struct** in `internal/infra/stats.go`:
   ```go
   type TokenSnapshot struct {
       InputTokens  int `json:"input_tokens"`
       OutputTokens int `json:"output_tokens"`
       TotalTokens  int `json:"total_tokens"`
   }
   ```

2. **Build snapshot at dispatch** in `internal/proxy/chat.go` — when the payload is fully built (post-Bouncer, post-`applyMaxTokens`), call `gw.CountTokens(payload)` and store the result in a `context.WithValue(ctx, tokenKey, ...)` or a lightweight per-request struct.

3. **Extract snapshot at response** in `internal/proxy/stream.go` — in the `OnUsage` callback, merge the stored input-token count with the output tokens from the SSE `usage` field, then inject the combined `TokenSnapshot` into the final `data: [DONE]` event (or into the non-streaming response body).

4. **Add response field** — modify the non-streaming path in `internal/proxy/chat.go` so the response JSON includes a `usage` object with `prompt_tokens`, `completion_tokens`, `total_tokens` (OpenAI-compatible).

### Files affected
- `internal/infra/stats.go` — add `TokenSnapshot`
- `internal/proxy/chat.go` — snapshot creation + non‑streaming usage injection
- `internal/proxy/stream.go` — OnUsage merge + streaming usage injection

---

## B. Generic "trim‑to‑max‑tokens" fallback

### Motivation
When a payload exceeds the model's `max_tokens` or a provider-specific cap, Nenya today either:
- Sends the entire payload to the Ollama engine (soft limit), or
- Applies middle-out truncation + optionally TF-IDF (hard limit).

Neither is a simple token-budget trim that can run **before** the Bouncer, saving an engine call for small overflows.

### Steps
1. **Create `TrimPayload` helper** in a new file `internal/pipeline/trim.go`:
   ```go
   // TrimPayload reduces the payload's message list until its token count
   // is <= maxTokens. It operates on individual user/assistant messages,
   // starting from the oldest non-system message, and uses TruncateMiddleOut
   // when a single message must be shortened. Returns true if the payload
   // was modified.
   func TrimPayload(payload map[string]interface{}, maxTokens int, countTokens func(string) int) (bool, error)
   ```

2. **Trimming algorithm**:
   - Parse `messages` from `payload`
   - Keep system messages untouched
   - Iterate from oldest → newest user/assistant messages
   - For each message, if removing it brings the total under `maxTokens`, remove it entirely and continue
   - If no single-message removal suffices, apply `TruncateMiddleOut` to the longest user message

3. **Wire into the routing pipeline** in `internal/routing/transform.go`:
   - After `applyMaxTokens(payload, effectiveMaxOutput)`, call `TrimPayload(payload, effectiveMaxOutput, deps.CountTokens)`.
   - This ensures the cap is enforced at the routing layer, **before** the Bouncer's `interceptContent` runs.

4. **Short-circuit Bouncer** in `internal/proxy/chat.go:interceptContent`:
   - After calling `TrimPayload`, re-check `countTokens`; if already ≤ soft limit, return early (skip engine).

### Files affected
- `internal/pipeline/trim.go` — new file with `TrimPayload`
- `internal/routing/transform.go` — wire TrimPayload after applyMaxTokens
- `internal/proxy/chat.go` — add early-return check after trim

---

## C. Configurable hard-limit fallback

### Motivation
Today `interceptContent` uses a fixed `hardLimit = softLimit * 2` (see `proxy/chat.go`). This should be configurable and, when hit, invoke `TrimPayload` directly **without** going through the Ollama engine.

### Steps
1. **Extend `config.ContextConfig`** in `config/types.go`:
   ```go
   // HardLimitTokens is the absolute maximum token count before the payload
   // is forcibly trimmed. 0 means use softLimit * 2 (backwards-compatible).
   HardLimitTokens int `json:"hard_limit_tokens,omitempty"`
   ```

2. **Update `interceptContent`** in `internal/proxy/chat.go`:
   ```go
   var hardLimit int
   if gw.Config.Context.HardLimitTokens > 0 {
       hardLimit = gw.Config.Context.HardLimitTokens
   } else {
       hardLimit = softLimit * 2
   }
   ```

3. **Add hard-limit fast path** — when `contentTokens > hardLimit`, call `TrimPayload` with `hardLimit`, then check if still above `hardLimit`. If so, log a warning and let the existing `interceptHardLimit` handle it.

### Files affected
- `config/types.go` — add `HardLimitTokens`
- `config/defaults.go` — add default (0 = backward compat)
- `internal/proxy/chat.go` — update `interceptContent` to use `HardLimitTokens`

---

## D. Metrics for trim events

### Motivation
Without metrics it's impossible to know how often trims happen, how many tokens they save, and which models are affected.

### Steps
1. **Add counters in `internal/infra/metrics.go`**:
   ```go
   func (m *Metrics) RecordTrimmedRequest(model string, savedTokens int)
   ```

2. **Wire into `TrimPayload`** — call `RecordTrimmedRequest` whenever a trim modifies the payload.

3. **Export** via the existing `/metrics` endpoint.

### Files affected
- `internal/infra/metrics.go` — new counter methods
- `internal/pipeline/trim.go` — call metrics

---

## E. Tests

| Test | File | What to cover |
|------|------|---------------|
| `TestTrimPayload_UnderLimit` | `pipeline/trim_test.go` | Payload already ≤ maxTokens → no-op |
| `TestTrimPayload_RemoveOldest` | `pipeline/trim_test.go` | Remove oldest non‑system message |
| `TestTrimPayload_TruncateSingle` | `pipeline/trim_test.go` | Shorten a single long message |
| `TestTrimPayload_SystemPreserved` | `pipeline/trim_test.go` | System messages never removed |
| `TestTrimPayload_ZeroMessages` | `pipeline/trim_test.go` | Empty messages list |
| `TestTokenSnapshot` | `infra/stats_test.go` | Snapshot fields correct |
| `TestHardLimitConfig` | `config/config_test.go` | HardLimitTokens applies correctly |

---

## F. Documentation

1. **`docs/CONFIG.md`** — document the new `context.hard_limit_tokens` field with examples.
2. **`docs/ARCHITECTURE.md`** — update the "Interception Pipeline" section to describe the new trim step.
3. **`CHANGELOG.md`** — entry describing the new feature and config changes.