---
plan_version: 1
project: nenya
language: go
created: 2026-06-12
---

# Plan: docs-audit-fixes

## Overview

Fix all discrepancies found in the comprehensive documentation vs codebase audit. Includes code bugs (pprof non-functional, statsz/metrics auth mismatch), path inconsistencies, missing seccomp configuration, and documentation inaccuracies (field names, model values, metric names).

## Verification

- lint: golangci-lint run
- build: go build ./...
- test: go test ./... -count=1

## Phases

### Phase 010: Fix broken code — pprof + statsz/metrics auth

**Depends on:** (none)
**Branch:** phase-010-fix-pprof-and-public-endpoints
**Files:** cmd/nenya/main.go, internal/proxy/handler.go, internal/proxy/handler_test.go, internal/adapter/registry.go, internal/providers/spec.go

#### Objective

Fix code bugs: (1) pprof endpoint is non-functional due to missing `net/http/pprof` import and `prefix=false`, (2) `/statsz` and `/metrics` require auth but docs promise no auth, (3) add HTTP method enforcement for endpoints that accept any method, (4) register moonshot adapter and ProviderSpec for consistency.

#### Steps

1. In `cmd/nenya/main.go`, add blank import `_ "net/http/pprof"` to the import block. The handlers only register on `DefaultServeMux` but are unreachable without the route + config gate, so unconditional import is safe.
2. In `internal/proxy/handler.go:84`, change the pprof route entry from `{false, "/debug/pprof", ...}` to `{true, "/debug/pprof", ...}` (enable prefix matching so `/debug/pprof/heap`, `/debug/pprof/profile`, etc. work).
3. In `internal/proxy/handler.go:141-151`, change `chainAuthStats` and `chainAuthMetric` from `requireAuth=true` to `requireAuth=false` (pass `false` as third arg to `chainEndpoint`). Rename the functions to `chainStats` and `chainMetrics` to reflect the auth removal.
4. In `internal/proxy/handler.go`, add HTTP method enforcement to chain chains that accept any method:
   - `chainChat` at `:115` — change empty method to `http.MethodPost`
   - `chainEmbeddings` — change empty method to `http.MethodPost`
   - `chainAuthMetric` / `chainMetrics` — change empty method to `http.MethodGet`
5. In `internal/adapter/registry.go`, add `moonshot` adapter registration using `OpenAIAdapter` with default capabilities.
6. In `internal/providers/spec.go`, add `moonshot` ProviderSpec registration.
7. Update `handler_test.go`:
   - Rename `TestServeHTTP_Statsz_NoAuth` to `TestServeHTTP_Statsz_PublicAccess`, change expected status from `http.StatusUnauthorized` to `http.StatusOK`
   - Update `TestServeHTTP_Statsz_ValidAuth` if needed
8. Run `go build ./...` and `go test ./... -count=1`.

#### Expected Outcome

pprof works when enabled via config. `/statsz` and `/metrics` are publicly accessible without auth (GET-only for metrics). HTTP methods are enforced on key endpoints (POST for chat/embeddings, GET for metrics). moonshot provider has full adapter/spec registration for consistency.

---

### Phase 011: Standardize binary path to /usr/bin

**Depends on:** (none)
**Branch:** phase-011-binary-path-standardize
**Files:** install.sh, docs/DEPLOY_BAREMETAL.md

#### Objective

Fix the mismatch where `install.sh` installs to `/usr/local/bin` but `nenya.service` expects `/usr/bin`.

#### Steps

1. In `install.sh:10`, change `INSTALL_DIR="/usr/local/bin"` to `INSTALL_DIR="/usr/bin"`.
2. In `install.sh:225`, change `INSTALL_DIR="/usr/local/bin"` to `INSTALL_DIR="/usr/bin"`.
3. In `docs/DEPLOY_BAREMETAL.md`, find all references to `/usr/local/bin/nenya` and replace with `/usr/bin/nenya`.

#### Expected Outcome

All install paths consistently use `/usr/bin/nenya`. Package installs (deb/rpm/arch) and script installs agree.

---

### Phase 012: Add explicit seccomp to systemd unit

**Depends on:** (none)
**Branch:** phase-012-seccomp-systemd
**Files:** deploy/nenya.service

#### Objective

Add `SystemCallFilter` to the systemd unit to make the "Seccomp" hardening claim real.

#### Steps

1. In `deploy/nenya.service`, add under the existing security directives (after line 35):
   ```
   # Syscall filtering (seccomp)
   SystemCallFilter=@system-service
   SystemCallFilter=~@mount @privileged @raw-io @reboot @swap
   ```
   The `@system-service` group covers all syscalls a Go HTTP server needs. The deny list blocks dangerous syscall groups.
2. Verify the unit loads: `systemd-analyze verify deploy/nenya.service` (on a systemd machine) or manual review.

#### Expected Outcome

systemd unit has explicit syscall filtering. K8s already has `seccompProfile: RuntimeDefault`. Container runtime defaults apply for compose/podman. The "Seccomp + no-new-privileges" claim is now fully backed.

---

### Phase 013: README accuracy fixes

**Depends on:** Phase 010 (auth changes)
**Branch:** phase-013-readme-accuracy
**Files:** README.md

#### Objective

Fix all README inaccuracies: auth table, endpoint methods, flow diagram, shutdown timing, provider count.

#### Steps

1. **Auth table**: In the endpoint table, `/statsz` and `/metrics` already say "None" — no change needed after Phase 010 makes code match. Confirm `/debug/pprof/*` says "Bearer" (correct).
2. **`/v1/files` method**: Change from `GET /v1/files` to `GET/POST/DELETE /v1/files` and update description to "File listing, upload, retrieval, deletion" (already correct).
3. **Flow diagram** (line 20): Change `POST /v1/messages + x-api-key` to `POST /v1/messages + Bearer token`. The `x-api-key` is for outbound Anthropic requests, not inbound to Nenya.
4. **Graceful shutdown**: Change "5s grace period" to "30s grace period" in the Reliability section (line 133).
5. **Provider count**: Verify and update "23 built-in providers" if count changed (currently 24 raw entries, 23 unique providers — keep as-is or clarify).
6. **`/v1/batches` method**: Change from `POST /v1/batches` to `POST/GET /v1/batches` since it supports sub-path retrieval via prefix routing.

#### Expected Outcome

README endpoint table, flow diagram, and feature descriptions match actual code behavior.

---

### Phase 014: CONFIGURATION.md field name and model table fixes

**Depends on:** (none)
**Branch:** phase-014-configuration-docs
**Files:** docs/CONFIGURATION.md

#### Objective

Fix all incorrect field names, ThinkingConfig documentation, and outdated model reference table.

#### Steps

1. **Line 17**: Change `#response-cache` anchor to `#response_cache`.
2. **Line 59**: Change `` `response-cache` `` to `` `response_cache` ``.
3. **Line ~194**: Change `patterns` field name to `redact_patterns` in the bouncer config table.
4. **Line ~197**: Change `output_enabled` to `redact_output`.
5. **Line ~198**: Change `output_window_chars` to `redact_output_window`.
6. **Lines 695-764**: Rewrite the Thinking Configuration section. `ThinkingConfig` only has `enabled` (bool) and `clear_thinking` (bool). Remove `min`, `max`, `zero_allowed`, `dynamic_allowed`, `levels` fields — those belong to `ModelThinkingConfig` (internal, per-model registry). If ModelThinkingConfig should be user-configurable, document it as a separate section.
7. **Lines 780-853**: Update the Model Reference Table by regenerating values from `config/registry.go`. Key corrections:
   - All Gemini models: 128K→1,048,576 context, 8K→65,536 output
   - `claude-3-7-sonnet-20250219`: 200K→128K context, 64K→8,192 output
   - `claude-3-5-sonnet-20241022`: output 8,192→64,000
   - `claude-sonnet-4-6`: context 1M→200,000
   - `kimi-k2.6`: context 200K→262,144, output 8K→65,536
   - `kimi-k2.5`: context 200K→131,072, output 8K→32,768
   - Add missing models: `gemini-3.5-flash`, `gemini-2.5-pro`, `gemini-3.1-pro-preview`, `claude-opus-4-1-20250805`, `kimi-k2-thinking`, GPT-5.x family, `codex-auto-review`
8. **Add undocumented fields**: Document `context.hard_limit_tokens`, `compaction.compaction_preset`, `governance.cost_mode`, `governance.billing_economy_scale`, `governance.billing_quality_scale`, and `providers.<name>.billing` sections.

#### Expected Outcome

All documented JSON field names match actual `config/types.go` struct tags. Model values match registry. No phantom fields.

---

### Phase 015: Remaining docs fixes (ARCHITECTURE, PASSTHROUGH, ADAPTERS, PROVIDERS)

**Depends on:** (none)
**Branch:** phase-015-remaining-docs
**Files:** docs/ARCHITECTURE.md, docs/PASSTHROUGH_PROXY.md, docs/ADAPTERS.md, docs/PROVIDERS.md

#### Objective

Fix metric name errors, package path errors, missing metric references, and provider capability mismatches.

#### Steps

**ARCHITECTURE.md:**
1. Replace all `internal/config/` references with `config/` (top-level package, not under `internal/`).
2. Add `internal/billing/`, `internal/auth/`, `internal/security/`, `internal/version/` to the package DAG.
3. Fix metric name: `nenya_backoff_level_total` → `nenya_backoff_increments_total`.
4. Update `InterceptResult` line reference (53, not 54).

**PASSTHROUGH_PROXY.md:**
5. Remove reference to `nenya_proxy_requests_total` metric at line 66. The metric does not exist in code.

**ADAPTERS.md:**
6. Clarify that `qwen`, `minimax`, `moonshot` use the default `OpenAIAdapter` (not explicitly registered). Only `qwen_free`, `minimax_free` have explicit registrations. After Phase 010, `moonshot` will have explicit registration.

**PROVIDERS.md:**
7. Fix MiniMax `AutoToolChoice` from `false` to `true` in capability table.
8. Add `zai-coding-plan` as a variant of z.ai (or note it as a built-in provider).

#### Expected Outcome

All architecture docs reference correct package paths and metric names. Passthrough docs don't reference non-existent metrics. Provider capability tables match code.