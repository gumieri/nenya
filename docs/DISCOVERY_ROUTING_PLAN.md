# Model Discovery-Based Routing Enhancement Plan

## Overview

This plan outlines how Nenya can leverage the dynamic model discovery system to enhance routing, configuration validation, and user experience. The discovery system already fetches model catalogs from providers at startup and on SIGHUP reload — this plan proposes ways to use that data more effectively.

## Current State

### What Discovery Already Provides

The `internal/discovery` package currently captures:

- **Model ID** — The model identifier (e.g., `gemini-2.5-flash`, `deepseek-chat`)
- **Provider** — Which provider hosts the model
- **MaxContext** — Input token limit (from Gemini parser, static registry)
- **MaxOutput** — Output token limit (from Gemini parser, static registry)
- **OwnedBy** — Provider name (e.g., `google`, `deepseek`, `anthropic`)

### What Discovery Currently Does

1. **Fetches** `/v1/models` from each configured provider (concurrently, 10s timeout)
2. **Parses** provider-specific response formats (OpenAI, Anthropic, Gemini, Ollama)
3. **Merges** with static ModelRegistry using three-tier priority:
   - Config overrides (agent model entries with explicit fields)
   - Discovered models
   - Static registry fallback
4. **Updates** `Gateway.ModelCatalog` used by routing, `/v1/models` endpoint, and `max_tokens` injection

### What Provider Capabilities Are Available

The `internal/providers` package defines per-provider capabilities:

- `SupportsToolCalls` — Function calling support
- `SupportsReasoning` — Returns reasoning tokens (`reasoning_content` field)
- `SupportsVision` — Accepts image inputs
- `SupportsStreamOptions` — Supports `stream_options.include_usage`
- `SupportsAutoToolChoice` — Supports `tool_choice: "auto"`
- `SupportsContentArrays` — Supports content as array of objects

**Note:** These are provider-level, not model-level. Discovery does not currently capture model-specific capabilities.

---

## Proposed Enhancements

### 1. Deep Provider Health Validation

**Goal:** Identify broken/misconfigured providers before they affect routing.

**Implementation:**

Create `internal/discovery/health.go` with:

```go
type ProviderHealth struct {
    Name         string
    Status       string // "ok", "unreachable", "empty", "invalid"
    ModelsFound  int
    ExpectedModels []string // from static registry
    MissingModels []string
    LastFetched   time.Time
    Error        string
}

func ValidateProviderHealth(ctx context.Context, provider string, catalog *ModelCatalog, logger *slog.Logger) ProviderHealth
```

**Behavior:**

- **Unreachable** — `/v1/models` fetch fails (network, auth, timeout)
- **Empty** — Fetch succeeds but returns 0 models
- **Invalid** — Response is malformed JSON or wrong content-type
- **OK** — Fetch succeeds with valid models

**Integration:**

- Log structured health summary at startup and on reload
- Mark unhealthy providers in circuit breaker (ForceOpen with 60s cooldown)
- Expose health via `/statsz` endpoint under `provider_health` key

**Example Output:**

```json
{
  "provider_health": {
    "gemini": {
      "status": "ok",
      "models_found": 12,
      "last_fetched": "2026-04-22T22:30:00Z"
    },
    "deepseek": {
      "status": "unreachable",
      "error": "dial tcp: connection refused",
      "last_fetched": "2026-04-22T22:29:00Z"
    }
  }
}
```

---

### 2. Auto-Generated Agents (Zero-Config UX)

**Goal:** Provide useful agents automatically without manual configuration.

**Design Principles:**

- **No provider splitting** — Agents are cross-provider, not per-provider
- **`auto_` prefix** — All auto-generated agents start with `auto_`
- **User override** — Users can define their own agents with same names to override
- **Graceful degradation** — If discovery fails, auto-agents fall back to static registry

**Agent Categories:**

| Agent Name | Criteria | Use Case |
|------------|----------|----------|
| `auto_fast` | MaxContext ≤ 32k, MaxOutput ≤ 4k | Quick responses, low latency |
| `auto_reasoning` | MaxContext ≥ 128k, provider supports reasoning | Complex reasoning, long context |
| `auto_vision` | Provider supports vision | Image analysis, multimodal |
| `auto_tools` | Provider supports tool calls | Function calling, MCP integration |
| `auto_large` | MaxContext ≥ 200k | Maximum context window |
| `auto_balanced` | 32k < MaxContext < 128k | General purpose |

**Implementation:**

Create `internal/discovery/agents.go` with:

```go
type AutoAgentConfig struct {
    Name        string
    Description string
    Strategy    string // "round-robin" or "fallback"
    Filter      func(DiscoveredModel, ProviderSpec) bool
}

func GenerateAutoAgents(catalog *ModelCatalog, providers map[string]*config.Provider, logger *slog.Logger) map[string]config.AgentConfig
```

**Filter Logic:**

```go
func isFastModel(m DiscoveredModel, spec ProviderSpec) bool {
    return m.MaxContext > 0 && m.MaxContext <= 32000 &&
           m.MaxOutput > 0 && m.MaxOutput <= 4096
}

func isReasoningModel(m DiscoveredModel, spec ProviderSpec) bool {
    return m.MaxContext >= 128000 && spec.SupportsReasoning
}

func isVisionModel(m DiscoveredModel, spec ProviderSpec) bool {
    return spec.SupportsVision
}

func isToolModel(m DiscoveredModel, spec ProviderSpec) bool {
    return spec.SupportsToolCalls
}
```

**Integration:**

- Call `GenerateAutoAgents` after `MergeCatalog` in `gateway.New()`
- Merge auto-agents with user-defined agents (user agents take precedence)
- Log auto-generated agent summary at startup

**Example Output:**

```json
{
  "agents": {
    "auto_fast": {
      "strategy": "round-robin",
      "models": [
        "gemini-2.5-flash",
        "deepseek-chat",
        "claude-haiku-4-5"
      ]
    },
    "auto_reasoning": {
      "strategy": "fallback",
      "models": [
        "claude-sonnet-4-5",
        "gemini-2.5-flash",
        "deepseek-reasoner"
      ]
    },
    "auto_vision": {
      "strategy": "round-robin",
      "models": [
        "gemini-2.5-flash",
        "claude-sonnet-4-5",
        "gpt-4o"
      ]
    }
  }
}
```

**User Override Example:**

```json
{
  "agents": {
    "auto_fast": {
      "strategy": "fallback",
      "models": ["my-custom-fast-model"]
    }
  }
}
```

---

### 3. Intelligent Target Prioritization

**Goal:** Improve fallback success rate and latency using runtime heuristics.

**Enhancements:**

#### A. Context-Aware Filtering

In `BuildTargetList`, skip models that are too small for the input:

```go
func (a *AgentState) BuildTargetList(...) []UpstreamTarget {
    for _, m := range agent.Models {
        maxCtx := m.MaxContext
        if maxCtx == 0 {
            if catalog != nil {
                if disc, ok := catalog.Lookup(m.Model); ok {
                    maxCtx = disc.MaxContext
                }
            }
        }

        // Skip if model is < 20% of required context
        if maxCtx > 0 && tokenCount > 0 && maxCtx < tokenCount*5 {
            logger.Info("skipping model: context too small",
                "model", m.Model, "max_context", maxCtx, "tokens", tokenCount)
            continue
        }
        // ... rest of logic
    }
}
```

#### B. Latency-Based Reordering (Optional)

Track median round-trip time per model and sort targets accordingly:

```go
type ModelLatency struct {
    Model      string
    Provider   string
    MedianMs   float64
    SampleSize int
}

func SortTargetsByLatency(targets []UpstreamTarget, latencyStats map[string]ModelLatency) []UpstreamTarget
```

**Configuration:**

```json
{
  "governance": {
    "auto_reorder_by_latency": true,
    "auto_context_skip": true
  }
}
```

---

### 4. Registry Dynamic Sync Mode

**Goal:** Keep Nenya's model registry in sync with upstream providers without manual updates.

**Implementation:**

Add `discovery_sync_mode` to config:

```json
{
  "discovery": {
    "sync_mode": "merge", // "merge" or "discovered-only"
    "sync_ttl_days": 30
  }
}
```

**Modes:**

- **`merge`** (default) — Discovered models augment static registry (current behavior)
- **`discovered-only`** — Only discovered models are used, static registry is ignored

**Behavior:**

- On startup, sync discovered models to a persistent cache (file or database)
- On subsequent startups, load cached models if discovery fails
- Expire cached models after `sync_ttl_days`

**Use Case:**

- Strict discovered-only mode for testing new provider models
- Prevents routing to deprecated models not returned by discovery

---

## Implementation Sequence

### Phase 1 (Next Sprint)

1. **Deep Provider Health Validation**
   - Create `internal/discovery/health.go`
   - Add health checks to startup and reload
   - Expose health via `/statsz`
   - Log structured health summary

2. **Auto-Generated Agents**
   - Create `internal/discovery/agents.go`
   - Implement filter functions for each category
   - Integrate with `gateway.New()`
   - Add logging for auto-generated agents

### Phase 2 (ROI)

1. **Context-Aware Filtering**
   - Add `auto_context_skip` config option
   - Implement context size check in `BuildTargetList`
   - Add logging for skipped models

2. **Registry Dynamic Sync**
   - Add `discovery.sync_mode` config
   - Implement persistent cache for discovered models
   - Add TTL-based expiration

### Phase 3 (Optimizations)

1. **Latency-Based Reordering**
   - Track per-model latency metrics
   - Implement sorting logic
   - Add `auto_reorder_by_latency` config

2. **Advanced Health Checks**
   - Compare discovered models with static registry
   - Detect deprecated/removed models
   - Emit warnings for config drift

---

## Risks & Safeguards

### Discovery Must Never Worsen Routing

- **Default behavior** — All enhancements are opt-in via config flags
- **Graceful degradation** — If discovery fails, fall back to static registry
- **No breaking changes** — Existing configurations continue to work unchanged

### Limited Metadata from Discovery

- **Current limitation** — Discovery only captures ID, provider, context, output limits
- **Provider-level capabilities** — Tool/vision/reasoning support is per-provider, not per-model
- **Future enhancement** — Some providers expose model-specific metadata (pricing, capabilities) — could be added later

### Security Considerations

- **Input validation** — All discovered model IDs are sanitized (max 256 chars, printable only)
- **Auth protection** — Discovery only runs for providers with valid API keys
- **Rate limiting** — Discovery respects provider rate limits (10s timeout per provider)

---

## Configuration Examples

### Enable Auto-Agents

```json
{
  "discovery": {
    "auto_agents": true
  }
}
```

### Enable Context-Aware Filtering

```json
{
  "governance": {
    "auto_context_skip": true
  }
}
```

### Enable Latency-Based Reordering

```json
{
  "governance": {
    "auto_reorder_by_latency": true
  }
}
```

### Strict Discovered-Only Mode

```json
{
  "discovery": {
    "sync_mode": "discovered-only",
    "sync_ttl_days": 30
  }
}
```

---

## Testing Strategy

### Unit Tests

- Test health validation logic with mock responses
- Test auto-agent generation with various model catalogs
- Test context-aware filtering with different token counts

### Integration Tests

- Test discovery with real provider endpoints (using test API keys)
- Test auto-agent routing with actual requests
- Test health endpoint output format

### Manual Testing

- Verify auto-agents appear in `/v1/models` endpoint
- Test routing with `auto_fast`, `auto_reasoning`, etc.
- Verify health checks in `/statsz` endpoint
- Test graceful degradation when discovery fails

---

## Documentation Updates

### ARCHITECTURE.md

- Add "Auto-Agent Generation" section
- Add "Provider Health Validation" section
- Update "Model Discovery" section with new features

### CONFIGURATION.md

- Add `discovery` section with all new config options
- Document auto-agent behavior and override mechanism
- Document health check output format

### README.md

- Add auto-agents to features list
- Update `/v1/models` endpoint description
- Add health check endpoint to API table

---

## Success Metrics

- **Reduced configuration burden** — Users can use `auto_fast` without defining agents
- **Improved reliability** — Unhealthy providers are detected and skipped before affecting traffic
- **Better routing** — Context-aware filtering prevents routing to models that are too small
- **Fresh model support** — New provider models are usable immediately after discovery
