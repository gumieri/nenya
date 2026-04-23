Plan: Enhancing Nenya with Model Discovery-Based Features  
1. Validate Existing Provider Config (Deep Health Check)  
Goal: Identify broken/misconfigured providers and replace/deprioritize them without client visible failures.  
How:  
- Create /internal/discovery/health.go to perform "deep health" of providers:  
  - Fetch /v1/models and compare response models with expected static list  
  - Ping /v1/models each restart and HUP => differentiate “catalog unreachable” from “catalog empty”  
  - For providers with market-models (Gemini, Anthropic), audit return:  
    - Input/Output token limits  
    - Parse/Ollama-style metadata fields  
- Activate Circuit Breaker on misconfigured providers (mark Open 60s on recurring 5xx during fetch)  
- Log audit summary in structured logs:  
  -稼働Provider, invalidprovider, missing-model list, recommend new names  
Outcomes:  
- Dyn-validate provider key + endpoint sanity before routing traffic  
- Autoskip unresponsive/404 providers and warn operator  
- Fail-fast on new Parsed model missing from routing tables (config drift)  
---
2. "Auto-Agent" Generation (Zero-Config UX)  
Goal: Save users from defining agents for common use-cases, give new “it just works” UX.  
How:  
A. Capability Clustered Agents:  
  - On discovery, classify each model by capabilities from discovery fields or dedicated endpoints:  
    - Small fast (8-32k ctx, reasoning=yes)  
    - Big reasoning (sweet spot 128k)  
    - Vision (if supports content.url.images)  
    - Tool-capable (expose function-calling)  
  - Auto-create agents like:  
    - “<provider>_fast” – filtering to fast, small models  
    - “<provider>_reasoning” – >128k ctx + supports reasoning field  
    - “<provider>_vision” – vision capable  
    - “<provider>_tools” – supports tools  
  - Agents include round-robin on members (no auth failures)  
B. Auto-Sized Window Agents:  
  - When MaxContext discovered, create agents with appropriate trigger_ratio and active_messages  
  - Example:  
    json\n    \"antropic_cheap\": {\n      \"strategy\": \"round-robin\",\n      \"models\": [\"claude-haiku-latest\",…],\n      \"window\": { \"max_context\": 128000, ”trigger_ratio\": 0.7 }\n    }\n    
C. Discovery-as-Source-of-Truth Mode (Optional Flip)  
  - Add agents_origin: \"discovered\" to flip list to override registry only with discovered models. Enables studio-like “no built-ins” mode — kind of strict discovered-only mode. Enables \<rules/bans for provider dog-food testing. \n\nOutcomes:  
- UX: “Use agent: reasoning to get cheapest/fastest reasoning model.”  
- Config: users can still override. (Cascade: agent locally=fixed priority; global agent=auto-generated.)  
- Nudge: end users get better/freshly released models without config bumps  
---
3. Intelligent Target Prioritization (Runtime Heuristics)  
Goal: Improve fallback success rate and latency by letting gateway rank models beyond round-robin.  
Input:  
- Discovery MaxContext, MaxOutput, OwnedBy  
- Rates + performance heuristics from /statsz internal counters  
- Provider metadata (pricing: cloud vs local)  
- Recent circuit-breaker health  
How:  
A. Latency Reordering  
  - Track median round-trip time per model per provider via stats*,metrics  
  - Sort discovered models in BuildTargetList via LatencyEst  
  - Gate it with auto_reorder_by_latency: true (opt-in to calibrate)  
B. Token-Congestion Aware Fallback  
  - Filter high-congestion (recent high-429/error) providers on fetch-discover weight average  
  - Only push to last (least expensive) provider from feedback loop  
C. Context-Match Aware  
  - In BuildTargetList, ResolveProvider, use discovered MaxContext to skip models that are <20% of input token count  
  - Disable strict delta with auto_context_skip:false  
Outcomes:  
- Route preferred, actual latencies  
- Skip tokens-too-small models without over-truncation loop  
- Better weighted failover chain, not blind shuffling  
---
4. Maintain Registry Dynamic Sync Mode  
Goal: Close gap between upstream released models and Nenya static registry (today released 1/d quarterly?).  
How:  
- Start treating registry as defaults not source  
  - Registry-compile-only includes foundational stable names  
  - At first startup, sync new names into discovered-only catalog  
- Add flag refresh_registry_from_discovery_on_cold_start:bool to merge field only for discovered entries (hold long-term TTL 30d, stored)  
- On telemetry, if static-model receives unknown model error (400 “unknown”), dump discovered list to debug recommending update or even emit &leave pseudo-comment lines into pull-request auto-prepare  
Outcomes:  
- New upstream models usable minutes after provider publishes  
- Gateway owners maintain stability/control without manual config churn  
---
Implementation Sequence (suggested)  
Phase 1 (next sprint)  
1. Deep-health discovery audit: /healthz_oiled endpoint listing provider health state + discovered models  
2. Auto-skip Open providers that report outdated stable list (heuristic: if discovery only returns <5 models and name hash differs, emit warning)  
Phase 2 (ROI)  
1. Provide capability detection functions (vision, tools, reasoning, max-context) from discovered  
2. Auto-generate “fast”, “reasoning”, “vision” agents page into runtime or optionally preflight  
Phase 3 (Optimizations)  
1. Latency/throughput-driven reorder with circuit state superseding slow hosts  
2. Context-aware filtering in BuildTargetList  
3. Long-term usage/4xx based provider reorder heuristics  
---
Risks & Safeguards  
- Discovery must never worsen routing — default log-warn-only, always revert hash-stable heuristics  
- Knowledge of discovered tokens per model or pricing absent, attendees to feature-limit at current property (context, reasoning flags, vision)  
- Path normalisation and quote sanitisation must be validated immunity in discovery msg parsing  
