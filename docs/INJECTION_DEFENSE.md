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
| Local summaries   | Window compaction summaries, bouncer rewrites, context-limit retry summaries re-enter the conversation | A summary that carries (or is steered into carrying) an action plan is executed by the model on the next turn (opencode #36682) |

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
benign-on-truncated-excerpt all fall back to the deterministic verdict. The
classifier now runs on the shared **Judge primitive** (`pipeline.Judge`,
contract `"injection"`) rather than a bespoke call path — behavior is
unchanged (same prompt, enum, budgets, and fallbacks), but it emits the common
judgment metrics and is engine-agnostic like every other judgment site.

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

**Layer 7 — Locally generated summaries (NENYA-148).** Nenya generates summary
text locally (window compaction, the bouncer's redaction rewrite, the
context-limit retry summarizer) and that text re-enters the conversation, so it
is model-authored content derived from untrusted conversation history. Upstream
evidence: opencode #36682 (a compaction summary executed its own "Next Move"
section as user instructions, triggering an unsolicited `ssh-keyscan` with
`StrictHostKeyChecking=no`), #32634, #13838, #28063. Hardening:

- **Envelope + preamble.** Every model-generated summary is wrapped in the
  untrusted-content delimiters (`<untrusted-content source="summary:window">`
  for window compaction, `summary:context-limit` for the retry summarizer) after
  `SummaryPreamble`, which tells the model the text is machine-produced data and
  not to act on any imperative text inside it. Embedded envelope tags in a
  summary are defanged, so a summary cannot close its own envelope. There is no
  "already enveloped?" content check: the content is attacker-influenced, so a
  summary that begins with the preamble is still enveloped (a sniff would
  silently disable the primary defense). Only model output is enveloped: a deterministic head (truncate/tfidf mode, or the truncation
  fallback) is the client's own history and is inserted verbatim.
- **Action plans stripped at insertion time.** An action-plan heading (next
  steps/moves/actions, action items/plan/steps, to-do, follow-ups, remaining
  work — whole-line or inline like "Next steps: run the migration") is removed
  with the list or paragraph that follows it; the generic words (tasks,
  recommendations, instructions) only count when the whole line is the heading.
  An inline heading counts when what follows it is an instruction, decided
  FAIL-SAFE: anything that is not a recognizable description (a closed set of
  descriptive leads — "the user will run the migration" describes) is treated as
  a plan, so an unlisted command ("exfiltrate …", "base64 …", "wget … && bash")
  is still removed. A summary that was nothing but a plan is replaced by an
  explicit removal marker instead of being re-inserted. All three summarizer
  prompts additionally require descriptive prose (the current task described
  neutrally, never as an instruction).
- **Role discipline.** A summary is never injected as a `user` turn: the window
  summary is a `system` context marker and the context-limit summary is an
  `assistant` context marker (its natural role — a summary of what happened),
  with the agent's leading system messages preserved verbatim so a summary
  cannot displace the operator's instructions. The window's synthetic
  continuation cue was reworded to a descriptive marker
  (`[Resumed context: ...]`) so no locally generated text is imperative.
- **Rewrite guard (bouncer).** The bouncer's local engine rewrites the caller's
  OWN message in place, so the role must stay as it is — the request must remain
  a request. Instead the rewrite is checked against its input: an action-plan
  section the source text did not contain is removed (the signature of a
  prompt-injected local model), while the caller's own imperative request
  survives untouched.

_Honest limitations:_ the envelope relies on the model honoring the preamble
(like spotlighting, it is a mitigation, not a sandbox); the stripper is a
deterministic heuristic over headings and list bodies, so a summary can still
*describe* an instruction in prose ("the user asked to rotate the key") — that
is data the model should read as history, not execute — and a plan rephrased
without any recognizable heading (plain prose imperative) is not removed. The
two sub-rules fail in opposite, deliberate directions: the INLINE rule is
fail-safe (anything that is not a recognizable description is stripped, so the
residual risk is a description that starts like an instruction losing its tail,
never an instruction that survives), while the PARAGRAPH rule uses a positive
imperative signal (an unrecognized descriptive paragraph is kept, so a
legitimate summary is never emptied to the removal marker); a
custom `system_prompt` that disables the descriptive-output rule still benefits
from the envelope and the stripper, but the prompt rule is the cheapest lever;
the bouncer's output keeps the caller's role by necessity, so its hardening is
the `[Nenya Sanitized via Ollama]` provenance marker plus the
introduction guard (an untrusted-content envelope would mislabel the caller's
own request as external data), and an engine that returns nothing but a plan is
treated as an engine failure (fail-open skips the rewrite, fail-closed rejects);
and the tool-prune placeholder (a locally generated `assistant` message whose
label is client-controlled) has its label whitespace-collapsed with quote
characters dropped, while the
stream-continuation replay carries the model's own partial output and is
deliberately not enveloped.

## Configuration quick reference

| Knob                                                      | Default                                                                    | Effect                                                                                                                                                                                                  |
| --------------------------------------------------------- | -------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `governance.injection.enabled`                            | `false`                                                                    | Deterministic prompt-injection detection and sanitization                                                                                                                                               |
| `governance.injection.strict`                             | `false`                                                                    | Reject on detection (`403 error_kind=injection_detected`) instead of sanitizing                                                                                                                         |
| `governance.injection.escalation.enabled`                 | `false`                                                                    | Two-tier classifier for ambiguous detections (tier-1 always applies on failure)                                                                                                                         |
| `governance.injection.escalation.engine`                  | —                                                                          | Engine target for the classifier (prefer a **local** engine, see limitations)                                                                                                                           |
| `governance.injection.escalation.max_bytes`               | `8192`                                                                     | Per-surface excerpt cap sent to the classifier (an egress event)                                                                                                                                        |
| `governance.spotlight.enabled`                            | `false`                                                                    | Envelope untrusted Nenya-managed MCP/memory content                                                                                                                                                     |
| `governance.spotlight.history_enabled`                    | = `enabled`                                                                | Envelope incoming `role:"tool"` history (client-side MCP surface)                                                                                                                                       |
| `governance.spotlight.max_tool_result_bytes`              | `524288`                                                                   | Tool-result size cap (truncation marker)                                                                                                                                                                |
| `governance.spotlight.risk_tiers.enabled`                 | `false` (blanket delimiters)                                               | Risk-tiered history marking: markers → high (datamarking), local/plain → low, ambiguous → advisory `spotlight_tier` judgment failing closed to high; metrics `nenya_spotlight_tiers_total{tier,source}` |
| `governance.exfil_guard.enabled`                          | `false`                                                                    | Output URL policy on markdown links/images and bare URLs                                                                                                                                                |
| `governance.exfil_guard.action`                           | `log`                                                                      | `log`, `strip` (placeholder), or `block` (`403`/stream `error_kind=exfil_blocked`)                                                                                                                      |
| `governance.canary.enabled`                               | `false`                                                                    | Per-request tripwire marker watched on every egress channel                                                                                                                                             |
| `governance.canary.action`                                | `block`                                                                    | `block` (`error_kind=exfil_detected`) or `log`                                                                                                                                                          |
| `governance.mcp_guard.enabled`                            | `true` — the section is materialized only when `mcp_servers` is configured | Tool-call argument guard                                                                                                                                                                                |
| `governance.mcp_guard.max_arg_bytes`                      | `1048576`                                                                  | Argument size cap                                                                                                                                                                                       |
| `governance.mcp_guard.url_policy`                         | `deny_private`                                                             | `deny_private`, `log`, or `off`                                                                                                                                                                         |
| `mcp_servers.<name>.allowed_hosts`                        | `[]`                                                                       | URL-policy exceptions (exact host or `*.suffix`)                                                                                                                                                        |
| `bouncer.fail_open`                                       | `true`                                                                     | `false` rejects oversized requests when the engine fails                                                                                                                                                |
| `agents.<name>.injection` / `.spotlight` / `.exfil_guard` | —                                                                          | Per-agent `enabled`/`strict`, `enabled`/`history_enabled`, `enabled`/`action`                                                                                                                           |

Full field-level documentation lives in
[CONFIGURATION.md](CONFIGURATION.md#governance). Wired interceptors are listed
with their strict/fail-open semantics in
[ARCHITECTURE.md](ARCHITECTURE.md#registered-interceptors).

## Response-path controls

**ExfilGuard (Phase 050)** inspects model-produced content on both paths:
streaming SSE deltas through a sliding window (chunk-boundary safe) and
buffered bodies via full inspection. It flags markdown images/links and bare
`http(s)` URLs whose scheme, host (IP literals, allowlist), query length, or
query entropy violate policy. The host is normalized before matching
(lowercased, every trailing dot removed, IPv6 zone cut) and IP literals are
recognized in every spelling an OS resolver accepts, including the legacy
integer forms (`2130706433`, `0x7f000001`, `0177.0.0.1`, `127.1`); the
private/link-local/CGNAT/NAT64 deny set is shared with the MCP argument policy
(`internal/netutil`), and a special-use hostname (`localhost`, `.local`,
`.internal`) is reported as `private_host` instead of passing as an ordinary
hostname. Actions are `log`, `strip`, or `block`. The MCP tool-argument policy
(NENYA-147) applies the same deny set to every whitespace-separated token of a
string argument — not just the first — and accepts any `scheme://` and the
scheme-relative `//host` form, plus a destination-shaped bare token
(an IP literal anywhere, or a special-use name with an explicit port or path —
a special-use name in plain prose is not a destination).

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

| Metric                                              | Meaning                                                                                                                       | Suggested action                                                                                         |
| --------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `nenya_injection_detections_total{action,category}` | Detections by outcome and category                                                                                            | Alert on `action="reject"` spikes; watch `category="custom"` for tuning                                  |
| `nenya_injection_escalations_total{verdict}`        | Tier-2 classifier outcomes (truncated-excerpt benigns recorded as `inconclusive`)                                             | Spike in `verdict="error"`/`inconclusive` means the classifier is degraded — tier-1 still applies        |
| `nenya_judgments_total{judgment,verdict,engine}`    | Judgment-layer outcomes; the injection classifier also appears here as `judgment="injection"` (truncated benigns as `benign`) | Same degradation signal as the escalation family; `engine` label shows which adjudicating model answered |
| `nenya_interceptor_errors_total{name}`              | Interceptor operational faults (security interceptors then render 503)                                                        | Alert: a broken security layer is failing traffic closed                                                 |
| `nenya_spotlighted_total{source}`                   | Enveloped surfaces                                                                                                            | Watch `source="tool-history"` growth (client-side MCP traffic)                                           |
| `nenya_exfil_detections_total{reason,action}`       | Output URL-policy hits (`reason` ∈ `scheme`, `ip_literal`, `private_ip`, `private_host`, `host_not_allowed`, `query_length`, `query_entropy`, `llm_screen`) | `query_entropy`/`query_length` spikes may indicate beacon exfil or noisy legitimate URLs; a `private_host` shift is NENYA-147 moving localhost/`.internal`/`.local` out of `host_not_allowed` |
| `nenya_exfil_events_total{channel}`                 | Canary echoes (`stream`, `buffered`, `tool_args`)                                                                             | **Page-worthy**: non-zero means injected context reached an egress channel                               |
| `nenya_stream_blocked_total`                        | Streams terminated by a policy                                                                                                | Correlate with the exfil/canary metrics above                                                            |
| `nenya_mcp_tool_calls_total{status}`                | Tool dispatches incl. guard rejections                                                                                        | Rejection spikes may be an injection campaign or a too-strict schema                                     |
| `nenya_auth_denials_total{reason}`                  | RBAC/endpoint denials                                                                                                         | Alert on `payload_too_large`/`invalid_body`/`missing_model`/`invalid_expiry` (fail-closed paths; `missing_model` is split out of `invalid_body` so a request that omits the model — and would otherwise skip agent scoping — is visible on its own)                                          |

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
  _Closed by the NENYA-147 variant audit:_ the destination is now normalized
  before matching (lowercased, every trailing dot removed, IPv6 zone cut) and
  the legacy integer IPv4 spellings (`2130706433`, `0x7f000001`, `0177.0.0.1`,
  `127.1`) are recognized as addresses in both the ExfilGuard and the MCP
  argument policy, through one shared helper (`internal/netutil`); the deny set
  now also covers CGNAT `100.64/10`, multicast/broadcast, and the NAT64
  `64:ff9b::/96` embedding when IP literals are permitted. The MCP argument
  policy additionally checks every whitespace-separated token (not just the
  first, so a URL embedded in a command line is seen), any scheme (not only
  http/https), the scheme-relative `//host` form, and a value that is itself a
  denied bare host.
  _Policy note:_ a scheme-less token that merely *contains* `://` is not
  treated as a URL: a scheme-prefixed URL anywhere in the token (a flag
  assignment, a markdown link, a quoted string) is extracted and judged, and a
  URL-shaped-but-host-less form (e.g. `file:///etc/passwd`) fails closed as
  `malformed`. A standalone numeric value needs a complete dotted IPv4 (four labels) or an
  explicit `0x` prefix to count as an address, so ordinary
  `8080`/`2025`/`0644`/`10.0`/`10.0.0` arguments are not rejected (URL hosts keep
  the browser-style interpretation, so `http://2130706433/` is still an
  address), and a bare single-label `localhost` counts as a
  destination only when it is the whole value or carries a port/path (prose
  that mentions the word is not rejected; a prose token that *is* an IP literal,
  or a special-use name with an explicit port or path, is rejected).
  _Still out of scope (neither closed nor a live bypass):_ the RFC 2765
  IPv4-translated `::ffff:0:0/96` form, 6to4 `2002::/16`, Teredo `2001::/32`,
  allowlist wildcards other than `*.` prefixes in the ExfilGuard (the MCP policy
  supports them), a non-URL-shaped destination (a bare public hostname), and the
  residual string shapes `https:/10.0.0.5/x` / `http:\10.0.0.5\x` (curl and
  fetch treat them as a path, not an authority) and a ZWSP-split host (a
  resolver will not resolve the ZWSP-bearing name — consistent with the
  string-only limitation).
- **Injection detection is surface-local.** A payload split across adjacent
  content-array parts is not reassembled. The sanitize marker is a spoofable
  literal — detection counters, not the marker, are the source of truth.
  ZWNJ/ZWJ (U+200C/U+200D) are exempt from hidden-text detection because they
  are legitimate typography.
  _Closed by the NENYA-147 variant audit:_ before matching, the surface is
  normalized — ANSI/CSI escape sequences, C0 control characters (including a
  lone ESC and DEL), the extended invisible/tag characters (soft hyphen,
  ZWNJ/ZWJ, Hangul fillers, variation selectors, the Unicode tag block
  U+E0000-U+E007F), and the hidden-text set (ZWSP, LRM/RLM, bidi overrides, BOM)
  are each removed **and** replaced by a space — two candidates, because such a
  character can hide either a missing separator or a split word — then fullwidth
  forms (U+FF01-U+FF5E) are folded to ASCII and the non-ASCII space separators
  (NBSP, U+2028/U+2029, U+3000, …) collapse to a plain space. When the
  invisible-character strip changed the surface, the pre-strip form is
  normalized too (a separator hidden by a stripped character is only restored
  from the original). A candidate is adopted only when it reveals a new pattern
  or re-neutralizes a span of an already-counted one, so benign content is never
  rewritten by normalization alone, and the legacy hidden-text strip keeps its
  deliberately narrow set.
  _Still out of scope:_ full NFKC, homoglyphs outside the fullwidth block
  (mathematical alphanumerics, dotted capital I, script/fraktur lookalikes),
  UTF-16 payloads, and literal `\uXXXX` escape text — all pinned as
  `detected: false` cases in the committed corpus
  (`internal/pipeline/testdata/injection_variants.json`).
- **Spotlighting reduces, not eliminates.** Envelopes + preamble lower
  indirect-injection success rates substantially; a sufficiently persuasive
  payload can still influence a model that treats the envelope as content.
- **MCP multi-turn loop re-scans are deterministic-tier only.** Tool results
  appended during the loop are re-scanned by the deterministic security
  subset (redaction, entropy, tier-1 injection — NENYA-136) but not by the
  token-economy stages or the tier-2 injection escalator; the bouncer never
  re-runs mid-conversation. Spotlighting and the canary/exfil egress
  defenses also cover appended content.
- **Secret redaction is best-effort.** Regex presets and entropy heuristics
  miss novel formats; treat them as friction, not a guarantee.
- **Fail-closed means downtime.** When a strict security interceptor faults,
  requests fail with 503 rather than proceeding degraded. Alert on
  `nenya_interceptor_errors_total` and fix the underlying fault.

## Adversarial variant corpus (NENYA-147)

The tier-1 gates are audited against the upstream bypass taxonomy with committed
corpora, so every shape has a test rather than a claim:

| Surface | Corpus | Test |
|---|---|---|
| ExfilGuard URL policy | `internal/stream/testdata/exfil_url_variants.json` | `TestExfilGuardVariantTaxonomy` |
| Tier-1 injection patterns | `internal/pipeline/testdata/injection_variants.json` | `TestInjectionVariantTaxonomy` |
| MCP argument URL policy | `internal/mcp/testdata/tool_arg_url_variants.json` | `TestToolArgURLVariantTaxonomy` |
| Destination deny set (shared) | `internal/netutil/ipdeny_test.go` (policy boundary) | `TestParseStandaloneIPv4`, `TestParseNumericIPv4URLHost`, `TestDenyHostNormalization` |
| Canary tripwire | prose only (CONFIGURATION.md) | no fixture: the marker is per-request random, so a fixture could only pin the documented non-detections (encoded dumps, streaming tool-arg deltas) |

A case whose evasion is a documented limitation is committed with the
non-detecting expectation, so a future change that closes it must update the
corpus (and cannot silently claim coverage it does not have). Each gate also has
a variant fuzz target: `FuzzExfilURLVariantScan`, `FuzzNormalizeForMatching`,
and `FuzzDenyURLHost`.

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
