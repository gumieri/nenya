# Manual Registry Refresh Runbook

How to refresh the static model/provider registry from the providers' public catalogs. This is the manual counterpart to [dynamic model discovery](CONFIGURATION.md#model-discovery): discovery fetches live `/v1/models` at startup, but string-shorthand model references, agent defaults, cost math, and context budgets are only as good as the static entries in `config/registry.go`.

**Scope:** the eight providers the registry carries curated entries for — `gemini`, `anthropic`, `zai` (+ `zai-coding-plan`), `deepseek`, `groq`, `xai`, `openrouter`, `zen` — plus the smaller free/GitHub/Moonshot/Mistral/Perplexity blocks that follow the same recipe.

---

## 1. What lives where

`config/registry.go` (single file, no external deps):

| Symbol | Contents |
| --- | --- |
| `ProviderRegistry` | Per-provider `URL`, `AuthStyle`, and spec-level behavior for the built-ins. Only touch when a provider moves an endpoint or changes auth. |
| `ModelRegistry` | `map[string]ModelEntry` — the static model catalog. Key = the **canonical API model ID**. |

Each `ModelEntry` carries:

| Field | Meaning |
| --- | --- |
| `Provider` | Registry key of the serving provider |
| `Format` | Wire format override (`"anthropic"` for Anthropic models; absent = provider default) |
| `MaxContext`, `MaxOutput` | Token windows — feed the per-model input budget derivation (`min(max_context/4×3, max_context − effective_output)`) and `auto_context_skip` |
| `Thinking` | Reasoning bounds: `Min`/`Max` budget tokens, `ZeroAllowed`, `DynamicAllowed`, `Levels` (reasoning-effort spellings) |
| `Pricing` | `PricingOverride`: `InputCostPer1M`, `OutputCostPer1M`, optional `PeakInputCostPer1M`/`PeakOutputCostPer1M` (UTC peak windows), `CachedInputCostPer1M` (cache-hit input), `CacheWriteCostPer1M` (Anthropic cache writes) |

**Resolution priority** (see `discovery` docs): config overrides > discovered models > static registry. A stale static entry is shadowed by discovery for listing, but string shorthand, cost math for un-discovered providers, and agent bootstrap resolve through the static entry.

## 2. Ground rules (read before editing)

1. **Canonical IDs only.** The key is what the provider's API accepts today. Keep legacy IDs registered with the same card when the provider still resolves them (precedent: `deepseek-flash` canonical, `deepseek-v4-flash`/`deepseek-v4.1-flash` legacy, `deepseek-v4-flash-vision-exp` retired → removed). Never invent IDs.
2. **Prices are USD per 1M tokens**, matching the provider pricing page's "per million" figure. If a page quotes per-1K, multiply by 1000... no — divide the per-1K price by 1000 to get per-1M.
3. **DeepSeek peak convention:** the official card doubles off-peak in the declared UTC windows (01:00–04:00, 06:00–10:00 Mon–Fri). Record `Peak*CostPer1M` explicitly; do not rely on any implicit multiplier.
4. **Cached-input rate is stored off-peak and is window-independent in the current cost model** — a peak-hour cache hit under-bills by half the cached rate (documented in `COST_MODEL.md`). Only DeepSeek has the discount modeled in the static registry; adding `CachedInputCostPer1M` for another provider makes its discount appear in `/statsz` cost — verify the provider actually reports cache-hit tokens before adding it.
5. **Anthropic cache-write** is input × 1.25 (`CacheWriteCostPer1M`).
6. **Opaque IDs are opaque** (NENYA-146): keys containing `/`, `:`, `.` (e.g. `meta-llama/llama-4-maverick-17b-128e-instruct`) are matched exactly and never case-normalized. Copy them byte-for-byte from the provider catalog.
7. **Thinking configs:** `Levels` must use the reasoning-effort spellings the provider accepts; `ZeroAllowed` only when the provider accepts a zero budget or an "off" level; `DynamicAllowed` only when the provider accepts omitting the budget. When in doubt, omit the field — an absent `Thinking` disables reasoning-parameter injection rather than sending a wrong one.
8. **Free models:** entries with no `Pricing` are treated as free by billing (see `BILLING.md` free-model detection). Keep the `*_free` provider blocks priced at the nominal $0.1 figure they carry only if that matches how the provider meters them.
9. **`non_chat_models` defaults** (`["^jev-"]`) apply when the field is omitted — a decision-only model discovered on any provider is classified automatically (NENYA-180). Registry work does not need to touch this.

## 3. Per-provider sources

Check each provider's public catalog; record the "verified YYYY-MM" date in a comment near the entries you touch (precedent: the DeepSeek comment block in `ModelRegistry`).

| Provider | Source of truth | What to check |
| --- | --- | --- |
| `gemini` | ai.google.dev → *Models* + *Pricing* pages | New `-preview`/`-latest` IDs, context/output limits, thinking ranges, price changes (Flash tier has shifted repeatedly) |
| `anthropic` | docs.anthropic.com → *Models overview* + *Pricing* | New model IDs (alias form preferred: `claude-sonnet-4-5`), `MaxOutput` deltas, cache-write = 1.25× input, adaptive-thinking levels for the 1M-context line |
| `zai` / `zai-coding-plan` | docs.z.ai → *Pricing* / *GLM models* | GLM catalog changes; Coding-Plan entries ride `zai-coding-plan` (plan-scoped concurrency, RPM/TPM built-ins disabled — see provider docs) |
| `deepseek` | api-docs.deepseek.com → *Pricing* | Off-peak/peak pairs, cached rates, canonical-vs-legacy ID status, the V4-Pro-served-by-Flash note (update the comment block if it changes) |
| `groq` | groq.com → *Pricing* + console *Models* | Slashed names (`meta-llama/...`), context windows (Groq lists live limits per model), per-model prices |
| `xai` | docs.x.ai → *Models* + *Pricing* | New Grok IDs, `-fast` variants' differing prices, reasoning levels (`none` allowed on 4.x line), 2M-context entries |
| `openrouter` | openrouter.ai/models | Mostly discovery-driven: add static entries ONLY for IDs agents reference by string shorthand. Verify `architecture.input_modalities`/`context_length` advertised fields are flowing via discovery instead of hand-pinning |
| `zen` | OpenCode Zen docs/models page | IDs and context limits; most entries carry no pricing (plan-bundled). Keep `non_chat_models` semantics untouched (`^jev-` default) |

## 4. Procedure

1. **Discover first, then pin.** Run the gateway with discovery enabled against the provider and diff `/v1/models` output against `ModelRegistry` keys for that provider — this is the fastest way to see what the provider currently serves:
   ```bash
   curl -s -H "Authorization: Bearer $TOKEN" localhost:8080/v1/models | jq -r '.data[].id' | sort
   ```
2. **Verify prices/limits** against the source table for every entry you touch (§3), not just new ones — price drift on existing entries is the most common stale-registry defect.
3. **Edit `config/registry.go`**: add/adjust entries; update the verified-on comment; remove retired IDs (keep legacy-resolvable ones per ground rule 1).
4. **Build + test:**
   ```bash
   GOTMPDIR=/var/tmp/gotmp go build ./...
   TMPDIR=/tmp GOTMPDIR=/var/tmp/gotmp go test ./config/... ./internal/discovery/...
   ```
   `config/registry_capabilities_test.go` pins capability inference for registry entries — extend it if you add a model whose ID shape breaks the heuristics.
5. **Validate + smoke:**
   ```bash
   ./nenya -validate -config <cfg>
   ```
   then run and confirm `/statsz` → `cost_model` shows the new cards, and `/v1/models` lists the entries (static entries always list; discovered entries merge over them).
6. **Update the docs** in the same PR:
   - `docs/CONFIGURATION.md` → *Model Reference Table* (the operator-visible price/context table)
   - `docs/PROVIDERS.md` provider section if IDs/behavior changed
   - the wiki mirror (`../nenya.wiki`: `Configuration.md` table, `Providers.md` section)
   - `CHANGELOG.md` under `[Unreleased]`
7. **Ship it** through the normal PR flow (CI requires `test` + `CodeQL`).

## 5. Verification checklist

- [ ] Every edited entry's price re-verified against the live pricing page (not a cached/aggregator copy)
- [ ] Canonical-vs-legacy ID status confirmed (legacy IDs still resolve upstream before you keep them)
- [ ] Peak windows still match the provider's declared UTC schedule
- [ ] `CachedInputCostPer1M` only where the provider reports cache-hit tokens
- [ ] `go build ./...` and the `config`/`discovery` test packages green
- [ ] `nenya -validate` passes; `/v1/models` and `/statsz` `cost_model` render the new state
- [ ] CONFIGURATION.md table, wiki mirror, and CHANGELOG updated in the same PR

## See also

- [COST_MODEL.md](COST_MODEL.md) — how the rate cards are priced (peak windows, cached reads)
- [DISCOVERY](CONFIGURATION.md#model-discovery) — the dynamic catalog that shadows this registry
- [CONFIGURATION.md](CONFIGURATION.md#model-registry) — the operator-visible model table
