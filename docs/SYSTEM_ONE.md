# System One Decision Models (Jev)

> Status: **recon / spec-first**. This document captures the validated wire
> contract for TypeSafe's System One decision models (Jev 1.13) as reachable
> through OpenCode Zen, and the Nenya integration paths. The first-class
> endpoint described under [Integration paths](#integration-paths) is planned
> (Plane module *System One Decision Models Adoption*).

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

| Model ID | Access with an OpenCode **Go** key |
|----------|-----------------------------------|
| `jev-1.13-free` | ✅ works (limited-time free model) |
| `jev-1.13` | ❌ `403` `Model access is disabled` |

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
| model not permitted on the plan | `403` `{"error":{"type":"server_error","message":"Upstream request failed: Model access is disabled"}}` |
| paid model on a Go key | `403` as above |
| `x-opencode-session` | not required on `/zen/v1/systemone` |

Note: Zen uses `400` for request validation, whereas TypeSafe's own docs
describe `422`. Treat the status class, not the literal code, as the contract.

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

### 2. First-class endpoint (planned)

`POST /v1/systemone` — Nenya authenticates with the client token, resolves the
provider's System One URL, injects the provider key, relays the non-streaming
JSON response, and records usage. See the Plane module for phases.

Consumers that speak the System One contract can point at either path. For
example, `@jkudish/jev-mcp` in compatible mode:

```
JEV_PROVIDER=compatible
JEV_API_BASE_URL=http://127.0.0.1:8080/proxy/zen/zen/v1/systemone
JEV_API_KEY=<nenya client token>
JEV_MCP_MODEL=jev-1.13-free
```

### 3. Discovery/routing guard (planned)

Zen's `/v1/models` lists `jev-1.13` and `jev-1.13-free` alongside chat models
and carries no modality field, so dynamic discovery currently advertises them
as chat models. A config-driven non-chat model marker will exclude them from
`/v1/models` and make chat requests fail fast with a structured
`error_kind=invalid_request` instead of a doomed upstream call. The marker must
not affect the `/proxy/` or `/v1/systemone` paths.

## Limitations

- Jev cannot generate text, code, summaries, or explanations — it is not a
  chat/agent model and cannot replace the coding model.
- No streaming; a single JSON response per request.
- Output tokens are free; input tokens are billed (except the free model).
- TypeSafe documents weak spots: literal reading, arithmetic/counting, date
  ordering, multi-hop indirection, context rot on large irrelevant state, and
  adversarial state (it does not treat `state` as hostile).
