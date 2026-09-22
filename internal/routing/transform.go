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
)

// TransformDeps provides the dependencies needed for request payload
// transformation (provider lookup, API key injection, metrics, etc.).
type TransformDeps struct {
	Logger             *slog.Logger
	Providers          map[string]*config.Provider
	Config             *config.Config
	ThoughtSigCache    *infra.ThoughtSignatureCache
	ExtractContentText func(msg map[string]interface{}) string
	Catalog            *discovery.ModelCatalog
	CountTokens        func(string) int
	AgentName          string
	CacheSalt          string
	Metrics            *infra.Metrics
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
		if mapped, ok := spec.ModelMap[strings.ToLower(modelName)]; ok {
			finalModel = mapped
		}
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
// providers that support prompt caching (xAI, OpenAI). The key is derived from
// the agent name and model, giving providers a per-session cache key to improve
// prompt-cache hit rates. Controlled by prefix_cache.enabled in config.
//
// The key is SHA-256(agentName:model) truncated to 16 hex chars (64 bits),
// providing sufficient entropy for per-session cache routing with low collision
// risk across typical agent+model counts (< 1M combinations).
func injectPromptCacheKey(deps TransformDeps, payload map[string]interface{}, providerName, modelName string) {
	if deps.Config == nil || !deps.Config.PrefixCache.Enabled {
		return
	}

	lower := strings.ToLower(providerName)
	if lower != "xai" && lower != "openai" {
		return
	}

	if _, exists := payload["prompt_cache_key"]; exists {
		return
	}

	if deps.AgentName == "" {
		if deps.Logger != nil {
			deps.Logger.Debug("skipped prompt_cache_key injection: empty agent name",
				"provider", providerName, "model", modelName)
		}
		return
	}

	// SHA-256 is used to derive a stable cache key from public identifiers
	// (agent name, model name). This is not password hashing — the input
	// data is non-secret (it's already transmitted to the upstream provider).
	// We need fast, deterministic hashing for cache key stability; Argon2/
	// scrypt/bcrypt would be inappropriate (too slow, unnecessary salt).
	h := sha256.Sum256([]byte(deps.AgentName + ":" + modelName))
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
		if entry, ok := config.ModelRegistry[finalModel]; ok && entry.MaxOutput > 0 {
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
	if entry, ok := config.ModelRegistry[finalModel]; ok && entry.MaxContext > 0 {
		return entry.MaxContext
	}
	return 0
}

// resolveTransformInputBudget derives the input-conversation budget for the
// transform-side TrimPayload call: 3/4 of the model's context window
// (reserving output headroom — same 3/4 policy as the interceptor hard limit
// computed in proxy/chat.go resolvePipelineContext), clamped to
// context.hard_limit_tokens when configured. The agent-resolved maxContext
// takes precedence; catalog/registry values are the fallback. Returns 0 (trim
// disabled) when the context window is unknown and no hard limit is set, per
// the documented UNKNOWN_MAXCONTEXT fallback. The budget is deliberately
// derived from MaxContext, never from MaxOutput: TrimPayload's parameter is an
// input-conversation budget, and clamping input to the output cap
// over-trimmed large-context models.
func resolveTransformInputBudget(deps TransformDeps, finalModel string, maxContext int) int {
	if hardLimit := deps.Config.Context.HardLimitTokens; hardLimit > 0 {
		return hardLimit
	}
	maxCtx := maxContext
	if maxCtx <= 0 {
		maxCtx = resolveEffectiveMaxContext(deps, finalModel)
	}
	if maxCtx <= 0 {
		return 0
	}
	// maxCtx/4*3 cannot overflow for any positive int (unlike maxCtx*3/4);
	// degenerate windows under 4 tokens yield 0, disabling the trim.
	return maxCtx / 4 * 3
}

func applyMaxTokens(payload map[string]interface{}, effectiveMaxOutput int) {
	if effectiveMaxOutput <= 0 {
		return
	}
	if _, hasMaxTokens := payload["max_tokens"]; !hasMaxTokens {
		payload["max_tokens"] = effectiveMaxOutput
		return
	}
	v, ok := payload["max_tokens"].(float64)
	if !ok || v <= float64(effectiveMaxOutput) {
		return
	}
	payload["max_tokens"] = effectiveMaxOutput
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

// convertToAnthropicFormat converts an OpenAI-format payload to Anthropic format,
// applying prefix cache configuration from agent config.
// Returns the converted payload; the caller must use the return value as the
// replacement for the input payload.
func convertToAnthropicFormat(deps TransformDeps, payload map[string]interface{}, modelName string) map[string]interface{} {
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
		System:    cacheSystem,
		Tools:     cacheTools,
		Messages:  cacheMessages,
		Automatic: cacheAutomatic,
		GlobalTTL: ttl,
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
	if m, ok := deps.Catalog.Lookup(modelName); ok {
		// Catalog-merged metadata (config overrides > discovered >
		// static) is authoritative; only an explicit capability verdict
		// overrides the adapter's family inference.
		midConvo := m.HasCapability(discovery.CapMidConversationSystem)
		opts.MidConversationSystem = &midConvo
	}
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
	SanitizePayload(deps, payload, modelName)
	applyParamCompatSanitize(deps, payload, modelName)
	injectPromptCacheKey(deps, payload, providerName, modelName)
	injectOpenAICacheBreakpoints(deps, payload, providerName, modelName)
	injectCacheSalt(deps, payload, providerName)
	resolveAgentSystemPrompt(deps, payload, origModel, providerName)

	effectiveMaxOutput := resolveEffectiveMaxOutput(deps, finalModel, maxOutput)
	applyMaxTokens(payload, effectiveMaxOutput)

	inputBudget := resolveTransformInputBudget(deps, finalModel, maxContext)
	if deps.CountTokens != nil && inputBudget > 0 {
		modified, saved := pipeline.TrimPayload(deps.Logger, payload, inputBudget, deps.CountTokens, deps.Config.Context)
		if modified {
			deps.Metrics.RecordTokensSaved("trim", saved)
			deps.Logger.Info("payload trimmed to fit token budget",
				"input_budget", inputBudget,
				"saved_tokens", saved)
		}
	}

	if format == "anthropic" {
		// Convert with the POST-mapping model: the upstream must receive
		// the aliased ID, not the canonical one the client sent (NENYA-22).
		payload = convertToAnthropicFormat(deps, payload, finalModel)
	}

	newBody, err := json.Marshal(payload)
	restoreOriginalModel(payload, origModel)

	if err != nil {
		return nil, "", fmt.Errorf("failed to marshal transformed request: %v", err)
	}

	return newBody, finalModel, nil
}

// CopyHeaders copies non-hop-by-hop headers from src to dst, filtering
// out connection-specific headers (Connection, Content-Length, Transfer-Encoding, etc.).
func CopyHeaders(src, dst http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "content-length", "content-encoding", "upgrade",
			"transfer-encoding", "te", "trailers", "proxy-authenticate",
			"proxy-authorization", "keep-alive", "proxy-connection":
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

	_, hasType := anthropic["type"]
	_, hasMessages := anthropic["messages"]
	if !hasType || !hasMessages {
		return body, nil
	}

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	a := adapter.GetAnthropicAdapter()
	openai := a.ConvertAnthropicRequestToOpenAIBody(anthropic)

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
