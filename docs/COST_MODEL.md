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
for tests. The window is the **peak interval**; the off-peak interval is its
complement. Wrap-around (start > end) is supported.

The built-in default is the DeepSeek window. DeepSeek publishes the
**off-peak** interval (16:30–00:30 UTC, discounted); the config therefore
stores the complement as the peak interval (00:30–16:30 UTC). The exact
boundary and published rates are confirmed against the live rate card in
Phase 008 — this document fixes the _convention_, not the number.

Providers without a window are never peak.

## Invariants

- **Zero-value compatibility.** No existing registry entry or discovered
  price changes meaning; absent peak/cached fields behave exactly as today.
- **Billing is time-correct.** Recorded cost uses the window in effect at
  request time and the cached-token count from the response usage.
- **Routing is time-invariant.** The cost-weight signal uses the **standard
  baseline** so routing does not oscillate with the clock; peak is exposed
  for display only.
- **The guard is conservative.** `max_cost_per_request` estimates on the
  **peak** rate (worst case) so it can never under-estimate a request's
  ceiling; peak-less entries fall back to the standard pair inside
  `CalculateCost`. (Whether this becomes configurable is decided in Phase 006.)
- **No pricing ⇒ no cost.** A catalog lookup miss keeps the current
  fast path (no cost recorded).

## Supersession

Commit `ea95032` stamped DeepSeek V4.1-Flash with _peak_ rates in the flat
fields and its comment asserts peak as the plain rate. Phase 008 replaces
those values with the standard/peak/cached split and rewrites the comment and
CHANGELOG wording.

## Related

- `docs/CONFIGURATION.md` → Cost Tracking (field reference; extended in
  Phase 010).
- `internal/discovery/pricing.go` (`PricingEntry`, `CalculateCost`).
- `config/entry.go` (`PricingOverride`).
