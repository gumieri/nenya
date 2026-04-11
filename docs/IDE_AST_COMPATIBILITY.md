# IDE / AST Compatibility

## Status

Fully implemented (all 3 phases).

## Decisions

- **Redaction**: Skip regex redaction inside code fences (markdown ` ``` ` blocks); still redact secrets in prose/documentation outside code blocks
- **Target clients**: Cursor and OpenCode (extensible via pattern registry in `internal/pipeline/client.go`)
- **Zero regression**: All IDE-specific behavior gated behind `ClientProfile.IsIDE` — unknown clients get standard pipeline

## What Changed

### Client Classification (`internal/pipeline/client.go`)

`ClassifyClient(headers http.Header)` inspects `User-Agent` and returns `ClientProfile{IsIDE, ClientName}`. Pattern registry is extensible.

### Code Fence Detection (`internal/pipeline/code_detect.go`)

`DetectCodeFences(text)` returns `[]CodeSpan{Start, End, Language}` for markdown fenced code blocks. Used by redaction and summarization.

### IDE-Aware Pipeline Behavior

| Stage | Non-IDE | IDE (Cursor, OpenCode) |
|-------|---------|----------------------|
| **Secret redaction** | `RedactSecrets` — regex on entire text | `RedactSecretsPreservingCodeSpans` — skips code fences, redacts prose only |
| **Text compaction** | `ApplyCompaction` — collapse blank lines, trim whitespace | **Skipped** — preserves whitespace and line refs |
| **Truncation** | `TruncateMiddleOut` — character-boundary | `TruncateMiddleOutCodeAware` — snaps to blank-line boundaries |
| **Engine prompt** | Generic privacy filter | Code-preserving prompt — only redacts secrets in prose, never restructures code |
| **Stream filter** | Filters `delta.content` | No change needed — tool call chunks already excluded (no `content` field) |

### Structured Content (`internal/gateway/gateway.go`)

`ExtractContentText` now handles:
- `{type: "text"}` — concatenated (unchanged)
- `{type: "image_url"}` → `[image]` placeholder for token counting
- `{type: "input_json"}` → serialized JSON for token counting

### Tool Call Safety

- `tool_calls`, `tool_call_id`, `function_call` pass through `SanitizePayload` unmodified (verified via `TestSanitizePayload_ToolCallsPassthrough`)
- Cache fingerprints correctly differentiate payloads with tool_calls vs tools param (verified via `TestFingerprintPayload_ToolCallMessages`)
- Stream filter already skips tool call chunks (only processes `delta.content`, which is empty for tool call deltas)

## Files

| File | Status |
|---|---|
| `internal/pipeline/client.go` | New — client classifier |
| `internal/pipeline/client_test.go` | New — 8 test cases |
| `internal/pipeline/code_detect.go` | New — code fence detection |
| `internal/pipeline/code_detect_test.go` | New — 8 test cases |
| `internal/pipeline/filter.go` | Modified — added `RedactSecretsPreservingCodeSpans`, `TruncateMiddleOutCodeAware` |
| `internal/pipeline/filter_test.go` | Modified — 12 new test cases |
| `internal/proxy/chat.go` | Modified — wires `ClientProfile` through pipeline |
| `internal/gateway/gateway.go` | Modified — extended `ExtractContentText` |
| `internal/routing/sanitize_test.go` | Modified — added tool_calls passthrough test |
| `internal/infra/response_cache_test.go` | Modified — added tool_call fingerprint tests |
| `docs/ARCHITECTURE.md` | Updated — IDE Compatibility section |
