# HA / Multi-Replica State Externalization — Design (NENYA-134)

Status: design, not yet implemented. This document inventories Nenya's
in-process coordination state, classifies it by consistency requirement,
and defines the phased plan for externalizing it so multiple replicas can
serve traffic behind one endpoint without redundant limits, split-brain
cooldowns, or lost affinity.

## 1. Problem

`docs/DEPLOY_KUBERNETES.md` shows `replicaCount: 3` behind a Service, but
every coordination structure in the gateway lives in process memory. With
N replicas:

- **Rate limits multiply**: a 60 RPM provider limit becomes 180 RPM
  aggregate — the upstream, not Nenya, enforces the real limit (429s,
  burned quota).
- **Billing budgets multiply**: per-account spend tracking is per-pod, so
  an exhausted account keeps receiving traffic from the other replicas.
- **Circuit breakers and cooldowns fragment**: each replica trips
  independently, so a failing provider keeps receiving 1/N of traffic
  and the cutoff only completes once every replica has independently tripped.
- **Latency-based reordering degrades**: each `LatencyTracker` sees only
  1/N of samples; medians are noisier and replicas reorder differently.
- **Response cache hit rate divides**: identical requests miss on a
  different replica; the semantic cache pays its Ollama embedding cost
  N times.
- **Usage/cost accounting splits**: `/statsz` and Prometheus counters
  report per-pod; aggregation requires summing N series (workable for
  Prometheus, broken for the in-process `/statsz` JSON).
- **Session affinity is lost**: provider-affinity chains (prompt-cache
  friendliness) only hold if the same replica sees the conversation.
- **Window-compaction continuity resets**: summaries, window heads, and
  TF-IDF selections live per-replica, so a conversation that hops
  replicas is re-summarized and re-frozen (prompt-cache misses).

Single-replica deployments are unaffected; everything below is opt-in.

## 2. State inventory

| # | State | Location | Keyed by | Class (§3) | Loss/cross-replica impact |
|---|-------|----------|----------|------------|---------------------------|
| 1 | Provider/host rate limits (RPM/TPM) | `infra/ratelimit.go` | bucket key = provider \| upstream host (resolved by `bucketKey()`) | A-strict | limits multiply across replicas |
| 2 | Key rate limits & token budgets | `auth/keylimits.go` (`KeyUsageTracker`) | API key; per-provider daily budgets keyed by provider | A-strict | per-key RPM limits, daily budgets, and provider budget tiers multiply ×N |
| 3 | Billing spend totals / account exhaustion | `billing/tracker.go` (`BillingTracker`) | provider:account composite | A-strict | exhausted accounts keep receiving traffic from other replicas |
| 4 | Concurrency slots | `infra/concurrency.go` | provider/model composite | A-strict | in-flight caps multiply |
| 5 | Circuit breakers | `resilience/circuitbreaker.go` | `CoolKey = agent:provider:model` | B-eventual | fragmented tripping; wasted upstream calls |
| 6 | Provider/account/model cooldowns | `auth/pool.go` (`AccountPool`: `RateLimitedUntil`, `ModelLocks` account+model, weighted virtual-clock rotation), `resilience/backoff.go`, `CircuitBreaker.modelLocks` (quota/class cooldowns) | account ID, account+model (pool); circuit key, model (breaker) | B-eventual | fragmented cooldowns; 429 storms |
| 7 | Latency samples | `infra/latency.go` | provider+model | B-eventual | noisier reordering |
| 8 | Usage/cost counters | `infra/stats.go`, `infra/cost_tracker.go` | model | B-eventual | split accounting |
| 9 | Thought-signature cache | `infra/cache.go` (`ThoughtSignatureCache`) | tool-call ID | B-eventual (correctness note) | cross-replica miss = missing `thought_signature` on follow-up turns → upstream errors for Gemini function-calling |
| 10 | Window-compaction continuity | `pipeline.SummaryCache` (window summaries), `WindowHeadCache`, `pipeline.TfidfSelectionCache` | conversation/session | C-disposable | re-summarization + refrozen selections + prompt-cache misses on replica hop |
| 11 | Response cache (exact + semantic) | `infra/response_cache.go` | request fingerprint (SHA-256 + auth-token hash) | C-disposable | hit-rate ÷ N, extra embed cost |
| 12 | Agent round-robin cursors | `routing/targets.go` (`AgentState.Counters`) | agent | C-disposable | rotation restarts from cursor zero on replica hop (negligible) |
| 13 | Session affinity | `routing/affinity.go` (session key → leading-turn fingerprint chain), `routing/session.go` (`SessionRouter`: SHA-256(agent\|system\|first user msg)) | session/conversation key | C-disposable | prompt-cache locality lost |

Out of scope (stay per-replica by design): secrets (`security.SecureMem`),
MCP client connections (each replica keeps its own SSE sessions — the
MCP servers must tolerate N connections), SSE streams to clients.
**Local engine caveat**: `local.EngineManager` is per-replica but targets
a single-host Ollama — with N replicas every replica preloads/evicts the
same models and their LRU evictions fight. Operators running the local
engine must pin it to one replica (or disable it elsewhere); see §8.

## 3. Consistency classes

**Class A — strict (correctness).** Rate limits, key budgets, billing
exhaustion, and concurrency slots only mean something if every replica
observes them atomically. These need a shared store with atomic
read-modify-write (Lua/transactions). A backend outage here fails
**open with a local clamp by default** (`fail_open: false` rejects with
503 instead): each replica falls back to
`limit / max(N, replica_count)` local buckets so aggregate behavior stays
approximately correct instead of unbounded.

**Class B — eventual (efficiency + correctness-adjacent).** Breakers,
cooldowns, latency samples, usage counters, and the thought-signature
cache tolerate seconds of propagation delay. Strategy: breaker
transitions are synchronous read-through compare-and-swap at decision
points (they gate half-open probe accounting — see §5); timestamps,
samples, and signatures use write-behind with a 1s flush and
last-writer-wins; **counters flush as additive deltas** (INCRBY of the
buffered increment) — LWW SETs would drop concurrent increments and
silently undercount usage/cost totals. The
thought-signature cache is the one Class B entry with a correctness
edge: a cross-replica miss surfaces as an upstream Gemini error on the
next function-calling turn — acceptable frequency-wise, but it must be
documented and observable (a Prometheus counter for cross-replica
signature misses). A backend outage degrades to today's per-replica
behavior — safe.

**Class C — disposable (cache/affinity).** Response cache, affinity,
window-compaction continuity, and round-robin cursors are optimizations. Strategy: shared store
with TTL, best-effort on both read and write; miss = recompute. Outage =
cold local cache.

## 4. Backend options

| Option | Fit | Notes |
|--------|-----|-------|
| **Redis (recommended)** | A, B, C | Atomic Lua for Class A; TTLs native for C; ubiquitous in K8s estates; single new dependency behind an interface |
| NATS JetStream | B | Great for event streams (breaker state changes), weaker for atomic counters |
| Postgres | A, B | Transactional but higher per-request latency for hot paths (rate limit check is on every dispatch) |
| Coordinator-free (gossip) | B | No new infra, but split-brain semantics defeat Class A entirely |

**Recommendation: Redis 7+**, accessed per class through distinct key
prefixes, exclusively via small store interfaces so the in-memory
implementation remains the default and tests never require Redis.
**Dependency note**: this is the project's first third-party dependency
(`go.mod` has zero requires, and the dependency policy mandates explicit
human authorization). The Phase 2 driver uses `github.com/redis/go-redis/v9`;
implementation must not start without that authorization on record.

## 5. Interface design

Each hot-path structure gains a store interface. The interfaces live
**consumer-side** (RateLimitStore in `internal/infra`, BreakerStore in
`internal/resilience`, etc.) rather than in a new leaf package: a
`internal/state` package defining `BreakerStore` would have to import
`resilience` (for the state type), placing it right of `resilience` in
the enforced DAG while `infra` sits left — an awkward middle position for
no benefit. The Redis driver lives in a new flat `internal/state` package — all
internal packages are flat today, and the enforced DAG guard rejects
nested package paths. The in-process versions are the default implementations,
so zero-config behavior is unchanged:

```go
// internal/infra (consumer-side)
type RateLimitStore interface {
    // Allow consumes one token atomically across all replicas.
    // scope: "provider" | "host"; the caller resolves the bucket key as
    // today (provider name, else upstream host, else raw URL).
    // opts groups the bucket parameters (per §11 options-struct rule).
    Allow(ctx context.Context, opts AllowOpts) (allowed bool, retryAfter time.Duration, err error)
    // AllowOpts: Scope ("provider"|"host"), Key, Limit, Window, Tokens.
}
type ConcurrencyStore interface {
    // Acquire takes a lease with TTL (not a blocking semaphore): on
    // success the caller holds a slot until Release or lease expiry.
    // TTL is set to the provider's dispatch timeout + streaming slack and
    // renewed by heartbeat for long SSE streams; crashed replicas release
    // slots at TTL expiry. A missed heartbeat renewal (Redis blip) can
    // orphan a live lease mid-stream — the resulting over-admission is
    // bounded by the orphaned-lease count until TTL and is accepted with
    // the same explicitness as breaker probe over-admission. When the
    // aggregate is exhausted (ok=false) the caller keeps today's
    // behavior: block on the local limiter until a slot frees (retrying
    // Acquire with jitter), bounded by the request context.
    Acquire(ctx context.Context, key string, limit int, ttl time.Duration) (lease string, ok bool, err error)
    Release(ctx context.Context, key, lease string) error
}
type CounterStore interface { /* Inc/Add + Snapshot for /statsz */ }

// internal/resilience (consumer-side)
type BreakerStore interface {
    // Snapshot/Transition carry the full mutable breaker record — counts,
    // generation, halfOpenInflight, expiry, model-lock cooldowns — not
    // just (state, until): half-open probe limits and failure thresholds
    // cannot be enforced cross-replica from a state enum alone.
    //
    // Transitions are compare-and-swap on a monotonic generation;
    // recovery may transiently over-admit probes across replicas
    // (bounded by N × the per-replica half-open max-inflight), which is
    // accepted and documented.
    Snapshot(ctx context.Context, key string) (BreakerRecord, bool, error)
    // ErrGenerationConflict (sentinel) signals a lost race so the caller
    // re-reads and re-decides.
    Transition(ctx context.Context, key string, rec BreakerRecord, expectGen uint64) error
}
```

Redis specifics per class: Class A `Allow` is a single Lua EVAL (token
bucket with server-side time); Class B uses pipelines + 1s write-behind;
Class C uses SET … EX / GET. Wiring follows the existing pattern: `main.go`
builds the configured store implementations and passes them through
`NenyaGateway` fields alongside `MCPSecurityChain`.

**Reload discipline**: the store handles are carried across SIGHUP like
`Stats` and `BillingTracker` already are, and rebuilt only when the
`network_state` section itself changed (precedent: `Reload` re-applies
config-dependent carried state). Write-behind buffers flush on close,
mirroring `ResponseCache.Stop()`.

Config surface (sketch):

```json
{
  "network_state": {
    "backend": "memory",            // "memory" (default) | "redis"
    "redis_url": "redis://redis:6379/0",
    "key_prefix": "nenya:",
    "replica_count": 3,             // Class A local-clamp divisor fallback
    "fail_open": true,              // false: reject dispatches with 503
                                    // error_kind=state_backend_unavailable
                                    // when the Class A backend is down
    "flush_interval_ms": 1000       // Class B write-behind
  }
}
```

**Replica count under HPA**: a static `replica_count` makes the local
clamp wrong as N changes (limit/3 with 5 live replicas ≈ 1.7× aggregate).
Phase 3 derives N from a backend instance registry (each replica holds a
TTL heartbeat key): `N = max(live_keys, 1)`; the clamp divisor is
`max(N, replica_count)` so a misconfigured floor can only under-admit,
never over-admit. HPA guidance documented alongside.

## 6. Failure semantics (fail-open ladder)

| Backend state | Class A | Class B | Class C |
|---------------|---------|---------|---------|
| healthy | atomic shared | shared, write-behind | shared, TTL |
| degraded (slow) | circuit-break the store after `store_failure_threshold` timeouts → local clamp | stale reads, buffered writes | local cache only |
| down | local clamp (`limit / max(N, replica_count)`, target-state formula per §5), or reject with 503 `state_backend_unavailable` when `fail_open=false` | per-replica memory | per-replica memory |
| recovery | shared buckets are authoritative; local-clamp consumption during the outage is **discarded, not merged**: outage usage is undercounted (an accepted over-admission window immediately after recovery) rather than risk merging partially-applied flush buffers | shared state re-read on next decision | shared cache repopulates |

The store itself gets a mini circuit breaker so a flapping Redis cannot
add latency to the dispatch hot path. Every degrade is logged once per
state change and exported as `nenya_state_backend_up` (gauge) +
`nenya_state_backend_failures_total`.

## 7. Phased plan

Each phase is independently shippable and reverts to memory on config
rollback.

1. **Phase 1 — interfaces + adapters**: consumer-side store interfaces,
   memory implementations wired as defaults, no behavior change. Existing
   structs need thin adapters (limits and windows live inside
   `RateLimiter` today; `Allow` makes them explicit parameters). A
   releasecontract drift test binds this document's §2 inventory to the
   code (file/struct existence assertions) so the inventory cannot rot.
   No config surface and no new package yet.
2. **Phase 2 — Redis driver + Class B** (breakers, cooldowns, latency,
   usage, thought-signature cache): biggest HA win, lowest risk (eventual
   consistency), exercised by the `resilience` decision points (including the
   cross-replica signature-miss counter mandated by §3). This
   phase adds the flat `internal/state` package — `docs/ARCHITECTURE.md`
   (DAG fence + Package Overview) and `docs/CONFIGURATION.md`
   (`network_state` surface) update in the same change, per the
   drift-guard house rules.
3. **Phase 3 — Class A atomic limits**: Redis Lua token buckets
   (`Allow` is one EVAL), lease-based concurrency, instance-registry
   replica counting, local-clamp fallback, `fail_open` semantics. Covers
   Class A rows 1–4: provider/host limits (#1), key limits and daily
   budget tiers (#2 — a keyed counter-with-gate, same EVAL shape),
   billing spend/exhaustion (#3 — counter-with-gate keyed
   provider:account, distinct store shape from a token bucket), and
   concurrency leases (#4). This
   phase registers the new `state_backend_unavailable` ErrorKind
   (`internal/infra/errors.go`: non-retryable; `Scope()` must be
   `ScopeConnection` — NEVER `ScopeProvider`, which would feed cooldown/
   rotation state and poison provider health off a Redis outage).
4. **Phase 4 — Class C**: shared response cache (bytes + TTL; semantic
   cache stores embeddings alongside), affinity chains, window-compaction
   continuity stores, and round-robin cursors (#12, CounterStore
   INCRBY + TTL).
5. **Phase 5 — `/statsz` aggregation** and multi-replica e2e test
   (`deploy/compose.yml` with 2 replicas + Redis, asserting the aggregate
   rate limit holds; Helm chart values documented for
   `deploy/chart/nenya/`).

## 8. Non-goals

- Distributed tracing / control plane (out of scope; OTel already covers
  observability).
- Multi-region: this design assumes one low-latency network (sub-ms to
  Redis). Cross-region HA needs a different consensus design.
- Shared local-engine state: local Ollama is single-host; multi-replica
  operators must pin the local engine to one replica or disable it on the
  others (the `local_engine` section is per-replica config).
- Secrets distribution: stays file/git-crypt based (`SECRETS_FORMAT.md`).
