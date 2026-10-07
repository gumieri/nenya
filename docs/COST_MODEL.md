# Cost Model (Peak/Off-Peak & Cached Input)

Status: **design contract** (Phase 001 of the Time-of-Day & Cached-Input Cost
Model module). This document fixes the representation and evaluation rules
before any value or code change lands; later phases implement it.

## Why

Nenya represented every model as a single blended
`input_cost_per_1m`/`output_cost_per_1m` pair and priced all tokens with one
rate. Two common provider behaviors are therefore mispriced:

- **Time-of-day pricing** — DeepSeek bills a peak rate during peak hours and a
  discounted rate off-peak.
- **Cached-input discounts** — DeepSeek and Anthropic bill cache reads
  (hits) below the standard input rate.

Stamping a peak rate into the flat fields overstates off-peak spend and can
also make the `max_cost_per_request` guard skip targets that are actually
cheap at the time of the request.

## Representation

The existing flat fields are redefined as the **standard (off-peak) baseline**.
Two optional dimensions are added; **zero means absent**, so every existing
registry entry and OpenRouter-discovered price keeps its current meaning.

| Field (config `PricingOverride` / `discovery.PricingEntry`) | Meaning                          | Zero rule                                         |
| ----------------------------------------------------------- | -------------------------------- | ------------------------------------------------- |
| `input_cost_per_1m`                                         | Standard (off-peak) input rate   | required baseline                                 |
| `output_cost_per_1m`                                        | Standard (off-peak) output rate  | required baseline                                 |
| `peak_input_cost_per_1m`                                    | Peak input rate                  | `0` = provider has no peak window                 |
| `peak_output_cost_per_1m`                                   | Peak output rate                 | `0` = provider has no peak window                 |
| `cached_input_cost_per_1m`                                  | Discounted cache-read input rate | `0` = cache reads billed at the window input rate |

A model is "peak-priced" iff `peak_input_cost_per_1m != 0` or
`peak_output_cost_per_1m != 0`. `IsZero()` must stay `false` for a
peak-only/cached-only entry (a model with any rate is not priceless).

`HasStandardRate()` (`InputCostPer1M != 0 || OutputCostPer1M != 0`) is the
predicate for "has a usable flat rate". Until the window-aware cost API lands,
every caller that prices via the flat-only `CalculateCost` MUST gate on
`HasStandardRate()`, **not** on `!IsZero()` — otherwise a peak-only entry
(non-zero, no standard rate) would silently price at `$0`.

## Evaluation order

For a request at instant `t` with `input`, `cachedInput`, and `output` tokens:

1. **Window.** `peak := IsPeakAt(provider, t)` — peak only if the provider
   declares a window _and_ the model declares peak rates; otherwise always
   standard.
2. **Window rates.** `inRate`, `outRate` = the peak pair when `peak`, else the
   standard pair. Per dimension: a peak side left at `0` falls back to the
   standard side (a half-declared peak pair never bills at $0).
3. **Cached rate.** `cachedRate = cached_input_cost_per_1m` when set, else
   `inRate`.
4. **Cost.**
   `cachedInput*cachedRate + (input - cachedInput)*inRate + output*outRate`,
   all divided by `1,000,000`. `cachedInput` is clamped to `[0, input]`; token
   counts are clamped non-negative; rates are clamped non-negative (a `NaN`
   rate from an unvalidated feed contributes nothing). The implementation sums
   the three products and divides once — equivalent to the legacy per-term
   form up to one ulp.

API shape (as of Phase 003): `CalculateCost` takes an options struct
(`PricingUsage{Input, CachedInput, Output int64; Peak bool}`) rather than two
scalars, so every caller must state the window and cached-token count
explicitly (AGENTS.md §11).

## Peak windows

Peak windows are **per-provider, expressed in UTC**, and live in config (not
hardcoded), resolved by a pure `IsPeakAt(provider, t)` with an injectable clock
for tests. The window is the **peak interval**, half-open — `start` inclusive,
`end` exclusive. Wrap-around (start > end) is supported. A nil, empty, or
malformed window means the provider is **never peak** at runtime (validation
rejects malformed windows at load; an absent window is legal and is the
"never peak" case). Today there is no opt-out for a built-in window: a user
`peak_window` replaces it, and omitting it inherits the built-in.

The built-in default is DeepSeek, verified against the official rate card
(2026-09): peak hours are **01:00–04:00 and 06:00–10:00 UTC, Monday–Friday**
(Beijing 09:00–12:00 / 14:00–18:00); every other hour — weekends included —
bills at the off-peak rate. A provider may declare any number of windows;
weekend exclusion is expressed per window via `weekdays_only` (the window
belongs to the calendar day its start falls in).

Known approximation: the cached-input rate is a single figure (the off-peak
hit price). DeepSeek's hit rate also doubles at peak ($0.003 → $0.006); a
peak-hour cache hit therefore under-bills by the cached-rate delta — at
DeepSeek's card, $0.003/1M tokens, negligible against the miss rates.

Providers without a window are never peak.

## Invariants

- **Zero-value compatibility.** No existing registry entry or discovered
  price changes meaning; absent peak/cached fields behave exactly as today.
- **Billing is time-correct.** Recorded cost uses the window in effect at
  request time and the cached-token count from the response usage.
  _Approximation:_ the window is sampled at the usage-recording instant
  (completion), so a request spanning a window boundary is priced entirely at
  the completion-side window.
- **Cache writes are out of scope.** Anthropic cache _creation_ tokens (billed
  at a premium) are counted and displayed but priced at the standard input
  rate; only cache _reads_ get the discounted `cached_input_cost_per_1m`.
  Making writes a distinct dimension is a possible follow-up.
- **Peak-only entries are not billed today.** Billing gates on
  `HasStandardRate`; a model declaring only peak rates records no cost until a
  standard baseline is added (Phase 008 ships baseline + peak together).
- **Routing is time-invariant.** The cost-weight signal uses the **standard
  baseline** so routing does not oscillate with the clock; peak is exposed
  for display only.
- **The guard is conservative.** `max_cost_per_request` estimates on the
  **peak** rate (worst case, `Peak: true` with no clock) so it can never
  under-estimate a request's ceiling; peak-less entries fall back to the
  standard pair inside `CalculateCost`. Decision (Phase 006): the guard is
  **not** configurable.
- **No pricing ⇒ no cost.** A catalog lookup miss keeps the current
  fast path (no cost recorded).

## Discovery & merge

Static registry pricing surfaces as catalog `Pricing` at merge time (all
paths: static-only, agent-overridden, and provider backfill), so billing and
the cost guard see it without an attached external feed. An attached external
feed (OpenRouter) overrides the **baseline** but never wipes the static
peak/cached dimensions (`mergeAttachedPeak`: attached baseline wins, static
surcharge preserved). Consequence: statically-priced targets are now subject
to the `max_cost_per_request` guard (they were previously unpriced and
unguarded).

## Display (`/v1/models`)

`/v1/models` exposes the cost model as optional, user-visible fields:
`peak_input_cost_per_1m`, `peak_output_cost_per_1m`, and
`cached_input_cost_per_1m` (omitted when zero — the same "zero = absent" rule
as the config). Agent pseudo-models expose the **chain average**: every
dimension averages over the models that declare a standard rate, so peak-less
priced models contribute 0 to the peak averages. Baseline fields win from
discovered pricing; static peak/cached dimensions are overlaid where a
discovered entry lacks them.

## Supersession

Commit `ea95032` stamped DeepSeek V4.1-Flash with _peak_ rates in the flat
fields and its comment asserts peak as the plain rate. Phase 008 replaced
those values with the verified standard/peak/cached split (standard is the
off-peak rate; peak is the declared surcharge; cache hits ride the cached
rate) and rewrote the comment and CHANGELOG wording.

## Estimation drift (NENYA-135)

Cost and budget math both start from token estimates, and the embedded
cl100k_base vocabulary drifts against non-OpenAI tokenizers — Gemini and
CJK-heavy DeepSeek/Mistral payloads can differ 20–100% from cl100k. The
gateway therefore calibrates: post-transform cl100k estimates recorded at
dispatch and real `prompt_tokens` from upstream usage feed a per-model
decaying ratio (`governance.token_calibration`), which the dispatch path
applies to budget-critical estimates (context-window trim, input budget).
Billed tokens are always the upstream-reported ones; the calibration only
sharpens the dispatch-path estimates (fewer surprise context-limit
errors, fewer wrong trims). The `max_cost_per_request` guard and the
rate limiter still consume the raw pre-pipeline client estimate. Known
bias: dispatches that fail before a usage payload arrives (network
errors, non-retryable 4xx) leave estimates unpaired, deflating ratios
for failure-prone models — inherent to the aggregate-pairing design (the
context-limit retry path additionally records a second, smaller estimate
for its summarized re-dispatch). Observed ratios: `/statsz` →
`token_calibration`.

## Operator surface

A cost total is explainable without reading code:

- `nenya_cost_micro_usd_total{model,window="peak|offpeak"}` — per-model cost
  split by the window it was billed at.
- `nenya_cached_input_tokens_total{model,window}` — cache-hit input volume
  under the same split.
- `/statsz` → `cost_model` — each provider's declared peak windows
  (`{start, end, weekdays_only}`) and each catalog model's rate card
  (`input_cost_per_1m`, `output_cost_per_1m`, `peak_*`,
  `cached_input_cost_per_1m`, `currency`).

## Related

- `docs/CONFIGURATION.md` → Cost Tracking (field reference; extended in
  Phase 010).
- `internal/discovery/pricing.go` (`PricingEntry`, `CalculateCost`).
- `config/entry.go` (`PricingOverride`).
