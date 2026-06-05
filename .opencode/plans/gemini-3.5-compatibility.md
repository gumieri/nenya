---
plan_version: 1
project: nenya
language: go
created: 2026-06-05
---

# Plan: Gemini 3.5 API Compatibility

## Overview

Update Nenya's Google Gemini integration to support Gemini 3.5+ API changes. Google has released new models (gemini-3.5-flash), shut down dead models (gemini-3-pro-preview), and changed how thinking/reasoning, function calling, and thought signatures work. This plan fixes 8 critical gaps to get Gemini back working with modern models.

## Verification

- lint: golangci-lint run
- build: go build ./...
- test: go test ./... -count=1
- race: go test -race -count=1 ./...

## Phases

### Phase 010: Update model registry and maps

**Depends on:** (none)
**Branch:** phase-010-gemini-model-registry
**Files:** config/registry.go, internal/providers/gemini.go, internal/adapter/gemini.go

#### Objective

Remove dead gemini-3-pro-preview model from registry and model maps, add new gemini-3.5-flash model, keep both model map copies in sync.

#### Steps

1. Remove line 142 from config/registry.go: delete `"gemini-3-pro-preview"` entry (model is shut down, causes 400 errors)
2. Add new entry to config/registry.go after line 138: `"gemini-3.5-flash": {Provider: "gemini", MaxContext: 1048576, MaxOutput: 65536, Thinking: ModelThinkingConfig{Min: 128, Max: 32768, DynamicAllowed: true, Levels: []string{"minimal", "low", "medium", "high"}}, Pricing: PricingOverride{InputCostPer1M: 0.075, OutputCostPer1M: 0.3}}`
3. Update internal/providers/gemini.go line 14: delete `"gemini-3-pro": "gemini-3-pro-preview",` from GeminiModelMap
4. Add to internal/providers/gemini.go line 15: `"gemini-3.5-flash": "gemini-3.5-flash",` to GeminiModelMap
5. Update internal/adapter/gemini.go line 14: delete `"gemini-3-pro": "gemini-3-pro-preview",` from GeminiModelMap
6. Add to internal/adapter/gemini.go line 15: `"gemini-3.5-flash": "gemini-3.5-flash",` to GeminiModelMap

#### Verification

- go test ./config/... -count=1
- go test ./internal/providers/... -count=1
- go test ./internal/adapter/... -count=1

#### Expected Outcome

Dead model removed from all 3 locations, gemini-3.5-flash added to all 3 locations, both GeminiModelMap copies are identical and in sync.

---

### Phase 011: Add gemini-3 capability rule

**Depends on:** (none)
**Branch:** phase-011-gemini-3-capability-rule
**Files:** internal/discovery/capabilities.go

#### Objective

Add capability inference rule for gemini-3 prefix so that gemini-3.x models (gemini-3-flash, gemini-3.1-pro, gemini-3.5-flash) get proper capabilities (vision, tool_calls, reasoning, content_arrays, auto_tool_choice) during dynamic discovery and static inference.

#### Steps

1. Add new rule after line 28: `{prefix: "gemini-3", caps: []Capability{CapVision, CapToolCalls, CapReasoning, CapContentArrays, CapAutoToolChoice}},`
2. Verify rule is placed before gemini-2 rule (prefix match order matters, longer prefixes should match first)

#### Verification

- go test ./internal/discovery/... -count=1

#### Expected Outcome

gemini-3 prefix now infers all capabilities, enabling proper routing and feature detection for Gemini 3.x models.

---

### Phase 012: Add thinking config mapping for Gemini

**Depends on:** (none)
**Branch:** phase-012-gemini-thinking-mapping
**Files:** internal/providers/gemini.go

#### Objective

Add mapping from client `reasoning_effort` parameter to Gemini's `thinkingLevel` (Gemini 3+) or `thinkingBudget` (Gemini 2.5), following the pattern used by ZAI provider. This allows clients to control thinking intensity on Gemini models.

#### Steps

1. Add helper function `injectThinkingForGemini(deps *SanitizeDeps, payload map[string]interface{})` after line 128 (before geminiSanitize)
2. Add helper function `isGemini3OrNewer(model string) bool` that checks if model contains "gemini-3"
3. In injectThinkingForGemini: extract model from payload, return early if empty
4. In injectThinkingForGemini: check for `reasoning_effort` in payload, return early if not present
5. In injectThinkingForGemini: if isGemini3OrNewer(model), map reasoning_effort ("none"/"disable"→"minimal", "low"→"low", "medium"→"medium", "high"→"high") to payload["extra_body"]["google"]["thinking_config"]["thinkingLevel"]
6. In injectThinkingForGemini: if NOT gemini-3 (Gemini 2.5), map reasoning_effort ("none"/"disable"→0, "minimal"/"low"→1024, "medium"→8192, "high"→24576) to payload["extra_body"]["google"]["thinking_config"]["thinkingBudget"]
7. In injectThinkingForGemini: remove `reasoning_effort` from payload after mapping
8. Add call to `injectThinkingForGemini(deps, payload)` at start of `geminiSanitize` function
9. Add debug logging when thinking config is injected (follow ZAI's logThinkingEnabled pattern)

#### Verification

- go test ./internal/providers/... -count=1

#### Expected Outcome

Clients can now send `reasoning_effort` parameter, which gets mapped to Gemini's thinking config. Gemini 3+ uses thinkingLevel (discrete levels), Gemini 2.5 uses thinkingBudget (integer tokens).

---

### Phase 013: Add temperature default for Gemini 3

**Depends on:** (none)
**Branch:** phase-013-gemini-temperature-default
**Files:** internal/providers/gemini.go

#### Objective

Set temperature=1.0 as default for Gemini 3 models, respecting existing client-specified temperature values. Google recommends temperature=1.0 for Gemini 3 to prevent looping on reasoning tasks.

#### Steps

1. Add helper function `injectTemperatureDefaultsForGemini(payload map[string]interface{})` after injectThinkingForGemini
2. In injectTemperatureDefaultsForGemini: extract model from payload, return early if empty
3. In injectTemperatureDefaultsForGemini: check if model contains "gemini-3" (case-insensitive)
4. In injectTemperatureDefaultsForGemini: if NOT gemini-3 or temperature already set, return early
5. In injectTemperatureDefaultsForGemini: set payload["temperature"] = 1.0
6. Add call to `injectTemperatureDefaultsForGemini(payload)` at start of `geminiSanitize` function (after injectThinkingForGemini)
7. Add debug logging when temperature is set to 1.0 for Gemini 3

#### Verification

- go test ./internal/providers/... -count=1

#### Expected Outcome

Gemini 3 models automatically get temperature=1.0 unless client explicitly sets it. Prevents looping behavior Google warns about for low temperature on reasoning models.

---

### Phase 014: Add function call ID forwarding for Gemini 3.5+

**Depends on:** (none)
**Branch:** phase-014-gemini-function-call-id
**Files:** internal/providers/gemini.go, internal/adapter/gemini.go

#### Objective

Add logic to forward tool call IDs from functionCalls to functionResponses for Gemini 3.5+ models. Gemini 3.5+ returns unique `id` with every functionCall and requires echoing it in functionResponse. This is only needed for AI Studio Gemini, NOT Vertex AI (Vertex rejects the id field).

#### Steps

1. Add helper function `isGemini35OrNewer(model string) bool` in internal/providers/gemini.go that checks if model contains "gemini-3.5" or "gemini-4"
2. Add helper function `forwardFunctionCallIDs(messages []interface{})` in internal/providers/gemini.go that scans assistant messages for tool_calls, builds ID→functionName map, then injects tool_call_id on matching tool messages
3. In geminiSanitize: after geminiFilterMessages, if isGemini35OrNewer(model), call forwardFunctionCallIDs(messages)
4. Duplicate forwardFunctionCallIDs logic to internal/adapter/gemini.go as adapter method (follow existing duplicate pattern for geminiSanitize)
5. Add debug logging when function call IDs are forwarded
6. Note: Vertex AI does NOT support id field — only forward for AI Studio (generativelanguage.googleapis.com host detection or rely on existing provider name check)

#### Verification

- go test ./internal/providers/... -count=1
- go test ./internal/adapter/... -count=1

#### Expected Outcome

Gemini 3.5+ function calls now have their IDs echoed in function responses, preventing 400 errors from missing id fields. Tool call ID→function name mapping ensures correct pairing.

---