# Injection & Exfiltration Defense

This document consolidates the anti-poisoning stack: why it exists, how the
layers compose, how to operate and roll it out, and what it deliberately does
**not** cover. Each layer is opt-in or defaulted as noted; the code is the
source of truth and this document tracks it.

- [Threat model](#threat-model)
- [Defense in depth](#defense-in-depth)
- [Configuration quick reference](#configuration-quick-reference)
- [Response-path controls](#response-path-controls)
- [Operational playbook](#operational-playbook)
- [Client composition](#client-composition)
- [Honest limitations](#honest-limitations)
- [Research and references](#research-and-references)

## Threat model

Nenya sits between local AI coding clients and upstream providers, so the
attack surface is the **content** flowing through it: user prompts, tool
results, memory context, model output, and tool-call arguments.

**The lethal trifecta** (Willison): an agent that (1) processes untrusted
content, (2) has access to private data, and (3) can communicate externally is
exfiltratable by prompt injection alone. A coding gateway exhibits all three
legs:

| Leg               | In Nenya                                                                                       | Attacker path                                                                     |
| ----------------- | ---------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| Untrusted content | Tool results, MCP/memory context, web/file content pasted into prompts, client-side MCP output | Poisoned tool result carries instructions the model obeys                         |
| Private data      | Conversation history, secrets in context, MCP memory, provider credentials                     | Instructions tell the model to read and transmit context                          |
| Egress            | Model output to the client, tool calls to MCP servers, links in markdown                       | Instructions tell the model to call a tool with private data or emit a beacon URL |

**OWASP Top 10 for LLM Applications mapping:**

Titles below follow the OWASP Top 10 for LLM Applications **2025** list.

| Risk                                   | Nenya defense                                                                                               |
| -------------------------------------- | ----------------------------------------------------------------------------------------------------------- |
| LLM01 Prompt Injection                 | Deterministic detector (`governance.injection`), two-tier escalation, spotlighting (`governance.spotlight`) |
| LLM02 Sensitive Information Disclosure | Tier-0 redaction + entropy redaction, bouncer summarization, output egress controls                         |
| LLM05 Improper Output Handling         | ExfilGuard URL policy (`governance.exfil_guard`), canary tripwire (`governance.canary`)                     |
| LLM06 Excessive Agency                 | MCP argument guard (`governance.mcp_guard`) bounds what a hijacked model can make tools do                  |

The posture is **defense in depth with a deterministic floor**: every layer is
independently useful, the deterministic detectors never depend on an LLM
verdict to act, and LLM-assisted layers can only _weaken_ their own false
positives — never the floor.

## Defense in depth

```
                       ┌───────────────────────── input path ─────────────────────────┐
 client request ──▶ [auth/RBAC] ──▶ [interceptor chain] ──▶ [MCP dispatch guards] ──▶ upstream
                                       │                          │
                      priority 10 RedactInterceptor (strict)        ├─ schema validation
                      priority 12 SpotlightInterceptor (strict)      ├─ 1 MiB argument cap
                      priority 15 InjectionInterceptor (strict)      └─ URL policy (deny_private)
                      priority 20 EntropyInterceptor (strict)
                      priority 30 TFIDFInterceptor (fail-open)
                      priority 50 BouncerInterceptor (bouncer.fail_open)

                       ┌──────────────────────── output path ─────────────────────────┐
 upstream ──▶ [streaming filters] ──▶ [ExfilGuard] ──▶ [CanaryWatcher] ──▶ client
                    │                    │                  │
                    │           URL policy on links   marker echo watch
                    │           + bare URLs           (stream / buffered / tool args)
                    └─ redaction-of-output, entropy, format transforms
```

**Layer 1 — Input normalization (Phase 010).** `WalkMessageText` visits every
text surface (string content, multimodal text parts, tool-call arguments);
financial presets and JWT patterns extend redaction; truncation paths are
hardened so a crafted payload cannot desynchronize the sanitizer.

**Layer 2 — Deterministic detection and neutralization (Phase 020).**
`InjectionInterceptor` scans all message surfaces for instruction-override
phrasing, role/format forgery (ChatML, `<system>` tags), hidden-text carriers
(invisible formatting characters, instruction-bearing HTML comments), and
base64/hex blobs that decode to imperative text. Matches are neutralized with
`[NENYA: INJECTION NEUTRALIZED]` by default; `strict` rejects the request with
`403 error_kind=injection_detected` and leaves the payload pristine for
forensics. `extra_patterns`/`ignore_patterns` tune the detector per
deployment; per-agent `enabled`/`strict` overrides are supported.

**Layer 3 — Spotlighting (Phase 030).** Untrusted content (Nenya-managed MCP
tool results, auto-search memory context, tool descriptions, and incoming
`role:"tool"` history) is enveloped in `<untrusted-content source="...">`
tags, and a one-time preamble rule tells the model that envelope content is
data, never instructions. Deterministic delimiters keep provider prompt-cache
prefixes stable; Nenya-managed results, memory context, and tool descriptions
are size-capped (512 KiB default) — the incoming-history path intentionally
does not truncate client history. This is the
primary defense for client-side MCP setups (opencode/crush), whose tool
results transit Nenya only as history.

**Layer 4 — Two-tier escalation (Phase 040).** Detections in the ambiguous
band (`[min_score, max_score)`) can be adjudicated by an LLM classifier
through the engine chain before the tier-1 verdict applies. The classifier
verdict is **advisory**: a clean verdict can clear surfaces, but malformed
output, engine failure, timeout, budget exhaustion, summarized traffic, and
benign-on-truncated-excerpt all fall back to the deterministic verdict.

**Layer 5 — Fail-closed chain semantics (Phase 070).** Security interceptors
(redaction, entropy, spotlight, injection) implement `Strict()`: an
operational fault aborts the request with `503 error_kind=internal_error`
(`pipeline.StrictError`) rather than forwarding unprocessed content. Token
saving interceptors (TF-IDF) keep fail-open behavior; the bouncer honors
`bouncer.fail_open`. Policy rejections (`pipeline.RejectError`) always render
`403` with their specific `error_kind`.

**Layer 6 — MCP dispatch guards (Phase 080).** Before any Nenya-managed
`CallTool` dispatch (model-initiated, auto-search, auto-save), arguments are
size-capped (1 MiB default) and URL-checked (`deny_private` rejects
loopback/private/link-local/metadata hosts in every spelling; non-routable
transition formats that embed an IPv4 address (RFC 2765 IPv4-translated, 6to4,
Teredo) are not claimed by the deny set; per-server `allowed_hosts` grants
exceptions). Registry-routed (model-initiated) calls
additionally validate arguments against the tool's declared `inputSchema`
(minimal subset: `type`, `enum`, `required`, `properties`, `items`) —
auto-search/auto-save have no declared schema and skip that check. A rejected
registry call returns a structured error result to the model so the loop can
correct itself; a rejected auto-search surfaces as a failed search and a
rejected auto-save falls back to the next server. The client response is
unaffected in every case.

## Configuration quick reference

| Knob                                                      | Default                                                                    | Effect                                                                             |
| --------------------------------------------------------- | -------------------------------------------------------------------------- | ---------------------------------------------------------------------------------- |
| `governance.injection.enabled`                            | `false`                                                                    | Deterministic prompt-injection detection and sanitization                          |
| `governance.injection.strict`                             | `false`                                                                    | Reject on detection (`403 error_kind=injection_detected`) instead of sanitizing    |
| `governance.injection.escalation.enabled`                 | `false`                                                                    | Two-tier classifier for ambiguous detections (tier-1 always applies on failure)    |
| `governance.injection.escalation.engine`                  | —                                                                          | Engine target for the classifier (prefer a **local** engine, see limitations)      |
| `governance.injection.escalation.max_bytes`               | `8192`                                                                     | Per-surface excerpt cap sent to the classifier (an egress event)                   |
| `governance.spotlight.enabled`                            | `false`                                                                    | Envelope untrusted Nenya-managed MCP/memory content                                |
| `governance.spotlight.history_enabled`                    | = `enabled`                                                                | Envelope incoming `role:"tool"` history (client-side MCP surface)                  |
| `governance.spotlight.max_tool_result_bytes`              | `524288`                                                                   | Tool-result size cap (truncation marker)                                           |
| `governance.exfil_guard.enabled`                          | `false`                                                                    | Output URL policy on markdown links/images and bare URLs                           |
| `governance.exfil_guard.action`                           | `log`                                                                      | `log`, `strip` (placeholder), or `block` (`403`/stream `error_kind=exfil_blocked`) |
| `governance.canary.enabled`                               | `false`                                                                    | Per-request tripwire marker watched on every egress channel                        |
| `governance.canary.action`                                | `block`                                                                    | `block` (`error_kind=exfil_detected`) or `log`                                     |
| `governance.mcp_guard.enabled`                            | `true` — the section is materialized only when `mcp_servers` is configured | Tool-call argument guard                                                           |
| `governance.mcp_guard.max_arg_bytes`                      | `1048576`                                                                  | Argument size cap                                                                  |
| `governance.mcp_guard.url_policy`                         | `deny_private`                                                             | `deny_private`, `log`, or `off`                                                    |
| `mcp_servers.<name>.allowed_hosts`                        | `[]`                                                                       | URL-policy exceptions (exact host or `*.suffix`)                                   |
| `bouncer.fail_open`                                       | `true`                                                                     | `false` rejects oversized requests when the engine fails                           |
| `agents.<name>.injection` / `.spotlight` / `.exfil_guard` | —                                                                          | Per-agent `enabled`/`strict`, `enabled`/`history_enabled`, `enabled`/`action`      |

Full field-level documentation lives in
[CONFIGURATION.md](CONFIGURATION.md#governance). Wired interceptors are listed
with their strict/fail-open semantics in
[ARCHITECTURE.md](ARCHITECTURE.md#registered-interceptors).

## Response-path controls

**ExfilGuard (Phase 050)** inspects model-produced content on both paths:
streaming SSE deltas through a sliding window (chunk-boundary safe) and
buffered bodies via full inspection. It flags markdown images/links and bare
`http(s)` URLs whose scheme, host (IP literals, allowlist), query length, or
query entropy violate policy. Actions are `log`, `strip`, or `block`.

**Canary tripwire (Phase 060)** injects a per-request random marker
(`NENYA-CANARY-` + 32 hex chars) as a system-context message _after_ the
content pipeline — never counted against token budgets, never redacted,
compacted, or summarized. Every egress channel is watched for its exact
appearance: the output stream, buffered bodies, and MCP tool-call arguments. A
detection means the model reproduced gateway-injected context into an egress
channel — the signature of successful injection-driven exfiltration. The
summarization retry path filters the marker from summarizer input and
re-appends it verbatim, so the tripwire cannot be silently disarmed and
summaries cannot self-trip it.

## Operational playbook

**Metrics to alert on:**

| Metric                                              | Meaning                                                                                                                       | Suggested action                                                                                  |
| --------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| `nenya_injection_detections_total{action,category}` | Detections by outcome and category                                                                                            | Alert on `action="reject"` spikes; watch `category="custom"` for tuning                           |
| `nenya_injection_escalations_total{verdict}`        | Tier-2 classifier outcomes                                                                                                    | Spike in `verdict="error"`/`inconclusive` means the classifier is degraded — tier-1 still applies |
| `nenya_interceptor_errors_total{name}`              | Interceptor operational faults (security interceptors then render 503)                                                        | Alert: a broken security layer is failing traffic closed                                          |
| `nenya_spotlighted_total{source}`                   | Enveloped surfaces                                                                                                            | Watch `source="tool-history"` growth (client-side MCP traffic)                                    |
| `nenya_exfil_detections_total{reason,action}`       | Output URL-policy hits (`reason` ∈ `scheme`, `ip_literal`, `private_ip`, `host_not_allowed`, `query_length`, `query_entropy`) | `query_entropy`/`query_length` spikes may indicate beacon exfil or noisy legitimate URLs          |
| `nenya_exfil_events_total{channel}`                 | Canary echoes (`stream`, `buffered`, `tool_args`)                                                                             | **Page-worthy**: non-zero means injected context reached an egress channel                        |
| `nenya_stream_blocked_total`                        | Streams terminated by a policy                                                                                                | Correlate with the exfil/canary metrics above                                                     |
| `nenya_mcp_tool_calls_total{status}`                | Tool dispatches incl. guard rejections                                                                                        | Rejection spikes may be an injection campaign or a too-strict schema                              |
| `nenya_auth_denials_total{reason}`                  | RBAC/endpoint denials                                                                                                         | Alert on `payload_too_large`/`invalid_body` (fail-closed paths)                                   |

**Rollout guidance (least disruptive → strictest):**

1. **Observe.** Enable detectors in non-destructive modes:
   `injection.enabled=true` (sanitizes), `exfil_guard.enabled=true` with
   `action=log`, `canary.enabled=true` with `action=log`,
   `spotlight.enabled=true`. Watch the metrics for a week of real traffic.
2. **Tune.** Raise `exfil_guard.max_query_chars` or add `allowed_hosts` for
   legitimate long-URL traffic (S3 presigned links, deep links). Add
   `injection.ignore_patterns` narrowly for false positives — one match
   disables every detector for that surface, so keep them specific.
3. **Enforce output.** Move `exfil_guard.action` to `strip`, then `block`.
   Set `canary.action=block` — but only if no gateway-routed agent reproduces
   system context (see the canary caveat below); otherwise keep `log`.
4. **Enforce input.** Set `injection.strict` per agent (start with agents that
   handle untrusted content), then `bouncer.fail_open=false` where an
   unsummarized oversized request must not reach upstream.
5. **Escalate.** Add `injection.escalation` with a **local** engine target to
   reduce false positives on ambiguous detections.

**Canary caveat:** the tripwire fires on _any_ verbatim echo of the marker,
including benign ones. Agents whose job is reproducing or summarizing system
context (compaction/summarization agents routed through Nenya) will trip
repeatedly, and `action=block` hard-fails those requests. Run the canary in
`log` mode globally, or exclude such agents from the workflow.

## Client composition

Nenya's stack covers what the gateway can see. Client-side defenses cover the
rest — understand the seam before relying on either:

| Surface                   | Client-side MCP (opencode/crush)                                                                                                 | Nenya-managed MCP                                             |
| ------------------------- | -------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------- |
| Tool result content       | Produced by the client's MCP server; transits Nenya only as `role:"tool"` history → **spotlighting + injection detection apply** | Enveloped at injection time (spotlight) plus history wrapping |
| Tool-call arguments       | Client dispatches directly to its MCP server — **Nenya does not see or guard them**                                              | Guarded (schema, size, URL policy)                            |
| Auto-search / auto-save   | n/a (client features)                                                                                                            | Guarded, memory context enveloped                             |
| Model output              | Transits Nenya → ExfilGuard + canary apply                                                                                       | Same                                                          |
| Upstream request/response | Redaction, entropy, bouncer, injection detection, compaction                                                                     | Same                                                          |

**Why the client-side gap matters:** the deterministic defenses on tool
_results_ (history) are strong, but a poisoned result that instructs the model
to call a _client-side_ tool with private data is not intercepted, because the
tool call never traverses Nenya.

Upstream reports motivating the layered approach (each a distinct facet, not a
single root cause): `anomalyco/opencode#36682` (compaction-time instruction
injection), `anomalyco/opencode#49545` (tool-output prompt injection),
`anomalyco/opencode#49975` (fail-closed hardening for its shell classifier).
None demonstrates the client-side tool-argument gap directly — that gap is
architectural, not a reported incident. Reducing risk: keep untrusted tool
servers on Nenya-managed MCP where the argument guard applies, and treat
client-side MCP servers as trusted-by-construction.

## Honest limitations

The stack is deliberately documented with its blind spots; a defense you
over-trust is worse than one you understand.

- **Canary is an exact-substring tripwire.** Encoded dumps (base64/hex) of the
  marker evade detection. Streaming tool-call argument deltas are not watched
  (the buffered tool-args path is). Log-mode scrubbing applies only to
  buffered paths and is best-effort per frame; an instructed model can alter
  the marker before echoing it. It detects _some_ exfiltration — it does not
  prove the absence of it.
- **Escalation is itself an egress event.** The tier-2 classifier excerpt
  (up to `max_bytes`, default 8 KiB) leaves the gateway for the configured
  engine. Use a **local** engine (e.g. Ollama) as the escalation target when
  content sensitivity demands it; a cloud classifier target means the excerpt
  transits that provider.
- **ExfilGuard is URL-shaped.** Bare URLs are evaluated once terminated by
  whitespace/delimiter — a violating URL as the literal final token of a
  stream escapes. Streaming tool-call argument deltas are not guarded (the
  buffered path is). STREAMING tool-argument fragments bypass; encoded or
  obfuscated destinations (punycode lookalikes, DNS that resolves privately)
  are not detected. MCP argument URL checks are string-only: DNS names that
  resolve to private addresses and DNS rebinding between check and fetch are
  not caught.
- **Injection detection is surface-local.** A payload split across adjacent
  content-array parts is not reassembled. The sanitize marker is a spoofable
  literal — detection counters, not the marker, are the source of truth.
  ZWNJ/ZWJ (U+200C/U+200D) are exempt from hidden-text detection because they
  are legitimate typography.
- **Spotlighting reduces, not eliminates.** Envelopes + preamble lower
  indirect-injection success rates substantially; a sufficiently persuasive
  payload can still influence a model that treats the envelope as content.
- **MCP multi-turn loops bypass the interceptor chain.** Tool results appended
  during the loop are not re-scanned by redaction/entropy/injection or the
  bouncer; spotlighting and the canary/exfil egress defenses do cover them.
- **Secret redaction is best-effort.** Regex presets and entropy heuristics
  miss novel formats; treat them as friction, not a guarantee.
- **Fail-closed means downtime.** When a strict security interceptor faults,
  requests fail with 503 rather than proceeding degraded. Alert on
  `nenya_interceptor_errors_total` and fix the underlying fault.

## Research and references

- Microsoft, **Spotlighting**: "Defending Against Indirect Prompt Injection
  Attacks With Spotlighting" — arXiv:2403.14720. Basis for the envelope +
  datamarking design (delimiters mode used for history; datamarking only for
  Nenya-managed results).
- Debenedetti et al., **CaMeL**: "Defeating Prompt Injections by Design" —
  arXiv:2503.18813. Design influence: capability/data-flow separation, and the
  principle that untrusted data must not be able to direct privileged actions.
- Simon Willison, **the lethal trifecta** — untrusted content + private data +
  egress. The organizing threat model for this stack.
- OWASP **Top 10 for LLM Applications** — LLM01 (Prompt Injection), LLM02
  (Sensitive Information Disclosure), LLM05 (Improper Output Handling), LLM06
  (Excessive Agency).
- OWASP **LLM Prompt Injection Prevention Cheat Sheet** — layered input/output
  handling guidance.
- Upstream client reports (layered-defense motivation, not one root cause):
  `anomalyco/opencode#36682` (compaction-time injection),
  `anomalyco/opencode#49545` (tool-output injection),
  `anomalyco/opencode#49975` (shell-classifier fail-closed hardening).
