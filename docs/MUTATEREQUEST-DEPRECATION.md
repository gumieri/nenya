# MutateRequest Interface - Deprecation Notice

## Status: **DEPRECATED** (Not Dead Code - Vestigial)

**Last Updated:** 2026-06-17  
**Deprecation Phase:** Documentation-only (no code changes needed)

---

## Summary

The `ProviderAdapter.MutateRequest()` interface method is **not dead code** but it is **vestigial** - it remains in the codebase for backward compatibility and minor provider-specific transformations, but its usage has significantly diminished due to architectural evolution.

---

## What MutateRequest Does

The `MutateRequest` method is defined in the `ProviderAdapter` interface (`internal/adapter/adapter.go:51`):

```go
type ProviderAdapter interface {
    MutateRequest(body []byte, model string, stream bool) ([]byte, error)
    InjectAuth(req *http.Request, apiKey string) error
    MutateResponse(body []byte) ([]byte, error)
    NormalizeError(statusCode int, body []byte) ErrorClass
}
```

It transforms the request body **after** spec-level sanitization (`ProviderSpec.SanitizeRequest`) but **before** sending the request to the upstream provider.

---

## Why It's Vestigial (Not Dead)

The codebase has evolved to use **ProviderSpec.SanitizeRequest** hooks instead for most request mutation:

### Historical Evolution

1. **Original Architecture:** All request mutations happened in `MutateRequest()`
2. **Current Architecture:** Mutations split across two layers:
   - **Spec-level (`ProviderSpec.SanitizeRequest`)**: Runs before adapter transformation
   - **Adapter-level (`MutateRequest`)**: Runs after spec-level sanitization

### Current Usage Patterns

#### Active Providers with MutateRequest

| Provider | Usage Pattern | Reason |
|----------|---------------|--------|
| **Anthropic** | **Full transformation** (OpenAI → Anthropic format) | Wire format incompatibility |
| **Gemini** | **Full transformation** (OpenAI → Gemini format) | Wire format incompatibility |
| **ZAI** | **Category B only** (message cleanup) | Tool call filtering, message merging |
| **Ollama** | **Minimal** (remove tool_choice) | Tool choice not supported |
| **XAI, OpenRouter, Perplexity** | **Delegate to OpenAI** | OpenAI-compatible |

#### Spec-Level SanitizeRequest (Preferred Layer)

| Provider | SanitizeRequest Hook | Operations |
|----------|---------------------|------------|
| **Gemini** | `geminiSanitize()` | Thinking injection, temperature defaults, message cleanup |
| **ZAI** | `ZaiSanitizeSpecOnly()` | Thinking injection, temperature defaults |
| **ZAI-Coding-Plan** | `ZaiSanitizeSpecOnly()` | Thinking injection, temperature defaults |

---

## What MutateRequest Is Used For Now

### 1. Wire Format Conversion (Primary Use Case)

**Providers:** Anthropic, Gemini

These providers have fundamentally different request/response formats from OpenAI, requiring full conversion in `MutateRequest()`:

```go
// Example: Anthropic adapter converts OpenAI format to Anthropic format
func (a *AnthropicAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
    // Parse OpenAI format
    var payload map[string]interface{}
    json.Unmarshal(body, &payload)

    // Transform to Anthropic format
    anthropicPayload := transformToAnthropic(payload, model, stream)

    // Return converted payload
    return json.Marshal(anthropicPayload)
}
```

### 2. Minor Provider-Specific Adjustments

**Providers:** Ollama, ZAI

These providers perform minimal adjustments in `MutateRequest()`:

- **Ollama:** Removes `tool_choice` field (not supported)
- **ZAI:** Category B message cleanup (tool call filtering, message merging)

### 3. Delegation (Compatibility Shim)

**Providers:** XAI, OpenRouter, Perplexity

These providers delegate to `OpenAIAdapter.MutateRequest()` because they're OpenAI-compatible:

```go
func (a *XAIAdapter) MutateRequest(body []byte, model string, stream bool) ([]byte, error) {
    return (&OpenAIAdapter{Caps: a.Caps}).MutateRequest(body, model, stream)
}
```

---

## Why Not Remove MutateRequest?

### 1. Wire Format Conversion Requirement

Some providers (Anthropic, Gemini) require full request/response transformation. This cannot be moved to `SanitizeRequest` because:

- `SanitizeRequest` runs **before** adapter transformation
- Wire format conversion happens **after** adapter transformation
- The two layers serve different purposes

### 2. Minor Adjustments Still Needed

ZAI and Ollama perform minor adjustments that don't justify spec-level hooks:

- **Ollama:** Single field removal (`tool_choice`)
- **ZAI:** Category B message cleanup (already split from `ZaiSanitizeSpecOnly()`)

### 3. Backward Compatibility

The `ProviderAdapter` interface is widely used and removing `MutateRequest()` would require:

- Updating all adapter implementations
- Breaking existing code
- No tangible benefit

### 4. Clear Separation of Concerns

The current architecture provides a clean separation:

- **Spec-level (`SanitizeRequest`)**: Cross-cutting concerns (thinking, temperature, generic cleanup)
- **Adapter-level (`MutateRequest`)**: Provider-specific concerns (wire format, minor adjustments)

---

## When to Use MutateRequest vs SanitizeRequest

### Use `MutateRequest()` When:

1. Converting wire formats (OpenAI → Anthropic, OpenAI → Gemini)
2. Removing/adding provider-specific fields that don't apply to other providers
3. Adjusting the request after spec-level sanitization has completed
4. The transformation is unique to the provider and not reusable

### Use `SanitizeRequest` When:

1. Injecting provider-specific configuration (thinking mode, temperature defaults)
2. Performing generic message cleanup that applies across providers
3. The transformation should run before adapter-level wire format conversion
4. The logic should be reusable (e.g., both `zai` and `zai-coding-plan`)

---

## Future Considerations

### No Action Required

The current architecture is **sound and intentional**. `MutateRequest()` serves a distinct purpose from `SanitizeRequest` and removing it would be unnecessary complexity.

### Recommended Maintenance

1. **Keep both layers:** Spec-level and adapter-level transformations serve different purposes
2. **Document new additions:** When adding new providers, document which layer handles which transformations
3. **Avoid double-mutation:** Ensure `SanitizeRequest` and `MutateRequest` don't duplicate work (use the split pattern demonstrated by `ZaiSanitizeSpecOnly()` + `zaiSanitizeAdapterOnly()`)

---

## References

- **Interface Definition:** `internal/adapter/adapter.go:50-55`
- **Spec-Level Hooks:** `internal/providers/spec.go:22-30`
- **Adapter Registry:** `internal/adapter/registry.go:14-18`
- **ZAI Split Pattern:** `internal/providers/zai.go:16-56`

---

## Conclusion

`MutateRequest` is **vestigial but not dead**. It remains essential for wire format conversion and minor provider-specific adjustments. The architectural evolution to use `SanitizeRequest` hooks for cross-cutting concerns is **correct** and the current two-layer approach is **by design**.

**No code changes are needed.** This document exists to clarify the architectural intent and prevent future confusion about whether `MutateRequest` should be removed.