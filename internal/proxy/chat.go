package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/adapter"
	"github.com/nenya/internal/billing"
	"github.com/nenya/internal/discovery"
	"github.com/nenya/internal/gateway"
	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/pipeline"
	"github.com/nenya/internal/routing"
	"github.com/nenya/internal/util"
)

// chatRequest holds the validated request data extracted from an incoming
// /v1/chat/completions or /v1/messages payload.
type chatRequest struct {
	Payload      map[string]any
	ModelName    string
	TokenCount   int
	Stream       bool
	Targets      []routing.UpstreamTarget
	AgentName    string
	Cooldown     time.Duration
	MaxRetries   int
	CacheKey     string
	HasMCPTools  bool
	SoftLimit    int
	HardLimit    int
	WindowMaxCtx int
	Profile      pipeline.ClientProfile
	Messages     []any
	KeyRef       string
	SourceFormat string // "openai" or "anthropic" - indicates the original client request format
	// Agent is the resolved agent config when ModelName names an agent
	// (zero value for direct model routes). Set once in resolveRouting;
	// all downstream consumers (sticky routing, MCP flow, session header
	// synthesis) share this resolution.
	Agent config.AgentConfig

	// Memoized sticky-session key (see derivedSessionKey). Memoization
	// sentinel: sessionKeyDone; the fields are only touched on the request
	// goroutine before upstream dispatch begins, so no lock is needed.
	sessionKey     string
	sessionKeyOK   bool
	sessionKeyDone bool

	// Memoized affinity-resolved session identity (see sessionIdentity):
	// the root session key, or a deterministic fork key when the leading
	// turns collide with a different conversation (NENYA-39). Same
	// single-goroutine discipline as the raw key fields.
	affinityKey  string
	affinityDone bool
}

// sessionIdentity returns the memoized affinity-resolved session identity:
// the raw session key when the leading turns continue the remembered
// conversation, or a deterministic fork key when they collide with a
// different conversation sharing the same opener (NENYA-39).
func (req *chatRequest) sessionIdentity(gw *gateway.NenyaGateway) (string, bool) {
	raw, ok := req.derivedSessionKey()
	if !ok {
		return raw, ok
	}
	if req.affinityDone {
		return req.affinityKey, true
	}
	req.affinityKey = raw
	if gw != nil && gw.AgentState != nil && gw.AgentState.Affinity != nil {
		req.affinityKey = gw.AgentState.Affinity.ResolveIdentity(raw, routing.BuildAffinityChain(req.Payload))
	}
	req.affinityDone = true
	return req.affinityKey, true
}

// httpError pairs an HTTP status code with a user-facing message and an
// optional structured error kind. When Kind is empty the renderer falls back
// to invalid_request.
type httpError struct {
	Code    int
	Message string
	Kind    infra.ErrorKind
}

func (e *httpError) Error() string { return e.Message }

// parseRequestBody parses the request body and converts it to OpenAI format if needed.
// Returns the parsed payload, source format ("openai" or "anthropic"), and an error if parsing fails.
func (p *Proxy) parseRequestBody(gw *gateway.NenyaGateway, r *http.Request, bodyBytes []byte) (map[string]any, string, *httpError) {
	sourceFormat := "openai"
	// Hot-path peek (NENYA-35): scan for the top-level "type" field without
	// unmarshaling the whole request into a map.
	if _, hasType := util.ExtractTopLevelField(bodyBytes, "type"); hasType {
		converted, err := routing.TransformIncomingAnthropicRequest(r.Context(), bodyBytes)
		if err != nil {
			gw.Logger.Warn("failed to convert Anthropic request", "err", err)
			return nil, "", &httpError{Code: http.StatusBadRequest, Message: "Failed to convert Anthropic format request"}
		}
		if converted != nil && string(converted) != string(bodyBytes) {
			sourceFormat = "anthropic"
			bodyBytes = converted
		}
	}

	var payload map[string]any
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		gw.Logger.Warn("failed to parse JSON, returning Bad Request")
		return nil, "", &httpError{Code: http.StatusBadRequest, Message: "Invalid JSON payload"}
	}
	return payload, sourceFormat, nil
}

// validateChatRequest reads and validates the incoming request body,
// returning a populated chatRequest or an httpError.
func (p *Proxy) validateChatRequest(w http.ResponseWriter, r *http.Request, gw *gateway.NenyaGateway, keyRef string) (*chatRequest, *httpError) {
	r.Body = http.MaxBytesReader(w, r.Body, gw.Config.Server.MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()

	bodyBytes, herr := readRequestBody(r, gw, "failed to read request body")
	if herr != nil {
		return nil, herr
	}

	if r.Context().Err() != nil {
		// The client is gone; there is no response worth rendering.
		return nil, errClientClosed
	}

	payload, sourceFormat, herr := p.parseRequestBody(gw, r, bodyBytes)
	if herr != nil {
		return nil, herr
	}

	modelName, ok := payload["model"].(string)
	if !ok || modelName == "" {
		gw.Logger.Warn("missing or empty model field in request body")
		return nil, &httpError{Code: http.StatusBadRequest, Message: `Missing or empty "model" field in request body`}
	}
	if len(modelName) > MaxModelNameLength {
		gw.Logger.Warn("model name exceeds maximum length", "length", len(modelName))
		return nil, &httpError{Code: http.StatusBadRequest, Message: "Model name too long"}
	}

	req := &chatRequest{
		Payload:      payload,
		ModelName:    modelName,
		TokenCount:   gw.CountRequestTokens(payload),
		Stream:       false,
		KeyRef:       keyRef,
		SourceFormat: sourceFormat,
	}
	if streamRaw, ok := payload["stream"]; ok {
		if s, ok := streamRaw.(bool); ok {
			req.Stream = s
		}
	}

	req.Targets, req.AgentName, req.Cooldown, req.MaxRetries, herr = p.resolveRouting(r.Context(), req, gw)
	if herr != nil {
		return nil, herr
	}

	req.CacheKey, herr = p.resolveCache(w, r, gw, req)
	if herr != nil {
		return nil, herr
	}

	req.Messages, req.HasMCPTools, req.SoftLimit, req.HardLimit, req.WindowMaxCtx, req.Profile = p.resolvePipelineContext(r, gw, req)

	// Record the client-side (pre-pipeline) token estimate once per request
	// so Prometheus can derive whole-pipeline savings as client - input.
	provider := ""
	if len(req.Targets) > 0 {
		provider = req.Targets[0].Provider
	}
	gw.Metrics.RecordTokens("client", req.ModelName, req.AgentName, provider, req.TokenCount)

	return req, nil
}

// resolveRouting determines the upstream targets, agent name, cooldown, and
// max retries for the given model.
func (p *Proxy) resolveRouting(ctx context.Context, req *chatRequest, gw *gateway.NenyaGateway) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	agent, hasAgent := gw.Config.Agents[req.ModelName]
	if hasAgent {
		req.Agent = agent
	}
	// Derive the session identity eagerly right after agent resolution so
	// every consumer (sticky pin, session-header synthesis) reads the same
	// memoized value derived from the resolved agent — no caller can derive
	// from an unresolved agent. The identity includes the NENYA-39 affinity
	// fork check over the leading turns.
	req.sessionIdentity(gw)
	if hasAgent {
		return p.resolveAgentRouting(ctx, req, gw, agent)
	}
	return p.resolveModelRouting(ctx, req, gw)
}

func (p *Proxy) resolveAgentRouting(ctx context.Context, req *chatRequest, gw *gateway.NenyaGateway, agent config.AgentConfig) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	req.AgentName = req.ModelName
	cooldown := getAgentCooldown(agent)
	maxRetries := agent.MaxRetries

	strategy := getAgentStrategy(agent)

	// Resolve the sticky session pin before target build so the pinned
	// account can steer credential selection via the account preference. The
	// read is non-touching (Peek): failed builds must not extend pin TTL.
	// Session keys resolve for EVERY strategy (NENYA-29): non-sticky agents
	// use the pin for credential-only affinity (no reordering), keeping
	// upstream per-key prompt caches warm under multi-account rotation.
	sticky := resolveStickyPin(req, gw)

	built := gw.AgentState.BuildTargetList(ctx, routing.TargetBuildOpts{
		Logger:          gw.Logger,
		AgentName:       req.ModelName,
		Agent:           agent,
		TokenCount:      req.TokenCount,
		Providers:       gw.Providers,
		Catalog:         gw.ModelCatalog,
		AutoContextSkip: gw.Config.Governance.AutoContextSkip != nil && *gw.Config.Governance.AutoContextSkip,
		AccountSelector: gw,
		Preferred:       sticky.preference(),
	})
	targets := filterExhaustedTargets(built, gw.BillingTracker, gw.Providers, gw.Logger)
	if len(targets) == 0 {
		if len(built) > 0 {
			// Every built target was dropped (billing exhaustion or missing
			// credentials): a 503 distinguishes this from the context-fit
			// 413 that handleEmptyAgentTargets reports for build-time skips.
			// quota_exhausted deliberately overloads both causes — it tells
			// retry-aware clients to back off, which is correct for
			// exhaustion and harmless for the rarer credential
			// misconfiguration.
			gw.Logger.Warn("all built targets filtered",
				"agent", req.ModelName, "built", len(built))
			return nil, "", 0, 0, &httpError{
				Code:    http.StatusServiceUnavailable,
				Message: "All provider accounts are exhausted or unavailable",
				Kind:    infra.ErrorKindQuotaExhausted,
			}
		}
		return handleEmptyAgentTargets(req, gw, agent)
	}

	if gw.Config.Governance.AutoReorderByLatency != nil && *gw.Config.Governance.AutoReorderByLatency {
		targets = reorderTargetsByLatency(req, gw, targets)
	}

	targets = applyStickyRouting(req, gw, agent, targets, sticky, strategy == discovery.AgentStrategySticky)

	gw.Logger.Info("agent routing",
		"agent", req.ModelName, "strategy", strategy, "models_in_chain", len(targets))

	return targets, req.AgentName, cooldown, maxRetries, nil
}

// getAgentStrategy returns the agent's routing strategy, defaulting to
// round-robin when unset.
func getAgentStrategy(agent config.AgentConfig) string {
	if agent.Strategy == "" {
		return discovery.AgentStrategyRoundRobin
	}
	return agent.Strategy
}

// stickyPin holds the sticky session context resolved once before target
// build: the session key and the current pin snapshot (read via Peek, which
// does not refresh LastSeen — only a request that completes routing extends
// the pin, via the Lookup inside applyStickyRouting). Account-preference
// steering and reorder/promotion reuse this single observation.
type stickyPin struct {
	key   string
	state routing.SessionState
}

// preference returns the pinned account for preferred credential selection
// during target build, or nil when there is no account-bearing pin.
func (s *stickyPin) preference() *routing.AccountPreference {
	if s == nil || s.state.Account == "" {
		return nil
	}
	return &routing.AccountPreference{
		Provider:  s.state.Provider,
		Model:     s.state.Model,
		AccountID: s.state.Account,
	}
}

// resolveStickyPin resolves the sticky session context for a request: the
// session key and, when present, the currently pinned provider/model/account
// (read via Peek — no LastSeen refresh, no expiry side effects). Returns nil
// when sticky routing cannot apply (no session router, or no session key
// derivable from the request). Reads req.Agent, resolved in resolveRouting.
func resolveStickyPin(req *chatRequest, gw *gateway.NenyaGateway) *stickyPin {
	if gw.AgentState == nil || gw.AgentState.SessionRouter == nil {
		return nil
	}
	key, ok := req.sessionIdentity(gw)
	if !ok {
		return nil
	}
	pin, found := gw.AgentState.SessionRouter.Peek(key)
	if !found {
		return &stickyPin{key: key}
	}
	return &stickyPin{key: key, state: pin}
}

// sessionStickyKeysEnabled reports whether a provider opted out of
// session-sticky credential pinning (NENYA-29). Unset = enabled; an explicit
// false disables pinning and preference steering for that provider.
func sessionStickyKeysEnabled(gw *gateway.NenyaGateway, providerName string) bool {
	if pr, ok := gw.Providers[providerName]; ok && pr.SessionStickyKeys != nil {
		return *pr.SessionStickyKeys
	}
	return true
}

// applyStickyRouting pins the session to its previously selected provider,
// model, and account when the pin is still valid (active, present in the
// built target list). When reorder is true (sticky strategy) the pinned
// target is moved to the front of the list, leaving the remainder as the
// ordered failover tail. When reorder is false (fallback/round-robin
// strategies, NENYA-29) the list order is untouched — the pin is used purely
// for credential affinity: prefer the pinned account when the front target
// matches the pin, and record the front target's provider/model/account so
// the session's next request reuses the same upstream credential.
// Promotion paths, each recorded as a failover pin-change via
// SessionRouter.PromoteIfChanged:
//
//   - Pinned account drift: the pinned provider/model is active at the front
//     but serves a different, non-empty account — the pinned account was
//     cooling, exhausted, or model-locked during target build and the LRU
//     fallback selected a sibling account. The pin follows the sibling.
//   - Pinned provider/model lost: the pinned target is cooling, was filtered
//     out (billing exhaustion), or no longer exists — the pin is promoted to
//     the first active target (existing failover behavior). Unlike the drift
//     path, this promotion is unconditional even when the new target carries
//     an empty account: the old provider/model is gone, so an empty-account
//     pin (self-healing on the next successful resolution) beats a pin stuck
//     on a lost target, and single-account legacy providers always run with
//     empty account names.
//
// A pin that still matches the served provider/model and account is left
// untouched: no re-pin, no failover metric, no Since reset. Providers that
// opted out via session_sticky_keys=false never pin or promote.
func applyStickyRouting(req *chatRequest, gw *gateway.NenyaGateway, agent config.AgentConfig, targets []routing.UpstreamTarget, sticky *stickyPin, reorder bool) []routing.UpstreamTarget {
	if sticky == nil || gw.AgentState == nil || gw.AgentState.SessionRouter == nil {
		return targets
	}

	ttl := agentStickyTTL(agent)

	// Refresh LastSeen only now that routing succeeded (targets non-empty).
	pin, found := gw.AgentState.SessionRouter.Lookup(sticky.key)
	if !found {
		pinSessionTarget(gw, sticky.key, targets, ttl)
		return targets
	}

	i := indexOfActiveTarget(targets, pin.Provider, pin.Model)
	if reorder && i > 0 {
		targets = moveToFront(targets, i)
		i = 0
	}
	if i == 0 {
		promoteOnFrontDrift(gw, req, sticky.key, pin, targets, ttl)
		return targets
	}
	promoteToFirstActive(gw, req, sticky.key, pin, targets, ttl)
	return targets
}

// pinSessionTarget records the first active target as the session's pin when
// the target's provider participates in session-sticky keys (NENYA-29).
func pinSessionTarget(gw *gateway.NenyaGateway, key string, targets []routing.UpstreamTarget, ttl time.Duration) {
	pinTarget, ok := firstActiveTarget(targets)
	if !ok || !sessionStickyKeysEnabled(gw, pinTarget.Provider) {
		return
	}
	gw.AgentState.SessionRouter.Pin(key, pinTarget.Provider, pinTarget.Model, pinTarget.AccountName, ttl)
}

// promoteOnFrontDrift handles the front target matching the pinned
// provider/model: re-pin only when the serving account drifted from the pin —
// a sibling account was selected during build because the pinned account was
// unavailable. An empty front account (resolution failed entirely) must NOT
// wipe the pin's account: the session may return to the pinned account once
// it recovers. PromoteIfChanged also dedupes concurrent promotions of the
// same effective target under the router lock.
func promoteOnFrontDrift(gw *gateway.NenyaGateway, req *chatRequest, key string, pin routing.SessionState, targets []routing.UpstreamTarget, ttl time.Duration) {
	front := targets[0]
	if front.AccountName == "" || front.AccountName == pin.Account || !sessionStickyKeysEnabled(gw, front.Provider) {
		return
	}
	if gw.AgentState.SessionRouter.PromoteIfChanged(key, front.Provider, front.Model, front.AccountName, ttl) {
		gw.Logger.Debug("sticky pin promoted to sibling account",
			"agent", req.ModelName, "provider", front.Provider, "model", front.Model,
			"from_account", pin.Account, "to_account", front.AccountName)
	}
}

// promoteToFirstActive handles the pinned provider/model being lost (cooling,
// filtered out, or removed): the pin is promoted to the first active target
// so the session's credential affinity follows wherever the traffic went.
func promoteToFirstActive(gw *gateway.NenyaGateway, req *chatRequest, key string, pin routing.SessionState, targets []routing.UpstreamTarget, ttl time.Duration) {
	pinTarget, ok := firstActiveTarget(targets)
	if !ok || !sessionStickyKeysEnabled(gw, pinTarget.Provider) {
		return
	}
	if gw.AgentState.SessionRouter.PromoteIfChanged(key, pinTarget.Provider, pinTarget.Model, pinTarget.AccountName, ttl) {
		gw.Logger.Debug("sticky pin promoted to alternative active target",
			"agent", req.ModelName, "from", pin.Provider+"/"+pin.Model, "to", pinTarget.Provider+"/"+pinTarget.Model)
	}
}

// sessionKeyFromRequest derives a stable session identifier from the agent
// name, the resolved system prompt, and the first user message in the raw
// client payload.
func sessionKeyFromRequest(req *chatRequest, agent config.AgentConfig) (string, bool) {
	systemPrompt, err := config.LoadPromptFile(agent.SystemPromptFile, agent.SystemPrompt, "")
	if err != nil || systemPrompt == "" {
		systemPrompt = agent.SystemPrompt
	}
	firstUser, ok := firstUserMessage(req.Payload)
	if !ok {
		return "", false
	}
	return routing.SessionKey(req.ModelName, systemPrompt, firstUser), true
}

// derivedSessionKey returns the memoized stable session key for the
// request. It is computed eagerly in resolveRouting immediately after
// agent resolution (both sticky routing and the opencode-session header
// need the same value; the derivation performs prompt-file I/O and hashes
// the first user message, so it must not run twice per request). Later
// callers only read the memo.
func (req *chatRequest) derivedSessionKey() (string, bool) {
	if req.sessionKeyDone {
		return req.sessionKey, req.sessionKeyOK
	}
	req.sessionKey, req.sessionKeyOK = sessionKeyFromRequest(req, req.Agent)
	req.sessionKeyDone = true
	return req.sessionKey, req.sessionKeyOK
}

const (
	// opencodeSessionHeader is the HTTP header carrying a stable
	// per-conversation session ID. OpenCode Zen/Go requires it on every chat
	// request for efficient routing and prompt caching.
	opencodeSessionHeader = "X-Opencode-Session"
	// opencodeSessionIDLen is the number of hex characters retained from the
	// 64-char session-key digest when synthesizing a session ID. Guarded at
	// use: routing.SessionKey currently always returns 64 chars, but a
	// shorter future digest must degrade to "no header", not panic.
	opencodeSessionIDLen = 16
	// maxOpencodeSessionHeaderLen bounds the accepted client-supplied
	// session ID length.
	maxOpencodeSessionHeaderLen = 256
)

// validSessionHeaderValue reports whether s is a usable outbound HTTP
// header value: printable ASCII plus tab, non-empty, no whitespace-only
// values, and within the length bound. The http.Transport rejects other
// byte values at request-write time (mid-retry-loop, with the raw value in
// the error text); validating here lets Nenya replace an unusable client
// value with a synthesized one. obs-text bytes (0x80-0xFF, e.g. UTF-8
// session IDs) are intentionally rejected: Go's transport would forward
// them, but ASCII-only keeps the synthesized-or-forwarded behavior uniform
// across upstreams.
func validSessionHeaderValue(s string) bool {
	if s == "" || len(s) > maxOpencodeSessionHeaderLen {
		return false
	}
	visible := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		if b != '\t' && (b < 0x20 || b > 0x7E) {
			return false
		}
		if b != ' ' && b != '\t' {
			visible = true
		}
	}
	return visible
}

// ensureOpencodeSessionHeader guarantees the chat request carries an
// x-opencode-session header before upstream dispatch. A client-supplied
// value is preserved verbatim when it is a valid header value; an absent
// or unusable client value is replaced with a stable identifier
// synthesized from the sticky-session key (agent name, system prompt, and
// first user message taken from the raw client payload), so the value
// stays constant across the turns of a conversation while remaining
// distinct per conversation. Requests without a derivable key are
// dispatched with the client header only when it is a valid header value;
// an unusable value is dropped so it cannot fail every round trip inside
// http.Transport. The header is set on the inbound request so every
// downstream dispatch path (retry loop, stream continuation, MCP buffered
// loop) inherits it via buildUpstreamRequest's srcHeaders.
func ensureOpencodeSessionHeader(gw *gateway.NenyaGateway, r *http.Request, req *chatRequest) {
	existing := r.Header.Get(opencodeSessionHeader)
	if validSessionHeaderValue(existing) {
		return
	}
	// req.Agent was resolved once in resolveRouting; the identity (raw key
	// plus NENYA-39 affinity fork check) is memoized on the request, so
	// sticky and non-sticky agents each resolve at most once per request.
	key, ok := req.sessionIdentity(gw)
	if !ok || len(key) < opencodeSessionIDLen {
		if existing != "" {
			r.Header.Del(opencodeSessionHeader)
		}
		return
	}
	sessionID := "nenya-" + key[:opencodeSessionIDLen]
	r.Header.Set(opencodeSessionHeader, sessionID)
	gw.Logger.Debug("synthesized opencode session header", "session_id", sessionID, "model", req.ModelName)
}

// agentStickyTTL returns the agent's configured sticky session idle TTL,
// falling back to the routing default when unset or non-positive.
func agentStickyTTL(agent config.AgentConfig) time.Duration {
	if agent.StickySessionTTLSeconds <= 0 {
		return routing.DefaultSessionTTL
	}
	return time.Duration(agent.StickySessionTTLSeconds) * time.Second
}

// firstUserMessage extracts the content of the first message with role
// "user" from the payload messages. Falls back to the first message's
// content if no user message is present yet.
func firstUserMessage(payload map[string]any) (string, bool) {
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return "", false
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			if text := messageText(msg["content"]); text != "" {
				return text, true
			}
		}
	}
	if msg, ok := messages[0].(map[string]any); ok {
		if text := messageText(msg["content"]); text != "" {
			return text, true
		}
	}
	return "", false
}

// messageText renders message content as a plain string for session keying.
func messageText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, part := range c {
			if obj, ok := part.(map[string]any); ok {
				if text, ok := obj["text"].(string); ok {
					sb.WriteString(text)
				}
			}
		}
		return sb.String()
	default:
		return ""
	}
}

// firstActiveTarget returns the first target not in cooling state, or the
// first target if none are flagged.
func firstActiveTarget(targets []routing.UpstreamTarget) (routing.UpstreamTarget, bool) {
	for _, t := range targets {
		if !t.Cooling {
			return t, true
		}
	}
	if len(targets) > 0 {
		return targets[0], true
	}
	return routing.UpstreamTarget{}, false
}

// indexOfActiveTarget returns the index of the first ACTIVE (non-cooling)
// target matching the given provider and model, or -1 if not found.
// Cooling pins are never promoted back to targets[0] to avoid re-firing a
// rate-limited provider mid-cooldown.
func indexOfActiveTarget(targets []routing.UpstreamTarget, provider, model string) int {
	for i, t := range targets {
		if t.Cooling {
			continue
		}
		if t.Provider == provider && t.Model == model {
			return i
		}
	}
	return -1
}

// moveToFront reorders the target slice placing the element at index i at
// the front, preserving relative order of the rest.
func moveToFront(targets []routing.UpstreamTarget, i int) []routing.UpstreamTarget {
	if i <= 0 || i >= len(targets) {
		return targets
	}
	t := targets[i]
	copy(targets[1:i+1], targets[:i])
	targets[0] = t
	return targets
}

func getAgentCooldown(agent config.AgentConfig) time.Duration {
	secs := agent.CooldownSeconds
	if secs == 0 {
		secs = routing.DefaultAgentCooldownSec
	}
	return time.Duration(secs) * time.Second
}

func handleEmptyAgentTargets(req *chatRequest, gw *gateway.NenyaGateway, agent config.AgentConfig) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	if len(agent.Models) > 0 {
		gw.Logger.Warn("all models excluded by max_context",
			"agent", req.ModelName, "tokens", req.TokenCount)
		return nil, "", 0, 0, &httpError{
			Code:    http.StatusRequestEntityTooLarge,
			Message: "Request too large for all configured models in this agent",
			Kind:    infra.ErrorKindPayloadTooLarge,
		}
	}
	gw.Logger.Error("agent has no models configured", "agent", req.ModelName)
	return nil, "", 0, 0, &httpError{
		Code:    http.StatusInternalServerError,
		Message: "Agent has no models configured",
		Kind:    infra.ErrorKindInternal,
	}
}

func reorderTargetsByLatency(req *chatRequest, gw *gateway.NenyaGateway, targets []routing.UpstreamTarget) []routing.UpstreamTarget {
	switch gw.Config.Governance.RoutingStrategy {
	case "balanced":
		return routing.SortTargetsByBalanced(targets, gw.LatencyTracker, gw.CostTracker, gw.ModelCatalog, routing.SortOptions{
			LatencyWeight:     gw.Config.Governance.RoutingLatencyWeight,
			CostWeight:        gw.Config.Governance.RoutingCostWeight,
			BillingMode:       routing.BillingMode(gw.Config.Governance.CostMode),
			BillingEconomy:    gw.Config.Governance.BillingEconomyScale,
			BillingQuality:    gw.Config.Governance.BillingQualityScale,
			BillingModel:      collectProviderBillingModels(gw.Providers),
			BillingFreeOnly:   collectProviderFreeOnly(gw.Providers),
			BillingFreeModels: collectProviderFreeModels(gw.Providers),
			RequestCaps:       detectRequestCapabilities(req.Payload),
		})
	default:
		return routing.SortTargetsByLatency(targets, gw.LatencyTracker, nil)
	}
}

func collectProviderBillingModels(providers map[string]*config.Provider) map[string]string {
	result := make(map[string]string)
	for name, p := range providers {
		if p.Billing != nil && p.Billing.Model != "" {
			result[name] = string(p.Billing.Model)
		}
	}
	return result
}

func collectProviderFreeOnly(providers map[string]*config.Provider) map[string]bool {
	result := make(map[string]bool)
	for name, p := range providers {
		if p.Billing != nil && p.Billing.FreeOnly {
			result[name] = true
		}
	}
	return result
}

func collectProviderFreeModels(providers map[string]*config.Provider) map[string][]string {
	result := make(map[string][]string)
	for name, p := range providers {
		if p.Billing != nil && len(p.Billing.FreeModels) > 0 {
			result[name] = p.Billing.FreeModels
		}
	}
	return result
}

// filterExhaustedTargets drops targets whose serving account is
// billing-exhausted and, for providers that require credentials (any
// AuthStyle except config.AuthStyleNone), targets whose credential is empty.
// Two distinct empty-credential cases are covered: (1) the selector reported
// not-ok — the selector (GetProviderAPIKeyForModel) already includes the
// legacy static-key fallback, so forward-time resolution fails identically;
// (2) a pool account resolved ok but carries an empty credential — a
// misconfiguration whose drop avoids billing misattribution and broken pins.
// Forwarding either case would produce a guaranteed upstream 401 instead of
// failing over to the next target.
// tracker may be nil (skips exhaustion checks); logger must be non-nil.
func filterExhaustedTargets(targets []routing.UpstreamTarget, tracker *billing.BillingTracker, providers map[string]*config.Provider, logger *slog.Logger) []routing.UpstreamTarget {
	if len(targets) == 0 {
		return targets
	}
	filtered := make([]routing.UpstreamTarget, 0, len(targets))
	for _, t := range targets {
		if tracker != nil && tracker.IsExhausted(t.Provider, t.AccountName) {
			logger.Debug("skipping exhausted billing account",
				"provider", t.Provider, "account", t.AccountName, "model", t.Model)
			continue
		}
		if t.Credential == "" && requiresCredentials(providers, t.Provider) {
			logger.Debug("skipping target without resolved credential",
				"provider", t.Provider, "model", t.Model)
			continue
		}
		filtered = append(filtered, t)
	}
	return filtered
}

// requiresCredentials reports whether the named provider needs an explicit
// credential. Providers with AuthStyle config.AuthStyleNone legitimately run
// without one; unknown providers are given the benefit of the doubt.
func requiresCredentials(providers map[string]*config.Provider, name string) bool {
	p, ok := providers[name]
	if !ok {
		return false
	}
	return p.AuthStyle != config.AuthStyleNone
}

func (p *Proxy) resolveModelRouting(ctx context.Context, req *chatRequest, gw *gateway.NenyaGateway) ([]routing.UpstreamTarget, string, time.Duration, int, *httpError) {
	matches := routing.ResolveProviders(req.ModelName, gw.Providers, gw.ModelCatalog)
	if len(matches) == 0 {
		gw.Logger.Warn("no provider found for model", "model", req.ModelName)
		return nil, "", 0, 0, &httpError{
			Code:    http.StatusBadRequest,
			Message: util.ErrNoProvider,
			Kind:    infra.ErrorKindModelNotFound,
		}
	}

	targets := buildProviderTargets(ctx, matches, gw, gw)
	targets = filterExhaustedTargets(targets, gw.BillingTracker, gw.Providers, gw.Logger)
	if len(targets) == 0 {
		gw.Logger.Error("no valid providers after filtering", "model", req.ModelName)
		return nil, "", 0, 0, &httpError{
			Code:    http.StatusServiceUnavailable,
			Message: "No valid providers available",
			Kind:    infra.ErrorKindQuotaExhausted,
		}
	}

	targetStrings := make([]string, len(targets))
	for i, t := range targets {
		targetStrings[i] = fmt.Sprintf("%s/%s", t.Provider, t.Model)
	}
	gw.Logger.Info("model routing", "model", req.ModelName, "providers", len(targets), "targets", targetStrings)
	return targets, "", 0, 0, nil
}

func buildProviderTargets(ctx context.Context, matches []routing.ProviderMatch, gw *gateway.NenyaGateway, accountSelector routing.AccountSelector) []routing.UpstreamTarget {
	targets := make([]routing.UpstreamTarget, 0, len(matches))
	for _, m := range matches {
		provider, ok := gw.Providers[m.Provider]
		if !ok {
			continue
		}
		url := routing.ProviderURL(m.Provider, "", m.Format, provider.FormatURLs, gw.Providers)
		t := routing.UpstreamTarget{
			URL:        url,
			Model:      m.Model,
			Format:     m.Format,
			Provider:   m.Provider,
			MaxContext: m.MaxContext,
			MaxOutput:  m.MaxOutput,
		}
		if accountSelector != nil {
			if cred, acctID, ok := accountSelector.SelectCredentialForModel(ctx, m.Provider, m.Model); ok {
				t.AccountName = acctID
				t.Credential = cred
			}
		}
		targets = append(targets, t)
	}
	return targets
}

// resolveCache checks the response cache and returns a cache key. If a cached
// response is found and served, it returns ("", httpError) to signal early return.
func (p *Proxy) resolveCache(w http.ResponseWriter, r *http.Request, gw *gateway.NenyaGateway, req *chatRequest) (string, *httpError) {
	if gw.ResponseCache == nil {
		return "", nil
	}

	authToken := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	cacheKey := infra.FingerprintPayloadWithAuth(req.Payload, authToken)
	if r.Header.Get(gw.Config.ResponseCache.ForceRefreshHeader) == "" {
		embedFunc := p.buildEmbedFunc(gw, r, req.Payload)
		model := req.ModelName
		if data, ok, cacheType := gw.ResponseCache.Lookup(cacheKey, model, embedFunc); ok {
			p.replayCachedResponse(gw, w, r, data, cacheType, req.Stream)
			return "", &httpError{Code: http.StatusNoContent, Message: "cache hit"}
		}
	}
	return cacheKey, nil
}

func (p *Proxy) buildEmbedFunc(gw *gateway.NenyaGateway, r *http.Request, payload map[string]any) func() ([]float32, error) {
	if !gw.Config.ResponseCache.EnableSemantic {
		return nil
	}

	return func() ([]float32, error) {
		messagesRaw, ok := payload["messages"]
		if !ok {
			return nil, nil
		}
		messages, ok := messagesRaw.([]any)
		if !ok || len(messages) == 0 {
			return nil, nil
		}

		userText := p.extractUserMessagesForEmbedding(gw, messages)
		if userText == "" {
			return nil, nil
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		return gw.Embedder.Embed(ctx, userText)
	}
}

func (p *Proxy) extractUserMessagesForEmbedding(gw *gateway.NenyaGateway, messages []any) string {
	const maxEmbeddingTextLen = 10000

	var userMsgs strings.Builder
	userMsgs.Grow(min(maxEmbeddingTextLen, len(messages)*100))

	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			gw.Logger.Debug("skipping non-object message in semantic embedding")
			continue
		}
		role, ok := msg["role"].(string)
		if !ok || role != "user" {
			continue
		}
		content, ok := msg["content"].(string)
		if !ok {
			gw.Logger.Debug("skipping non-string content in semantic embedding", "role", role)
			continue
		}

		// Enforce size limit to prevent unbounded string growth
		if userMsgs.Len()+len(content) > maxEmbeddingTextLen {
			break
		}

		userMsgs.WriteString(content)
		userMsgs.WriteString("\n")
	}
	return userMsgs.String()
}

// resolvePipelineContext extracts messages, MCP tool state, limits, and client
// profile from the validated request, preparing the payload for the content
// filtering pipeline.
// Returns messages, MCP tools flag, soft/hard limits, window max context, and client profile.
// Proactive truncation thresholds:
//   - SoftLimit: triggers Ollama summarization (1/8 of MaxContext)
//   - HardLimit: absolute truncation limit (3/4 of MaxContext, leaves room for response)
//
// If MaxContext is unknown (<=0), truncation is disabled (limits=0) and the full payload
// is sent upstream. The upstream provider may return context_length_exceeded, which triggers
// automatic retry with summarization.
func (p *Proxy) resolvePipelineContext(r *http.Request, gw *gateway.NenyaGateway, req *chatRequest) ([]any, bool, int, int, int, pipeline.ClientProfile) {
	messagesRaw, ok := req.Payload["messages"]
	if !ok {
		return nil, false, 0, 0, 0, pipeline.ClientProfile{}
	}
	messages, ok := messagesRaw.([]any)
	if !ok || len(messages) == 0 {
		gw.Logger.Warn("messages field is not a non-empty array, skipping Ollama interception")
		return nil, false, 0, 0, 0, pipeline.ClientProfile{}
	}

	autoSearchCtx, autoSearchCancel := context.WithTimeout(r.Context(), mcpAutoSearchTimeout)
	p.injectAutoSearch(gw, autoSearchCtx, req.Payload, messages, req)
	autoSearchCancel()
	p.injectMCPTools(gw, req.Payload, req)

	softLimit := 0
	hardLimit := 0
	if len(req.Targets) > 0 {
		primaryTarget := req.Targets[0]
		if primaryTarget.MaxContext > 0 {
			softLimit = primaryTarget.MaxContext / 8
			// maxCtx/4*3 cannot overflow for any positive int (unlike
			// maxCtx*3/4); kept identical to the transform-side budget.
			hardLimit = primaryTarget.MaxContext / 4 * 3
		} else {
			gw.Logger.Warn("MaxContext unknown for model, proactive truncation disabled — configure max_context to enable",
				"model", req.ModelName,
				"provider", primaryTarget.Provider)
		}
	}

	windowMaxCtx := routing.ResolveWindowMaxContext(req.ModelName, gw.Config.Agents, gw.ModelCatalog)
	profile := pipeline.ClassifyClient(r.Header)
	if profile.IsIDE {
		gw.Logger.Debug("IDE client detected", "client", profile.ClientName)
	}

	return messages, hasMCPTools(gw, req.Agent), softLimit, hardLimit, windowMaxCtx, profile
}

// handleChatCompletions processes chat completion requests with optional content filtering and tool integration.
func (p *Proxy) handleChatCompletions(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, apiKey *config.ApiKey) {
	gwStart := time.Now()
	keyRef := ""
	if apiKey != nil {
		keyRef = apiKey.Name
	}
	req, herr := p.validateChatRequest(w, r, gw, keyRef)
	if herr != nil {
		if herr.Code == http.StatusNoContent {
			// Cache-hit sentinel: nothing to render.
			return
		}
		renderBodyReadError(w, herr)
		return
	}

	ensureOpencodeSessionHeader(gw, r, req)

	if err := p.applyContentPipeline(gw, r.Context(), contentPipelineOpts{
		Payload:      req.Payload,
		TokenCount:   req.TokenCount,
		WindowMaxCtx: req.WindowMaxCtx,
		Profile:      req.Profile,
		SoftLimit:    req.SoftLimit,
		HardLimit:    req.HardLimit,
		AgentName:    req.ModelName,
		Agent:        agentConfigFor(gw, req.ModelName),
	}); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			// The client is gone (or the deadline burned): nothing to
			// dispatch, nothing to render.
			return
		}
		if writePipelineRejection(gw, w, err) {
			return
		}
		gw.Logger.Warn("content pipeline failed, proceeding with interceptor output", "err", err)
	}
	// NENYA-70: the interceptor chain/trim may have shrunk the payload after
	// the estimate was taken at request build. Pre-dispatch guards (TPM, cost)
	// and token accounting must judge the payload that is actually dispatched,
	// so refresh the count from the post-pipeline body.
	req.TokenCount = gw.CountRequestTokens(req.Payload)

	// Canary tripwire (Phase 060): inject AFTER the pipeline so the
	// marker never participates in redaction/compaction/token accounting
	// (it must not trigger the bouncer). The token threads to the
	// response-path watchers.
	canaryResult := pipeline.InjectCanary(gw.Config.Governance.Canary, req.Payload)
	if canaryResult.Token != "" {
		gw.Logger.Debug("canary injected", "agent", req.AgentName)
	}

	gw.Metrics.RecordGatewayProcessing(r.Method, infra.NormalizeMetricPath(r.URL.Path), time.Since(gwStart))

	if req.HasMCPTools {
		p.forwardToUpstreamWithMCP(gw, w, r, forwardOptions{
			Targets:      req.Targets,
			Payload:      req.Payload,
			Stream:       req.Stream,
			Cooldown:     req.Cooldown,
			TokenCount:   req.TokenCount,
			AgentName:    req.AgentName,
			Agent:        req.Agent,
			MaxRetries:   req.MaxRetries,
			CacheKey:     req.CacheKey,
			KeyRef:       req.KeyRef,
			SourceFormat: req.SourceFormat,
			ApiKey:       apiKey,
			Canary:       canaryResult,
		})
		return
	}

	p.forwardToUpstream(gw, w, r, forwardOptions{
		Targets:      req.Targets,
		Payload:      req.Payload,
		Stream:       req.Stream,
		Cooldown:     req.Cooldown,
		TokenCount:   req.TokenCount,
		AgentName:    req.AgentName,
		Agent:        req.Agent,
		MaxRetries:   req.MaxRetries,
		CacheKey:     req.CacheKey,
		KeyRef:       req.KeyRef,
		SourceFormat: req.SourceFormat,
		ApiKey:       apiKey,
		Canary:       canaryResult,
	})
}

// replayCachedResponse serves a previously cached response, setting Content-Type
// based on whether the original request was streaming (SSE) or non-streaming (JSON).
func (p *Proxy) replayCachedResponse(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, data []byte, cacheType string, stream bool) {
	var cacheStatus string
	switch cacheType {
	case "exact":
		cacheStatus = "HIT"
	case "semantic":
		cacheStatus = "SEMI-HIT"
	default:
		cacheStatus = "HIT"
	}

	gw.Logger.Info("response cache hit", "type", cacheType)
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Nenya-Cache-Status", cacheStatus)
	w.WriteHeader(http.StatusOK)

	var dst io.Writer
	if fw, ok := newImmediateFlushWriter(w); ok {
		dst = fw
	} else {
		dst = w
	}
	buf := getStreamBuffer()
	defer streamingBufPool.Put(buf)
	if _, err := copyStream(r.Context(), dst, bytes.NewReader(data), *buf); err != nil {
		gw.Logger.Error("failed to replay cached SSE stream", "err", err)
	}
}

// contentPipelineOpts groups the parameters for applyContentPipeline.
type contentPipelineOpts struct {
	Payload      map[string]interface{}
	TokenCount   int
	WindowMaxCtx int
	Profile      pipeline.ClientProfile
	SoftLimit    int
	HardLimit    int
	// AgentName is the canonical agent identity (top-level model field
	// as resolved by the proxy); Agent is the resolved config when it
	// names a configured agent (nil otherwise).
	AgentName string
	Agent     *config.AgentConfig
}

// agentConfigFor resolves the agent config for a model name, returning
// nil when the name is not a configured agent.
func agentConfigFor(gw *gateway.NenyaGateway, modelName string) *config.AgentConfig {
	if modelName == "" {
		return nil
	}
	if agent, ok := gw.Config.Agents[modelName]; ok {
		return &agent
	}
	return nil
}

// applyContentPipeline runs the shared preprocessing stages (prefix
// cache optimization, compaction, windowing, interceptor chain) over the
// request payload. Mutations are applied in place.
func (p *Proxy) applyContentPipeline(gw *gateway.NenyaGateway, ctx context.Context, opts contentPipelineOpts) error {
	payload := opts.Payload
	if gw.InterceptorChain == nil {
		return nil
	}

	messages, ok := payload["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return nil
	}

	pipeline.ApplyPrefixCacheOptimizations(payload, messages, gw.Config.PrefixCache)

	if !opts.Profile.IsIDE {
		if pipeline.ApplyCompaction(messages, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
	}

	if !opts.Profile.IsIDE {
		if pipeline.PruneStaleToolCalls(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
		if pipeline.PruneThoughts(payload, gw.Config.Compaction) {
			gw.Metrics.RecordCompaction()
		}
	}

	deps := buildWindowDeps(gw)
	if windowed, err := pipeline.ApplyWindowCompaction(ctx, deps, payload, messages, opts.TokenCount, gw.Config.Window, opts.WindowMaxCtx, gw.CountRequestTokens); err != nil {
		gw.Logger.Warn("window compaction failed, proceeding without it", "err", err)
	} else if windowed {
		gw.Metrics.RecordWindow(gw.Config.Window.Mode)
		gw.Metrics.RecordTokensSaved("window", opts.TokenCount-gw.CountRequestTokens(payload))
	}

	messages = payload["messages"].([]interface{})
	if len(messages) == 0 {
		return nil
	}

	msgObjs := make([]map[string]any, len(messages))
	for i, m := range messages {
		if obj, ok := m.(map[string]any); ok {
			msgObjs[i] = obj
		}
	}

	req := &pipeline.InterceptRequest{
		Payload:    payload,
		Messages:   msgObjs,
		AgentName:  opts.AgentName,
		Agent:      opts.Agent,
		Profile:    opts.Profile,
		SoftLimit:  opts.SoftLimit,
		HardLimit:  opts.HardLimit,
		TokenCount: opts.TokenCount,
	}

	_, err := gw.InterceptorChain.Execute(ctx, req)
	return err
}

// buildWindowDeps creates a WindowDeps from the gateway state.
func buildWindowDeps(gw *gateway.NenyaGateway) pipeline.WindowDeps {
	return pipeline.WindowDeps{
		Logger:    gw.Logger,
		ClientFor: gw.ClientFor,
		Providers: gw.Providers,
		InjectAPIKey: func(providerName string, headers http.Header) error {
			return routing.InjectAPIKeyWithGateway(providerName, gw, headers)
		},
		CountTokens:  gw.CountTokens,
		SummaryCache: gw.WindowSummaries,
	}
}

// applyBufferedEgressPolicies runs the output-side defenses over a
// buffered response body: the ExfilGuard URL policy and the canary
// tripwire. Each may terminate the response with a structured error
// (upstream usage is recorded first); log/strip actions rewrite the
// body in place and let the response continue.
func (p *Proxy) applyBufferedEgressPolicies(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName string, responseMap map[string]interface{}, canary pipeline.CanarResult) bool {
	if guard := exfilGuardFor(gw, agentName); guard != nil {
		hits := inspectResponseTexts(guard, responseMap)
		if hits.blocked {
			// Upstream still consumed tokens on a blocked reply — record
			// them before terminating the client response.
			if usage, ok := responseMap["usage"].(map[string]interface{}); ok {
				recordNonStreamingUsage(r.Context(), gw, target, agentName, usage)
			}
			p.handleExfilBlock(gw, w, target.Model, hits.reason)
			return true
		}
	}
	if canary.Token != "" {
		return p.handleBufferedCanaryHit(gw, w, r, target, agentName, responseMap, canary)
	}
	return false
}

// handleBufferedCanaryHit resolves a canary detection on a buffered
// response: block action records usage, writes the structured 403 and
// reports terminal; log action strips the canary occurrence from every
// surface and lets the response continue.
func (p *Proxy) handleBufferedCanaryHit(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName string, responseMap map[string]interface{}, canary pipeline.CanarResult) bool {
	hits := inspectCanaryTexts(responseMap, canary.Token)
	if !hits.hit {
		return false
	}
	gw.Metrics.RecordExfilEvent("buffered")
	gw.Logger.Warn("canary detected in buffered output",
		"agent", agentName, "model", target.Model, "channel", "buffered")
	if canary.Action != config.CanaryActionBlock {
		return false
	}
	if usage, ok := responseMap["usage"].(map[string]interface{}); ok {
		recordNonStreamingUsage(r.Context(), gw, target, agentName, usage)
	}
	writeStructuredError(w, http.StatusForbidden, infra.ErrorKindExfilDetected,
		"response blocked: canary token detected in output")
	return true
}

func (p *Proxy) buildUpstreamRequest(gw *gateway.NenyaGateway, ctx context.Context, method, url string, body []byte, providerName, modelName, preselectedCredential string, srcHeaders http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}
	if err := routing.InjectAPIKeyWithGatewayCtx(ctx, providerName, modelName, gw, req.Header, preselectedCredential); err != nil {
		return nil, fmt.Errorf("API key injection failed: %w", err)
	}
	// Forward only safe passthrough headers; never let client-supplied
	// headers leak internal routing tokens or override upstream auth.
	for _, h := range []string{
		"X-Request-Id", "X-Correlation-Id", "X-Trace-Id",
		"Traceparent", "Tracestate",
		// OpenCode Zen/Go requires a stable per-conversation session ID for
		// routing and prompt caching (see docs/CLIENT_OPENCODE.md).
		opencodeSessionHeader,
	} {
		if v := srcHeaders.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", gw.Config.Server.UserAgent)
	return req, nil
}

// hasMCPTools reports whether the resolved agent has MCP servers with at
// least one ready client. Direct-model routes pass the zero AgentConfig,
// which never has MCP servers.
func hasMCPTools(gw *gateway.NenyaGateway, agent config.AgentConfig) bool {
	if agent.MCP == nil || len(agent.MCP.Servers) == 0 {
		return false
	}
	for _, serverName := range agent.MCP.Servers {
		if client, ok := gw.MCPClients[serverName]; ok && client.Ready() {
			return true
		}
	}
	return false
}

func (p *Proxy) handleNonStreamingResponse(gw *gateway.NenyaGateway, w http.ResponseWriter, r *http.Request, target routing.UpstreamTarget, agentName, sourceFormat string, action upstreamAction, cacheKey string, cooldownDuration time.Duration, canary pipeline.CanarResult) streamResult {
	defer action.cancel()

	const maxNonStreamingResponseBytes = 10 * 1024 * 1024
	respBody, err := io.ReadAll(io.LimitReader(action.resp.Body, maxNonStreamingResponseBytes))
	if err != nil {
		gw.Logger.Error("failed to read non-streaming response body", "err", err)
		writeGatewayError(w, http.StatusBadGateway, ErrorTypeProvider, "Failed to read upstream response")
		return streamResult{empty: true}
	}
	_ = action.resp.Body.Close()

	if len(respBody) == 0 {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("empty non-streaming response from upstream", "model", target.Model)
		return streamResult{empty: true}
	}
	if len(respBody) >= maxNonStreamingResponseBytes {
		gw.Logger.Error("non-streaming response exceeded size limit", "model", target.Model)
		writeGatewayError(w, http.StatusBadGateway, ErrorTypeProvider, "Upstream response too large")
		return streamResult{empty: true}
	}

	var responseMap map[string]interface{}
	if err := json.Unmarshal(respBody, &responseMap); err != nil {
		// An unparseable 200 body is a failed upstream attempt like any
		// other: record it and fail over instead of terminating the request.
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("failed to parse non-streaming JSON response, trying next target",
			"model", target.Model, "provider", target.Provider, "error_message", err.Error())
		return streamResult{empty: true}
	}

	// A completion that terminated with a network-failure finish_reason is a
	// failed upstream attempt, not a successful response: record the failure
	// and let the retry loop try the next target instead of relaying it.
	if reason := terminalNetworkErrorReason(responseMap); reason != "" {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("non-streaming completion ended with network-error finish_reason, trying next target",
			"model", target.Model, "provider", target.Provider, "finish_reason", reason)
		return streamResult{empty: true}
	}

	// Some providers return HTTP 200 with an error object in the body
	// (Ollama-style error strings, OpenAI/Anthropic-style error objects).
	// Treat those as failed upstream attempts too: record the failure and
	// fail over instead of relaying the error as if it were a completion. A
	// body carrying BOTH a populated error object and choices is treated as
	// a failure envelope — the error wins.
	if details, isErr := embeddedProviderError(responseMap); isErr {
		gw.AgentState.RecordFailure(target, cooldownDuration)
		gw.Logger.Warn("provider error embedded in HTTP-200 body, trying next target",
			"model", target.Model, "provider", target.Provider, "error_message", details.Message)
		return streamResult{empty: true}
	}

	// NENYA-51: cache Gemini thought signatures from the OpenAI-format
	// response before any format conversion drops them. Covers the
	// non-stream path for OpenAI-format clients and the pre-conversion
	// window for Anthropic-source clients (handleNonStreamingResponse
	// bypasses the streaming transformer entirely).
	cacheExtraContentFromResponse(gw.ThoughtSigCache, responseMap)

	if sourceFormat == "anthropic" && target.Format != "anthropic" {
		a := adapter.GetAnthropicAdapter()
		responseMap = a.ConvertOpenAIResponseToAnthropicBody(responseMap)
	} else if target.Format == "anthropic" {
		a := adapter.GetAnthropicAdapter()
		responseMap = a.ConvertAnthropicToOpenAIBody(responseMap, false)
	}

	// ExfilGuard + canary tripwire (Phases 050/060): egress policies on
	// the buffered body before headers are committed. Both may terminate
	// the response with a structured error (terminal: no failover).
	if done := p.applyBufferedEgressPolicies(gw, w, r, target, agentName, responseMap, canary); done {
		return streamResult{terminal: true}
	}

	if usage, ok := responseMap["usage"].(map[string]interface{}); ok {
		recordNonStreamingUsage(r.Context(), gw, target, agentName, usage)
	}

	gw.ExtractQuotaFromResponseHeaders(r.Context(), target.Provider, target.AccountName, action.resp.Header)

	routing.CopyHeaders(action.resp.Header, w.Header())
	w.WriteHeader(action.resp.StatusCode)
	if err := json.NewEncoder(w).Encode(responseMap); err != nil {
		gw.Logger.Debug("non-streaming response write failed", "err", err)
	}
	return streamResult{}
}
