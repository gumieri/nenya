package routing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/nenya/config"
	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	providerpkg "github.com/nenya/internal/providers"
	"github.com/nenya/internal/util"
)

// TransformDeps provides the dependencies needed for request payload
// transformation (provider lookup, API key injection, metrics, etc.).
type TransformDeps struct {
	Logger             *slog.Logger
	Providers          map[string]*config.Provider
	Config             *config.Config
	ThoughtSigCache    infra.ThoughtSignatureKV
	ExtractContentText func(msg map[string]interface{}) string
	Catalog            *discovery.ModelCatalog
	// UnknownFieldsAudited gates auditRequestFields to once per logical
	// request across failover attempts (AUD-39 / NENYA-245). Per-request
	// callers pass a shared bool owned by the request's retry loop; nil
	// keeps the per-attempt semantics for callers without request scope.
	UnknownFieldsAudited *bool
	CountTokens          func(string) int
	AgentName            string
	CacheSalt            string
	// SessionIdentity is the coherent per-conversation session identity
	// (synthesized or client-supplied, NENYA-143/158). When set, the
	// prompt-cache key derives from IT — per conversation — instead of the
	// per-agent fallback.
	SessionIdentity string
	// CacheAffinity is the target provider's affinity convention ("body" |
	// "header", NENYA-158): "header" suppresses the body-field injection
	// (the identity rides the Session-Id header instead).
	CacheAffinity string

	// SourceFormat is the CLIENT's wire format ("openai" | "anthropic"): the
	// client axis of the (client, target) pair target-aware normalization
	// keys on (NENYA-179).
	SourceFormat string

	// WindowCompacted reports that the content pipeline rewrote the history
	// (window compaction) for this request. Auto cache-breakpoint injection
	// stands down when it is set: the compacted head is a gateway-produced
	// region whose stability across turns is not ours to promise (NENYA-185).
	WindowCompacted bool

	Metrics *infra.Metrics
}

func InjectAPIKeyWithGateway(providerName string, gw interface {
	GetProviderAPIKey(providerName string) ([]byte, bool)
	GetProvidersMap() map[string]*config.Provider
}, headers http.Header) error {
	providers := gw.GetProvidersMap()
	p, ok := providers[providerName]
	if !ok {
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	if p.AuthStyle != config.AuthStyleNone {
		keyBytes, ok := gw.GetProviderAPIKey(providerName)
		if !ok {
			return fmt.Errorf("provider %s has no API key configured", providerName)
		}
		a := adapter.ForProviderWithAuth(providerName, p.AuthStyle)
		req := &http.Request{Header: headers}
		return a.InjectAuth(req, string(keyBytes))
	}

	return nil
}

// APIKeyProviderModel extends APIKeyProvider with model-aware key selection.
type APIKeyProviderModel interface {
	GetProviderAPIKey(providerName string) ([]byte, bool)
	GetProviderAPIKeyForModel(ctx context.Context, providerName, model string) ([]byte, string, bool)
	GetProvidersMap() map[string]*config.Provider
}

// InjectAPIKeyWithGatewayCtx selects an API key using multi-account routing
// when available, falling back to the legacy single-key path. If preselectedCredential
// is non-empty, it is used directly without calling SelectAccount again.
func InjectAPIKeyWithGatewayCtx(ctx context.Context, providerName, modelName string, gw APIKeyProviderModel, headers http.Header, preselectedCredential string) error {
	providers := gw.GetProvidersMap()
	p, ok := providers[providerName]
	if !ok {
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	if p.AuthStyle == config.AuthStyleNone {
		return nil
	}

	key, err := resolveCredential(ctx, providerName, modelName, gw, preselectedCredential)
	if err != nil {
		return err
	}

	a := adapter.ForProviderWithAuth(providerName, p.AuthStyle)
	req := &http.Request{Header: headers}
	return a.InjectAuth(req, key)
}

// resolveCredential returns the credential to use, preferring preselectedCredential
// over legacy single-key path via GetProviderAPIKeyForModel.
func resolveCredential(ctx context.Context, providerName, modelName string, gw APIKeyProviderModel, preselectedCredential string) (string, error) {
	if preselectedCredential != "" {
		return preselectedCredential, nil
	}
	keyBytes, _, ok := gw.GetProviderAPIKeyForModel(ctx, providerName, modelName)
	if !ok {
		return "", fmt.Errorf("provider %s has no API key configured", providerName)
	}
	return string(keyBytes), nil
}

func resolveModelMapping(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) string {
	finalModel := modelName
	if spec, ok := providerpkg.Get(providerName); ok && spec.ModelMap != nil {
		finalModel = util.ApplyModelMap(spec.ModelMap, modelName)
	}
	// Operator-configured aliases win over built-in spec mappings: an
	// explicit model_aliases entry is deliberate per-deployment intent
	// (NENYA-22). Exact match on the canonical ID.
	if p, ok := deps.Providers[providerName]; ok && p != nil && len(p.ModelAliases) > 0 {
		if aliased, ok := p.ModelAliases[modelName]; ok {
			finalModel = aliased
		}
	}
	payload["model"] = finalModel
	if finalModel != modelName && deps.Logger != nil {
		deps.Logger.Info("provider model mapping", "provider", providerName, "from", modelName, "to", finalModel)
	}
	return finalModel
}

func buildSanitizeDeps(deps TransformDeps) *providerpkg.SanitizeDeps {
	sanitizeDeps := &providerpkg.SanitizeDeps{
		Logger:             deps.Logger,
		ThoughtSigCache:    deps.ThoughtSigCache,
		ExtractContentText: deps.ExtractContentText,
	}
	if deps.Catalog != nil {
		sanitizeDeps.SupportsReasoning = func(model string) bool {
			// Check dynamic discovery catalog first.
			if dm, ok := deps.Catalog.Lookup(model); ok && dm.Metadata != nil {
				return dm.Metadata.SupportsReasoning
			}
			// Fall back to static model registry: a model supports reasoning
			// if its Thinking config has any non-zero fields.
			if entry, ok := config.ModelRegistry[model]; ok {
				return entry.Thinking.HasThinking()
			}
			return false
		}
	}
	if deps.Providers != nil {
		sanitizeDeps.ProviderThinking = func(name string) (bool, bool, bool) {
			p, ok := deps.Providers[name]
			if !ok || p.Thinking == nil {
				return false, false, false
			}
			return p.Thinking.Enabled, p.Thinking.ClearThinking, true
		}
	}
	return sanitizeDeps
}

func applyProviderSanitize(deps TransformDeps, payload map[string]interface{}, providerName string) {
	spec, ok := providerpkg.Get(providerName)
	if !ok {
		return
	}
	if spec.SanitizeRequest == nil {
		return
	}
	sanitizeDeps := buildSanitizeDeps(deps)
	if p, ok := deps.Providers[providerName]; ok && p != nil {
		sanitizeDeps.ThoughtSignaturePolicy = p.ThoughtSignaturePolicy
	}
	spec.SanitizeRequest(sanitizeDeps, payload)
}

// injectPromptCacheKey injects a stable cache key into the request body for
// providers that support prompt caching (xAI, OpenAI). Controlled by
// prefix_cache.enabled in config; a client-supplied prompt_cache_key always
// wins.
//
// The key is SHA-256 truncated to 16 hex chars (64 bits) — sufficient
// entropy for cache routing with low collision risk. The identity is
// PER-CONVERSATION when the caller supplies the session identity (NENYA-158:
// every conversation of an agent sharing one key defeated per-session cache
// routing); the SHA-256(agent:model) fallback covers callers without one.
// The input is non-secret (already transmitted upstream), so fast
// deterministic hashing is correct; Argon2/scrypt/bcrypt would be
// inappropriate.
func injectPromptCacheKey(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) {
	if deps.Config == nil || !deps.Config.PrefixCache.Enabled {
		return
	}

	if deps.CacheAffinity == "header" {
		// Header convention (NENYA-158): the identity rides the
		// Session-Id header at dispatch; the body field stays clean.
		return
	}
	lower := strings.ToLower(providerName)
	if lower != "xai" && lower != "openai" && deps.CacheAffinity != "body" {
		return
	}

	if _, exists := payload["prompt_cache_key"]; exists {
		return
	}

	identity := deps.SessionIdentity
	if identity == "" {
		if deps.AgentName == "" {
			if deps.Logger != nil {
				deps.Logger.Debug("skipped prompt_cache_key injection: no session identity or agent name",
					"provider", providerName, "model", modelName)
			}
			return
		}
		identity = deps.AgentName + ":" + modelName
	}
	h := sha256.Sum256([]byte(identity))
	payload["prompt_cache_key"] = hex.EncodeToString(h[:])[:16]
}

func injectCacheSalt(deps TransformDeps, payload map[string]interface{}, providerName string) {
	if deps.CacheSalt == "" {
		return
	}
	lower := strings.ToLower(providerName)
	// vLLM/OpenAI-compatible providers support cache_salt
	if lower != "openai" && lower != "vllm" && lower != "deepinfra" {
		return
	}
	payload["cache_salt"] = deps.CacheSalt
}

func injectOpenAICacheBreakpoints(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) {
	if deps.Config == nil || !deps.Config.PrefixCache.Enabled {
		return
	}

	lower := strings.ToLower(providerName)
	if lower != "openai" {
		return
	}

	if !*deps.Config.PrefixCache.OpenAIBreakpoint {
		return
	}

	if !supportsOpenAICacheBreakpoints(modelName) {
		if deps.Logger != nil {
			deps.Logger.Debug("skipped OpenAI cache breakpoint injection: model not in GPT-5.6+ family", "model", modelName)
		}
		return
	}

	if _, hasKey := payload["prompt_cache_key"]; !hasKey {
		if deps.Logger != nil {
			deps.Logger.Debug("skipped OpenAI cache breakpoint injection: prompt_cache_key missing", "model", modelName)
		}
		return
	}

	setOpenAICacheMode(deps, payload)
	injectOpenAIBreakpointOnLastBlock(payload)
}

func supportsOpenAICacheBreakpoints(modelName string) bool {
	lowerModel := strings.ToLower(modelName)
	return strings.HasPrefix(lowerModel, "gpt-5.6") || strings.HasPrefix(lowerModel, "gpt-5.7")
}

func setOpenAICacheMode(deps TransformDeps, payload map[string]interface{}) {
	if deps.Config.PrefixCache.OpenAIMode == nil {
		return
	}
	if *deps.Config.PrefixCache.OpenAIMode != config.OpenAIModeExplicit {
		return
	}
	if _, hasOptions := payload["prompt_cache_options"]; !hasOptions {
		payload["prompt_cache_options"] = map[string]interface{}{"mode": "explicit"}
	} else if options, ok := payload["prompt_cache_options"].(map[string]interface{}); ok {
		options["mode"] = "explicit"
	}
}

func injectOpenAIBreakpointOnLastBlock(payload map[string]interface{}) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) < 2 {
		return
	}

	targetMsg, ok := messages[len(messages)-2].(map[string]interface{})
	if !ok {
		return
	}

	content, ok := targetMsg["content"].([]interface{})
	if !ok || len(content) == 0 {
		return
	}

	lastBlock, ok := content[len(content)-1].(map[string]interface{})
	if ok {
		lastBlock["prompt_cache_breakpoint"] = map[string]interface{}{"mode": "explicit"}
	}
}

func shouldInjectSystem(firstMsg map[string]interface{}, agent config.AgentConfig) bool {
	if agent.ForceSystemPrompt {
		return true
	}
	role, ok := firstMsg["role"].(string)
	if !ok || role != "system" {
		return true
	}
	return false
}

func safeCapPlusOne(n int) int {
	if n >= math.MaxInt {
		return n
	}
	return n + 1
}

func injectSystemMessage(deps TransformDeps, payload map[string]interface{}, agentNameRaw string, systemPrompt string, agent config.AgentConfig) {
	messagesRaw, ok := payload["messages"]
	if !ok {
		return
	}
	messages, ok := messagesRaw.([]interface{})
	if !ok || len(messages) == 0 {
		return
	}
	injectSystem := true
	if !agent.ForceSystemPrompt {
		if firstMsg, ok := messages[0].(map[string]interface{}); ok {
			injectSystem = shouldInjectSystem(firstMsg, agent)
			if !injectSystem {
				deps.Logger.Debug("agent system prompt skipped: first message already system role", "agent", agentNameRaw)
			}
		}
	}
	if !injectSystem {
		return
	}
	insertAt := 0
	if firstMsg, ok := messages[0].(map[string]interface{}); ok && agent.ForceSystemPrompt && firstMessageCarriesCacheControl(firstMsg) {
		// Never displace a client system message that carries a
		// cache_control breakpoint: inserting ahead of it would shift
		// the provider-cached prefix and re-bill the conversation every
		// turn. Place the agent prompt immediately after it instead.
		insertAt = 1
		deps.Logger.Debug("agent system prompt placed after cached client system message", "agent", agentNameRaw)
	}
	systemMsg := map[string]interface{}{
		"role":    "system",
		"content": systemPrompt,
	}
	capMsg := safeCapPlusOne(len(messages))
	newMessages := make([]interface{}, 0, capMsg)
	newMessages = append(newMessages, messages[:insertAt]...)
	newMessages = append(newMessages, systemMsg)
	newMessages = append(newMessages, messages[insertAt:]...)
	payload["messages"] = newMessages
	deps.Logger.Info("injected agent system prompt", "agent", agentNameRaw)
}

// firstMessageCarriesCacheControl reports whether the message carries an
// Anthropic prompt-cache breakpoint, either at the message level or on
// any content block. Used to keep injected system prompts from shifting
// the client's cached prefix.
func firstMessageCarriesCacheControl(msg map[string]interface{}) bool {
	if _, ok := msg["cache_control"]; ok {
		return true
	}
	content, ok := msg["content"].([]interface{})
	if !ok {
		return false
	}
	for _, partRaw := range content {
		if part, ok := partRaw.(map[string]interface{}); ok {
			if _, ok := part["cache_control"]; ok {
				return true
			}
		}
	}
	return false
}

func resolveAgentSystemPrompt(deps TransformDeps, payload map[string]interface{}, origModel interface{}, providerName string) {
	agentNameRaw, ok := origModel.(string)
	if !ok {
		return
	}
	agent, ok := deps.Config.Agents[agentNameRaw]
	if !ok {
		return
	}
	if agent.SystemPrompt == "" && agent.SystemPromptFile == "" {
		return
	}
	systemPrompt, err := config.LoadPromptFile(agent.SystemPromptFile, agent.SystemPrompt, "")
	if err != nil {
		deps.Logger.Warn("failed to load agent system prompt, skipping", "agent", agentNameRaw, "err", err)
		return
	}
	if systemPrompt == "" {
		return
	}
	injectSystemMessage(deps, payload, agentNameRaw, systemPrompt, agent)
}

func resolveEffectiveMaxOutput(deps TransformDeps, finalModel string, maxOutput int) int {
	effectiveMaxOutput := 0
	if deps.Catalog != nil {
		if m, ok := deps.Catalog.Lookup(finalModel); ok && m.MaxOutput > 0 {
			effectiveMaxOutput = m.MaxOutput
		}
	}
	if effectiveMaxOutput == 0 {
		// AUD-42 (NENYA-236): fold plain-ID spellings (opaque IDs exact,
		// NENYA-146) — see registryLimitsKey.
		if entry, ok := config.ModelRegistry[registryLimitsKey(finalModel)]; ok && entry.MaxOutput > 0 {
			effectiveMaxOutput = entry.MaxOutput
		}
	}
	if maxOutput > 0 && (effectiveMaxOutput == 0 || maxOutput < effectiveMaxOutput) {
		effectiveMaxOutput = maxOutput
	}
	return effectiveMaxOutput
}

// resolveEffectiveMaxContext returns the model's context window from the
// discovery catalog or the static registry; 0 when unknown.
func resolveEffectiveMaxContext(deps TransformDeps, finalModel string) int {
	if deps.Catalog != nil {
		if m, ok := deps.Catalog.Lookup(finalModel); ok && m.MaxContext > 0 {
			return m.MaxContext
		}
	}
	if entry, ok := config.ModelRegistry[registryLimitsKey(finalModel)]; ok && entry.MaxContext > 0 {
		return entry.MaxContext
	}
	return 0
}

// resolveTransformInputBudget derives the input-conversation budget for the
// transform-side TrimPayload call: util.DeriveInputTokenBudget(maxContext,
// maxOutput), i.e. 3/4 of the model's context window further clamped to the
// input room left after reserving the effective output cap. Reserving output
// room keeps input + output within the window (the interceptor hard limit in
// proxy/chat.go resolvePipelineContext uses the same helper on the primary
// target; this per-target call is the failover-safe guard). The budget is never
// the output cap alone: clamping input to the output cap would over-trim
// large-context models. context.hard_limit_tokens, when configured, replaces the
// budget outright. The agent-resolved maxContext takes precedence;
// catalog/registry values are the fallback. Returns 0 (trim disabled) when the
// context window is unknown and no hard limit is set, per the documented
// UNKNOWN_MAXCONTEXT fallback.
func resolveTransformInputBudget(deps TransformDeps, finalModel string, maxContext, maxOutput int) int {
	if hardLimit := deps.Config.Context.HardLimitTokens; hardLimit > 0 {
		return hardLimit
	}
	maxCtx := maxContext
	if maxCtx <= 0 {
		maxCtx = resolveEffectiveMaxContext(deps, finalModel)
	}
	return util.DeriveInputTokenBudget(maxCtx, maxOutput)
}

func applyMaxTokens(payload map[string]interface{}, effectiveMaxOutput int) {
	if effectiveMaxOutput <= 0 {
		return
	}
	if _, hasMaxTokens := payload["max_tokens"]; !hasMaxTokens {
		// Store the canonical numeric type a JSON decode produces, so later
		// comparisons (duplicate resolution, conversions) never no-op on a
		// type mismatch (NENYA-186).
		payload["max_tokens"] = float64(effectiveMaxOutput)
		return
	}
	v, ok := payload["max_tokens"].(float64)
	if !ok || v <= float64(effectiveMaxOutput) {
		return
	}
	payload["max_tokens"] = effectiveMaxOutput
}

// tokenCapSnapshot remembers the token-cap fields before the pair-driven
// normalization renamed one, so the shared payload map is handed to the next
// failover target in its original shape (NENYA-179 review).
type tokenCapSnapshot struct {
	maxTokens     interface{}
	hadMaxTokens  bool
	completionTok interface{}
	hadCompletion bool
}

func snapshotTokenCaps(payload map[string]interface{}) tokenCapSnapshot {
	snap := tokenCapSnapshot{}
	snap.maxTokens, snap.hadMaxTokens = payload["max_tokens"]
	snap.completionTok, snap.hadCompletion = payload["max_completion_tokens"]
	return snap
}

func (s tokenCapSnapshot) restore(payload map[string]interface{}) {
	if s.hadMaxTokens {
		payload["max_tokens"] = s.maxTokens
	} else {
		delete(payload, "max_tokens")
	}
	if s.hadCompletion {
		payload["max_completion_tokens"] = s.completionTok
	} else {
		delete(payload, "max_completion_tokens")
	}
}

func restoreOriginalModel(payload map[string]interface{}, origModel interface{}) {
	if origModel == nil {
		delete(payload, "model")
	} else {
		payload["model"] = origModel
	}
}

// applyParamCompatSanitize applies the param-compat table (NENYA-32):
// config-declared rules first, then the built-in rejections (Gemini 3
// sampling/candidate_count, reasoning-effort floors, forced tool_choice
// relaxation). Changes are logged; metrics stay in the proxy layer where
// agent/provider labels are available.
func applyParamCompatSanitize(deps TransformDeps, payload map[string]interface{}, modelName string) {
	if modelName == "" {
		return
	}
	rules := resolveParamCompatRules(&deps.Config.Governance)
	applied := applyParamCompat(rules, payload, modelName)
	for _, param := range applied {
		deps.Logger.Debug("param-compat rule applied", "model", modelName, "param", param)
	}
}

// applyAutoCacheBreakpoints implements the opt-in automatic prompt-cache
// anchoring (NENYA-185). It only ever ENABLES the gateway's own markers; it
// never disables or rewrites one the client placed.
//
// Policy (all must hold to inject):
//   - the agent opted in (agents.<name>.auto_cache_breakpoints),
//   - the prefix cache is enabled and not in automatic mode (automatic mode
//     emits a single top-level marker and forces block markers off, so
//     enabling them would be a recorded no-op),
//   - the client sent NO cache_control mark anywhere in the request (a bounded
//     deep scan: system blocks, message content blocks, tools, and the
//     extra_body/extra_content escape hatches),
//   - the request has prior history (>= 2 messages that are not gateway
//     trailers: a one-shot turn has no stable boundary, and the gateway's own
//     canary message must not be mistaken for history),
//   - the content pipeline did NOT window-compact the history and this
//     dispatch did NOT trim it: both rewrite the message array, and an
//     anchored boundary must be a client-stable prefix, not a gateway-produced
//     head.
//
// Every outcome is recorded, including the stand-downs, so an operator can see
// why anchoring did not happen.
func applyAutoCacheBreakpoints(deps TransformDeps, payload map[string]interface{}, pc config.PrefixCacheConfig, opts *adapter.CacheOpts, historyTrimmed bool) {
	enabled := false
	if agent, ok := deps.Config.Agents[deps.AgentName]; ok && agent.AutoCacheBreakpoints != nil {
		enabled = *agent.AutoCacheBreakpoints
	}
	if !enabled {
		return // not opted in: no telemetry (the knob is off by default)
	}
	if !pc.Enabled {
		deps.Metrics.RecordAutoCacheBreakpoint("skipped_disabled")
		return
	}
	if pc.CacheMode != nil && *pc.CacheMode == config.CacheModeAutomatic {
		deps.Metrics.RecordAutoCacheBreakpoint("skipped_automatic_mode")
		return
	}
	if deps.WindowCompacted {
		deps.Metrics.RecordAutoCacheBreakpoint("skipped_windowed")
		return
	}
	if historyTrimmed {
		deps.Metrics.RecordAutoCacheBreakpoint("skipped_trimmed")
		return
	}
	if opts.System && opts.Messages {
		// The explicit prefix-cache defaults already anchor this request; the
		// knob changed nothing, which the outcome says instead of claiming an
		// injection.
		deps.Metrics.RecordAutoCacheBreakpoint("already_anchored")
		return
	}
	if hasClientCacheControl(payload, deps.Logger) {
		deps.Metrics.RecordAutoCacheBreakpoint("skipped_client_marks")
		return
	}
	stableMessages := countStableHistoryMessages(payload)
	if stableMessages < 2 {
		deps.Metrics.RecordAutoCacheBreakpoint("skipped_one_shot")
		return
	}
	// Anchor the stable prefix: the end of the leading system run (the system
	// block marker) and the last stable history message (the adapter's
	// message marker lands on the last-but-one message — with two stable
	// messages that is the first of them, so the system anchor is the one
	// that always lands).
	opts.System = true
	opts.Messages = true
	deps.Metrics.RecordAutoCacheBreakpoint("injected")
	if deps.Logger != nil {
		deps.Logger.Debug("auto cache breakpoints injected",
			"agent", deps.AgentName, "messages", stableMessages)
	}
}

// countStableHistoryMessages counts the request's messages that are real
// conversation turns: the gateway's own trailing canary marker is not history,
// so a one-shot request with a canary stays one-shot (NENYA-185 review).
func countStableHistoryMessages(payload map[string]interface{}) int {
	msgs, ok := payload["messages"].([]interface{})
	if !ok {
		return 0
	}
	stable := 0
	for _, raw := range msgs {
		msg, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if content, ok := msg["content"].(string); ok && strings.Contains(content, pipeline.CanaryPrefix) {
			continue
		}
		stable++
	}
	return stable
}

// hasClientCacheControl reports whether the request carries a cache_control
// mark anywhere a client could have placed one — including the extra_body /
// extra_content escape hatches, where a harness can hide a breakpoint the
// top-level scan would miss. The walk is bounded by depth and node count so a
// hostile body cannot turn the check into a denial of service.
func hasClientCacheControl(payload map[string]interface{}, depsLogger *slog.Logger) bool {
	const maxDepth = 12
	const maxNodes = 20000
	nodes := 0
	truncated := false
	var walk func(v interface{}, depth int) bool
	walk = func(v interface{}, depth int) bool {
		if depth > maxDepth || nodes > maxNodes {
			// Fail open (anchor anyway) but never invisibly: the scan was
			// truncated, so "no client mark" is not a proven statement.
			truncated = true
			return false
		}
		nodes++
		switch t := v.(type) {
		case map[string]interface{}:
			if _, marked := t["cache_control"]; marked {
				return true
			}
			for _, child := range t {
				if walk(child, depth+1) {
					return true
				}
			}
		case []interface{}:
			for _, child := range t {
				if walk(child, depth+1) {
					return true
				}
			}
		}
		return false
	}
	found := walk(payload, 0)
	if truncated && depsLogger != nil {
		depsLogger.Debug("client cache_control scan truncated; treating the request as unmarked")
	}
	return found
}

// convertToAnthropicFormat converts an OpenAI-format payload to Anthropic format,
// applying prefix cache configuration from agent config.
// Returns the converted payload; the caller must use the return value as the
// replacement for the input payload.
func convertToAnthropicFormat(deps TransformDeps, payload map[string]interface{}, modelName string, historyTrimmed bool) map[string]interface{} {
	stream := false
	if s, ok := payload["stream"].(bool); ok {
		stream = s
	}
	anthropicAdapter := adapter.GetAnthropicAdapter()
	pc := deps.Config.PrefixCache
	cacheSystem := pc.CacheSystem != nil && *pc.CacheSystem && pc.Enabled
	cacheTools := pc.CacheTools != nil && *pc.CacheTools && pc.Enabled
	cacheMessages := pc.CacheMessages != nil && *pc.CacheMessages && pc.Enabled
	cacheAutomatic := pc.Enabled && pc.CacheMode != nil && *pc.CacheMode == config.CacheModeAutomatic
	ttl := pc.CacheControlTTL
	if ttl == "" {
		ttl = "ephemeral"
	}
	opts := adapter.CacheOpts{
		// The egress canary marker varies every request: keep it out of
		// the cache-marked system block (NENYA-171).
		TrailingContextMarkers: []string{pipeline.CanaryPrefix},
		System:                 cacheSystem,
		Tools:                  cacheTools,
		Messages:               cacheMessages,
		Automatic:              cacheAutomatic,
		GlobalTTL:              ttl,
	}
	if pc.CacheSystemTTL != nil {
		opts.SystemTTL = *pc.CacheSystemTTL
	}
	if pc.CacheToolsTTL != nil {
		opts.ToolsTTL = *pc.CacheToolsTTL
	}
	if pc.CacheMessagesTTL != nil {
		opts.MessagesTTL = *pc.CacheMessagesTTL
	}
	applyMidConversationVerdict(deps, modelName, &opts)
	applyAutoCacheBreakpoints(deps, payload, pc, &opts, historyTrimmed)
	return anthropicAdapter.ConvertOpenAIToAnthropicBody(payload, modelName, stream, opts)
}

func TransformRequestForUpstream(deps TransformDeps, providerName, upstreamURL string, payload map[string]interface{}, model string, maxOutput int, maxContext int, format string, reasoningEffort string) ([]byte, string, error) {
	origModel := payload["model"]

	if model != "" {
		payload["model"] = model
	}

	modelRaw, ok := payload["model"]
	if !ok {
		restoreOriginalModel(payload, origModel)
		return nil, "", nil
	}

	modelName, ok := modelRaw.(string)
	if !ok {
		restoreOriginalModel(payload, origModel)
		return nil, "", nil
	}

	finalModel := resolveModelMapping(deps, payload, providerName, modelName)

	if reasoningEffort != "" {
		if _, exists := payload["reasoning_effort"]; !exists {
			// Inject default reasoning_effort from agent config.
			// Client-specified reasoning_effort always takes precedence.
			payload["reasoning_effort"] = reasoningEffort
		}
	}

	applyProviderSanitize(deps, payload, providerName)
	SanitizePayload(deps, payload, modelName, format)
	applyParamCompatSanitize(deps, payload, modelName)
	injectPromptCacheKey(deps, payload, providerName, modelName)
	injectOpenAICacheBreakpoints(deps, payload, providerName, modelName)
	injectCacheSalt(deps, payload, providerName)
	resolveAgentSystemPrompt(deps, payload, origModel, providerName)

	effectiveMaxOutput := resolveEffectiveMaxOutput(deps, finalModel, maxOutput)
	applyMaxTokens(payload, effectiveMaxOutput)
	// The pair-driven normalization may RENAME the token cap to the spelling a
	// target declares; the payload map is shared across failover targets of a
	// corrective retry, so the original spelling must be restored after
	// marshaling or the next (possibly tolerant) target inherits it
	// (NENYA-179 review). The rule set matches the pre-mapping model ID, the
	// same ID param-compat sanitization above used.
	capSnapshot := snapshotTokenCaps(payload)
	normalizeTargetParams(deps, payload, resolveParamCompatRules(&deps.Config.Governance), modelName, deps.SourceFormat)

	inputBudget := resolveTransformInputBudget(deps, finalModel, maxContext, util.EffectiveOutputTokens(payload, effectiveMaxOutput))
	historyTrimmed := false
	if deps.CountTokens != nil && inputBudget > 0 {
		modified, saved := pipeline.TrimPayload(deps.Logger, payload, inputBudget, deps.CountTokens, deps.Config.Context)
		historyTrimmed = modified
		if modified {
			deps.Metrics.RecordTokensSaved("trim", saved)
			deps.Logger.Info("payload trimmed to fit token budget",
				"input_budget", inputBudget,
				"saved_tokens", saved)
		}
	}

	auditRequestFields(deps, payload, format)

	// The restores below must run on the ORIGINAL map: the anthropic
	// conversion rebinds payload to a new map, so restoring after the
	// rebind would mutate a discarded map and leak the rename into the
	// shared failover payload (NENYA-179 review).
	sharedPayload := payload
	if format == "anthropic" {
		// Convert with the POST-mapping model: the upstream must receive
		// the aliased ID, not the canonical one the client sent (NENYA-22).
		payload = convertToAnthropicFormat(deps, payload, finalModel, historyTrimmed)
	}

	newBody, err := json.Marshal(payload)
	restoreOriginalModel(sharedPayload, origModel)
	capSnapshot.restore(sharedPayload)

	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal transformed request: %v", err)
	}

	return newBody, finalModel, nil
}

// applyMidConversationVerdict sets the mid-conversation-system override
// only when the catalog carries an EXPLICIT positive verdict (NENYA-171):
// metadata without a verdict must not force the legacy hoisting path over
// the adapter's family inference.
func applyMidConversationVerdict(deps TransformDeps, modelName string, opts *adapter.CacheOpts) {
	if deps.Catalog == nil {
		return
	}
	m, ok := deps.Catalog.Lookup(modelName)
	if !ok || !m.HasCapability(discovery.CapMidConversationSystem) {
		return
	}
	midConvo := true
	opts.MidConversationSystem = &midConvo
}

// reservedTransformHeaders are first-party response headers the gateway
// populates itself; an upstream must not inject them via CopyHeaders or its
// value would sit alongside the gateway's own merged annotations
// (X-Nenya-Dropped-Content, NENYA-176).
var reservedTransformHeaders = map[string]struct{}{
	strings.ToLower(adapter.HeaderDroppedContent): {},
}

// CopyHeaders copies upstream response headers onto the client response,
// skipping hop-by-hop headers and the gateway's reserved first-party
// channels.
func CopyHeaders(src, dst http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "content-length", "content-encoding", "upgrade",
			"transfer-encoding", "te", "trailers", "proxy-authenticate",
			"proxy-authorization", "keep-alive", "proxy-connection":
			continue
		}
		if _, reserved := reservedTransformHeaders[lk]; reserved {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// SliceContains reports whether needle exists in haystack.
func SliceContains(haystack []int, needle int) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TransformIncomingAnthropicRequest converts an Anthropic-format request body to
// OpenAI chat completions format. This is called when the gateway receives a request
// from an Anthropic-native client (e.g., Claude Desktop) using the /v1/messages endpoint.
// The converted request is then processed through the standard OpenAI-format pipeline.
func TransformIncomingAnthropicRequest(ctx context.Context, body []byte) ([]byte, error) {
	return TransformIncomingAnthropicRequestReported(ctx, body, nil)
}

// TransformIncomingAnthropicRequestReported is TransformIncomingAnthropicRequest
// with a DropReport: messages carrying content the OpenAI shape cannot
// represent (thinking signatures, documents, unknown block types) are recorded
// by kind instead of dropped silently (NENYA-176). Nil report behaves exactly
// like the unreported variant.
func TransformIncomingAnthropicRequestReported(ctx context.Context, body []byte, report *adapter.DropReport) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	var anthropic map[string]interface{}
	if err := json.Unmarshal(body, &anthropic); err != nil {
		return nil, fmt.Errorf("failed to unmarshal request: %w", err)
	}

	// Native detection (NENYA-171): a top-level "type" is a RESPONSE field
	// the Messages request schema does not define, so a real native body
	// (model + max_tokens + messages + optional native system blocks) must
	// also be recognized by landmark — otherwise it is treated as OpenAI
	// and reverse-converted, silently dropping its system prompt.
	_, hasType := anthropic["type"]
	_, hasMessages := anthropic["messages"]
	_, hasSystem := anthropic["system"]
	nativeLandmarks := hasType || hasSystem
	if !nativeLandmarks || !hasMessages {
		return body, nil
	}
	if _, looksOpenAI := anthropic["max_completion_tokens"]; looksOpenAI {
		return body, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	openai := adapter.GetAnthropicAdapter().ConvertAnthropicRequestToOpenAIBodyReported(anthropic, report)

	out, err := json.Marshal(openai)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal converted OpenAI request: %w", err)
	}
	return out, nil
}

// ExtractField performs a lightweight JSON field extraction without full unmarshaling.
// Returns the value and true if the field exists, nil and false otherwise.
func ExtractField(body []byte, field string) (interface{}, bool) {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return nil, false
	}
	v, ok := data[field]
	return v, ok
}
