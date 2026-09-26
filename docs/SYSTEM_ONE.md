# System One Decision Models (Jev)

> Status: **shipped**. Nenya supports TypeSafe System One decision models
> (Jev 1.13) through a first-class `POST /v1/systemone` endpoint, with
> discovery/routing guards and usage metrics. This document captures the
> validated wire contract as reachable through OpenCode Zen and the Nenya
> integration paths.

## What Jev is

Jev is a **System One decision model**, not a chat model. It emits no prose,
no reasoning, and no tool calls. You POST a `state` (the text to judge) plus a
map of typed `questions`; it returns one typed `answers` entry per question.

Three question primitives:

| Type | Asks | Returns |
|------|------|---------|
| `noul` | "does this condition hold?" | `noul`: probability the answer is yes (0..1) |
| `choice` | "which of these options?" | `choice`: the winning option, `probabilities` per option, `confidence` |
| `score` | "where on this ordered scale?" | `score`: probability-weighted level, `legend`, `probabilities`, `confidence` |

Constraints: text-only input; 64k total context (32k for `state` + the longest
question); up to 255 choice options; 2–10 score levels; **no streaming**.

## Validated transport (OpenCode Zen)

Nenya's built-in `zen` provider targets `https://opencode.ai/zen/v1/...`. The
System One endpoint is:

```
POST https://opencode.ai/zen/v1/systemone
Authorization: Bearer <opencode zen/go key>
Content-Type: application/json
```

Model IDs served by Zen:

| Model ID | Billing | Access |
|----------|---------|--------|
| `jev-1.13-free` | none (limited-time free model) | ✅ works with any OpenCode key, no Zen balance |
| `jev-1.13` | Zen credits (pay-as-you-go) | requires a funded Zen balance; an unfunded account returns `402 Insufficient account funds` |

**Jev is billed against Zen credits, not the OpenCode Go subscription.** The Go
subscription funds the Go model list (served at `/zen/go/v1/*`); Jev is not in
that list and `/zen/go/v1/systemone` returns `400 Model is unavailable`. A Go
plan with no Zen balance can therefore call `jev-1.13-free` but not `jev-1.13`.

Model access is also a workspace setting: a disabled entitlement surfaces as
`403 Model access is disabled`, an enabled-but-unfunded paid model as
`402 Insufficient account funds`. Nenya relays both statuses verbatim (no
retry — only 5xx are retried), so the client sees the real cause.

The newer Go endpoint (`/zen/go/v1/systemone`) does **not** serve Jev: it
returns `400 Model is unavailable` even with a session header.

### Request

```json
{
  "model": "jev-1.13-free",
  "state": "My payments have failed for three days and I am losing sales.",
  "questions": {
    "is_urgent": {
      "type": "noul",
      "instructions": "Does this request require urgent attention?"
    },
    "department": {
      "type": "choice",
      "instructions": "Which team should handle this request?",
      "criteria": {
        "returns": "Refunds, exchanges, or damaged items",
        "billing": "Charges, invoices, or payment problems"
      }
    },
    "frustration": {
      "type": "score",
      "instructions": "How frustrated is the customer?",
      "criteria": ["Calm", "Frustrated", "Very angry"]
    }
  }
}
```

### Response

```json
{
  "model": "jev-1.13-free",
  "answers": {
    "is_urgent": { "type": "noul", "noul": 0.94 },
    "department": {
      "type": "choice",
      "choice": "returns",
      "confidence": 1,
      "probabilities": { "returns": 1, "billing": 0 }
    },
    "frustration": {
      "type": "score",
      "score": 0.99,
      "confidence": 0.98,
      "legend": { "0": "Calm", "1": "Frustrated", "2": "Very angry" },
      "probabilities": { "0": 0.01, "1": 0.99, "2": 0 }
    }
  },
  "usage": { "input_tokens": 393, "output_tokens": 54 }
}
```

Answers come back keyed by the same ids supplied in `questions`. `usage`
reports token counts; input tokens are billed (output tokens are free for Jev).

### Errors and edge cases (observed via Zen)

| Condition | Result |
|-----------|--------|
| missing/invalid key | `401` |
| malformed question (validation) | `400` with `{"detail": "..."}` |
| `stream: true` | `400` `{"detail":{"error_type":"api_usage_error","message":"Invalid request."}}` |
| model access disabled for the workspace | `403` `{"error":{"type":"server_error","message":"Upstream request failed: Model access is disabled"}}` |
| paid model enabled but account unfunded | `402` `{"error":{"type":"server_error","message":"Upstream request failed: Insufficient account funds"}}` |
| `x-opencode-session` | not required on `/zen/v1/systemone` |

Note: Zen uses `400` for request validation, whereas TypeSafe's own docs
describe `422`. Treat the status class, not the literal code, as the contract.
Nenya relays `4xx` verbatim and does not retry it; only `5xx` and transport
errors are retried.

## Integration paths

### 1. Generic passthrough (works today, no code)

Nenya's `/proxy/{provider}/{path}` handler forwards any method/body to
`provider.BaseURL + "/" + path`, injecting the provider key. `BaseURL` is the
host-only derivation of `providers.zen.url`
(`https://opencode.ai/zen/v1/chat/completions` → `https://opencode.ai`), so the
passthrough path includes the `zen` prefix:

```
POST /proxy/zen/zen/v1/systemone
Authorization: Bearer <nenya client token>
```

```bash
curl -sS http://127.0.0.1:8080/proxy/zen/zen/v1/systemone \
  -H "Authorization: Bearer $NENYA_CLIENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"jev-1.13-free","state":"...","questions":{...}}'
```

Requires only `provider_keys.zen` in the secrets file. Limits: bypasses the
content pipeline, circuit breaker, and per-model token accounting; it is
RBAC-gated as `/proxy/*`.

**Validated** against a local Nenya build: the request above returns `200`
with the answers relayed verbatim; a wrong client token returns `403`
`error_kind=auth_failed` (so the client `Authorization` is replaced by the
provider key, never forwarded); `/statsz` records `proxy:zen.requests` and
`proxy:zen.errors`, but `input_tokens`/`output_tokens` stay `0` — the
passthrough path does not account for decision usage.

### 2. First-class endpoint (shipped)

`POST /v1/systemone` — Nenya authenticates with the client token, resolves the
provider's System One URL, injects the provider key, relays the non-streaming
JSON response, and records usage.

```bash
curl -sS http://127.0.0.1:8080/v1/systemone \
  -H "Authorization: Bearer $NENYA_CLIENT_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"jev-1.13-free","state":"...","questions":{...}}'
```

Behavior:

- **Auth/RBAC:** the Nenya client token gates the route like any other
  `/v1/*` endpoint; `user` and `admin` keys are allowed (POST). A missing token
  returns `401`, an unauthorized key `403`.
- **Resolution:** System One models are absent from the chat catalog
  (`non_chat_models`), so the handler resolves to the provider that classifies
  the model as non-chat and declares a `format_urls.systemone` endpoint. A
  provider without one fails closed (`400` `error_kind=model_not_found`).
- **Provider key:** injected upstream; the client token is never forwarded.
- **Body:** `http.MaxBytesReader` cap, `state` and `questions` forwarded
  verbatim (the body is not an OpenAI chat shape).
- **Errors:** upstream 4xx is relayed with its status; 5xx and transport errors
  are retried (`util.DoWithRetryResp`) and surface as `502`
  `error_kind=network_error` on exhaustion.
- **Usage:** input/output tokens are recorded from the response `usage` object
  (`/statsz` per-model counters, `nenya_tokens_estimated_total{direction}`), and
  the request is counted in `nenya_decisions_total{model,provider}`.

Consumers that speak the System One contract can point at either path. For
example, `@jkudish/jev-mcp` in compatible mode:

```
JEV_PROVIDER=compatible
JEV_API_BASE_URL=http://127.0.0.1:8080/v1/systemone
JEV_API_KEY=<nenya client token>
JEV_MCP_MODEL=jev-1.13-free
```

### 3. Discovery/routing guard (shipped)

Zen's `/v1/models` lists `jev-1.13` and `jev-1.13-free` alongside chat models
and carries no modality field, so a config-driven marker classifies them:
`providers.zen.non_chat_models = ["^jev-"]` (built-in default for `zen`). Non-chat
models are excluded from the merged catalog and `/v1/models`, and chat requests
naming one fail fast with `400 error_kind=invalid_request`. The marker never
affects `/proxy/` or `/v1/systemone`.

```json
{
  "providers": {
    "zen": {
      "url": "https://opencode.ai/zen/v1/chat/completions",
      "non_chat_models": ["^jev-"]
    }
  }
}
```

See [`docs/CONFIGURATION.md`](CONFIGURATION.md#providers) for the field
reference and [`docs/PROVIDERS.md`](PROVIDERS.md#opencode-zen) for the Zen
decision endpoint.

## Limitations

- Jev cannot generate text, code, summaries, or explanations — it is not a
  chat/agent model and cannot replace the coding model.
- No streaming; a single JSON response per request.
- Output tokens are free; input tokens are billed (except the free model).
- TypeSafe documents weak spots: literal reading, arithmetic/counting, date
  ordering, multi-hop indirection, context rot on large irrelevant state, and
  adversarial state (it does not treat `state` as hostile).
