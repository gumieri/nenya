# Spike: Reversible Pseudonymization × Prompt Caching (C010, NENYA-356)

Status: **mechanical gates evaluated; live cache measurement pending operator runs —
C020 remains gated until that measurement lands.**

Module C proposes reversible pseudonymization: declared terms (company, people,
domains, identifiers) are replaced with deterministic pseudonyms on request text
and restored on response text, so the model never sees the real values while the
conversation stays coherent. The property that makes or breaks the idea is its
interaction with provider prompt caches: pseudonymization rewrites prompt bytes,
and any instability in that rewrite converts cache hits into cache misses (full
input re-ingestion at first-turn rates on every turn).

Phase 010 is the spike and benchmark harness that decides go/no-go. Gate
criteria (recorded at planning time):

| Criterion | Gate | Measures |
|---|---|---|
| Apply rate | ≥ 99.5% | patched occurrences / detected occurrences over real traffic |
| Unresolved rate | ≤ 0.5% | declared-term variants (inflections, distinct words) left verbatim |
| Test impact | ≥ 95% of baseline | full suite stays green with the mechanism active |
| Round-trip fidelity | 0 MISMATCH | restore(patch(x)) == x modulo recorded case loss |
| Cache compatibility | operator-judged | live cached-token deltas, patched vs unpatched sessions |

## What landed in this phase

- `internal/pseudonym` — the mechanism, stdlib-only:
  `Table` (declared terms, longest-first), `Patcher` (deterministic
  `<KIND>-<hex12>` pseudonyms keyed on `(salt, kind, lowercase term)`; word-boundary
  case-insensitive matching; exact-over-inflected precedence so a possessive of a
  declared term patches the term and keeps `'s` natural), `Restorer` (non-destructive
  mapping back to canonical casing), `PatchStats` (the gate's accounting).
- `cmd/pseudonym-spike` — the harness: `--terms terms.json --corpus DIR` measures
  the mechanical gate over any corpus and emits a JSON report; `--emit-requests DIR`
  writes the operator's live-run request bodies (4-turn cumulative conversation,
  `unpatched/` + `patched/`, OpenAI format); `--salt` pins the epoch for
  reproducibility.
- Contract pins (`internal/pseudonym/pseudonym_test.go`): determinism within a salt,
  salt-rotation cache bust, byte-exact round trip on exact-case documents, canonical
  restore for case variants, unresolved accounting for distinct-word inflections,
  no-split-token matching, longest-first overlap handling, apply-rate arithmetic.
- Benchmarks (`go test ./internal/pseudonym -bench .`): the measured throughput on
  a 16 KiB conversational fixture.

## Measured results (this repo, offline, fixture corpus)

Mechanical run (`pseudonym-spike --terms testdata/terms.json --corpus testdata/corpus
--salt spike-run-001`), fixture corpus of 3 documents deliberately seeded with case
variants, a possessive, a distinct-word inflection ("Acme Corps"), an email address
carrying the domain mid-token, and repeat lines:

```
docs=3 terms=4 occurrences=38 patched=37 (97.37%) unresolved=1 (2.63%)
round-trip byte_exact=2 case_loss=1 MISMATCH=0
```

Read-through, honestly:

- **Round-trip fidelity: PASS.** Zero MISMATCH documents. `restore(patch(x)) == x`
  holds exactly wherever the casing matches the declaration; the one case_loss
  document is the recorded, counted loss (case variants restore to the declared
  casing — a fidelity cost the gate tracks, not a correctness bug).
- **Apply/unresolved rates: fixture-informed, not gate-deciding.** The fixture
  deliberately contains a distinct-word inflection; 97.37%/2.63% on THIS corpus does
  not meet the ≥99.5%/≤0.5% gates, and it is not supposed to — the gate is defined
  over OPERATOR-real corpus and traffic. The fixture proves the accounting detects
  and classifies every variant class. The operator runbook (below) is the step that
  produces the deciding numbers.
- **Throughput: measured, adequate for prompt sizes, not hot-path-ready.** Patch
  ≈ 2.4–3.8 MB/s run-to-run on this machine, restore ≈ 180–190 MB/s (16 KiB fixture;
  `go test ./internal/pseudonym -bench . -benchmem`; the ~4.4k allocs/document figure
  is the stable one). A 100 KiB prompt patches in ~30–40 ms — acceptable off-band,
  but a production C020 wiring needs a fused single-automaton scanner (the current
  two-alternation merge costs ~4.4k allocs per document — see the stable figure
  above) and would add the patch to the interceptor chain's budget. Recorded as
  C020 design input.
- **Test impact: none yet by construction.** The spike is not wired into the request
  path, so the suite is unaffected (the full gate ran green with the package present).
  The ≥95%-of-baseline gate becomes live evidence only when C020 wires the mechanism;
  the harness's per-doc accounting plus the contract pins are the standing proxy.

## Known limitations (surface now, decide later)

1. **Inflections are detection-only.** term+suffix distinct words ("Acme Corps") are
   counted unresolved and left verbatim — correct (patching would corrupt the word)
   but it caps the apply rate on languages with rich inflection. Language packs are
   C060. Word-boundary semantics also mean a mid-token domain ("acme.community"
   sharing the "acme.com" prefix) never matches — under-protection there belongs to
   the identity layer (C040).
2. **Case fidelity.** Same term, different casing → same pseudonym → restore renders
   the declared casing. Cross-turn determinism REQUIRES casing-insensitive pseudonym
   keys; per-occurrence casing would break the cache-stability property. The trade is
   recorded and measured, not hidden.
3. **Mapping sensitivity.** The mapping plus the salt re-identifies everything; they
   share a store and lifetime. C020 must define that store (where, TTL, who reads it)
   before any wiring.
4. **Pseudonym shape.** `ORG-<hex12>` / `DOM-<hex12>.invalid` / `PER-<hex12>` /
   `SEC-<hex12>` are legible enough for models to reason about roles while carrying
   no structure of the real value. The `.invalid` domain cannot resolve even if a
   model leaks it. Shape tuning is a C020 concern (token-cost per pseudonym matters:
   ~18–23 bytes per replacement).
## Live measurement — operator runbook (the deciding half)

Goal: quantify the prompt-cache cost of pseudonymization on real models, patched
vs unpatched, and the salt-rotation cache bust. Needs ≥ 2 models with provider-side
prompt caching (e.g. one Anthropic-format, one OpenAI-format or DeepSeek).

1. Generate the run inputs:

       go run ./cmd/pseudonym-spike \
         --terms cmd/pseudonym-spike/testdata/terms.json \
         --corpus cmd/pseudonym-spike/testdata/corpus \
         --salt live-run-001 --emit-requests /tmp/spike-live --out /tmp/spike-stats.json

2. For EACH model (≥ 2), dispatch the four cumulative turns of each sequence
   (`unpatched/turnN.json`, then `patched/turnN.json`) with the provider's prompt
   caching enabled, honoring each provider's caching contract:
   - Anthropic-format: POST /v1/messages, add `cache_control: {"type":"ephemeral"}`
     on the system block (or last message block) per docs; usage carries
     `cache_read_input_tokens` / `cache_creation_input_tokens`.
   - OpenAI-format (OpenAI/DeepSeek/…): POST /v1/chat/completions as emitted
     (automatic caching where offered; usage carries `prompt_tokens_details.cached_tokens`,
     DeepSeek: `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens`).
   Record per turn: `cache_read`, `cache_creation`/`cache_miss`, input tokens, cost.

3. Salt-bust check: regenerate `patched/` with a different `--salt` and re-dispatch
   turns 2–4 of one model; confirm cache reads collapse to creation (the isolation
   boundary costs exactly one re-ingestion per epoch).

4. Decide against the gate table: the go/no-go question is **cache-read retention** —
   the fraction of turns 2–4 cache reads that survive pseudonymization (patched vs
   unpatched sessions), per model. Deterministic pseudonyms predict ≈ full retention
   (identical patched bytes across turns); the live run verifies on real routers.

5. Record outcomes in this file (append a "Live measurement — <date>" section) and
   set the C010 Plane state with the decision: **go** (C020 proceeds) or **no-go**
   (C020/C030 cancel per the recorded plan; C040+ identity-layer work re-scopes).

## Decision template

```
Live measurement — <date>
Models: <model A>, <model B>
Cache-read retention turns 2-4, patched/unpatched: A=<x%> B=<y%>
Salt-rotation bust verified: <yes/no>
Apply rate on operator corpus (<N> docs): <x%> (gate >= 99.5%)
Unresolved rate: <y%> (gate <= 0.5%)
Decision: go | no-go — rationale
```
