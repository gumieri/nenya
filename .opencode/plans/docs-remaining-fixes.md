---
plan_version: 1
project: nenya
language: go
created: 2026-06-14
---

# Plan: Remaining Documentation Audit Fixes

## Overview

Fix all remaining discrepancies found during the plan-vs-execution comparison of the Docs Audit. Three categories: (1) model table value mismatches and duplicates in CONFIGURATION.md, (2) adapter registration inaccuracies in ADAPTERS.md, and (3) missing zai-coding-plan documentation in PROVIDERS.md.

## Verification

- lint: golangci-lint run
- build: go build ./...
- test: go test ./... -count=1
- race: go test -race -count=1 ./...

## Phases

### Phase 016: CONFIGURATION.md Model Table Cleanup

**Depends on:** (none)
**Branch:** phase-016-model-table-cleanup
**Files:** docs/CONFIGURATION.md

#### Objective

Fix 3 value mismatches and remove 3 duplicate rows from the model reference table (lines 848–855).

#### Steps

1. Line 849 (`kimi-k2`): Remove pricing `$0.10/M` → `—` (registry.go:223 has no Pricing)
2. Line 850 (`nemotron-3-super`): Delete entire row — duplicate of line 793 with wrong pricing (`—` vs `$0.10/M`)
3. Line 851 (`gpt-5-nano`): Delete entire row — exact duplicate of line 836
4. Line 852 (`claude-opus-4-7`): Provider `zen` → `anthropic`
5. Line 853 (`claude-opus-4-6`): Provider `zen` → `anthropic`
6. Line 854 (`claude-sonnet-4-6`): Provider `zen` → `anthropic`, context `1,000,000` → `200,000`
7. Line 855 (`kimi-k2`): Delete entire row — duplicate of line 849

#### Expected Outcome

Model table has zero value mismatches vs registry.go, zero duplicate rows.

---

### Phase 017: ADAPTERS.md Registration Accuracy

**Depends on:** (none)
**Branch:** phase-017-adapters-accuracy
**Files:** docs/ADAPTERS.md

#### Objective

Fix 3 factual inaccuracies in adapter documentation and add a sub‑section explaining the 3‑tier registration model.

#### Steps

1. Line 75 (OpenAIAdapter "Used by" list): Remove `zai-coding-plan` (uses ZAIAdapter at runtime) and `qwen` (not registered — uses default fallback)
2. Lines 168–182 (Provider Adapter Mappings): Rewrite table with Registration column
   - Add `deepseek`, `zen`, `moonshot` (explicitly registered, missing from table)
   - Fix `qwen`/`minimax`: they use **default fallback**, not explicit caps
   - Keep `qwen_free`/`minimax_free`/`nvidia`/`nvidia_free` (explicitly registered)
3. After line 182: Add "Explicit vs Default Registration" sub‑section with 3‑tier table and runtime‑override note for `zai-coding-plan`

#### Expected Outcome

ADAPTERS.md accurately reflects adapter registry; no misleading claims about qwen/minimax registration.

---

### Phase 018: PROVIDERS.md zai-coding-plan Documentation

**Depends on:** (none)
**Branch:** phase-018-providers-zai-coding-plan
**Files:** docs/PROVIDERS.md

#### Objective

Document the `zai-coding-plan` provider variant in all 4 relevant sections.

#### Steps

1. Line 11 (Tier 1 list): Add `z.ai Coding Plan` note under `z.ai`
2. After line 65 (Provider Reference Table): Add row for **z.ai Coding Plan** (same capabilities as z.ai)
3. Lines 133–152 (z.ai Special Behaviors): Update to document both variants
   - Add variant table with endpoint URLs
   - Note shared ZAIAdapter at runtime
   - Clarify thinking injection + temperature defaults are zai only
4. Line 224 (Auth Styles Reference): Add `z.ai Coding Plan` to bearer list

#### Expected Outcome

`zai-coding-plan` is fully discoverable across all provider doc sections.
