package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/nenya/config"
)

// cachePolicyScope is the canonical identity of the content policy that can
// change what a client receives, beyond the cacheable payload fields
// (NENYA-154). It exists because a cached body is only valid for the caller,
// agent, wire format, and guardrail set that produced it: replaying a body
// cached before a redaction/egress guardrail existed is the GoModel incident
// (#971) this hardening mirrors.
//
// Every field is a policy INPUT (a global governance/bouncer section plus the
// resolved agent's overrides), never a resolved verdict, so no resolution logic
// has to mirror the interceptors faithfully to stay safe: equal scopes imply
// equal inputs. (EngineIdentity is the one deliberate exception: it renders the
// resolved engine chain, because the referenced agent's own config is not part
// of this scope and the resolved chain is what the engine call actually uses.)
// A no-op edit (e.g. an agent override restating the global default) merely
// yields a distinct scope — over-partitioning costs a cache miss, never a wrong
// body.
//
// Scope boundary (documented, deliberate): the response cache is keyed from the
// request as received, before the content pipeline runs. The scope covers the
// guardrail inputs — the ones whose verdict decides whether content may be
// served at all (redaction/entropy, injection, spotlight, egress guard, canary,
// execution-policy blocked patterns, prefix-cache redaction skip, output
// redaction) — plus the agent identity fields that change what is dispatched
// (prompt identity, `cache_aware`, `cache_salt`) and the advisory judgment
// engines that can upgrade a guardrail verdict. The remaining stage gates
// (`context.*` trim limits, `window.*`/`compaction.*`, token calibration,
// routing) only change the tokens sent upstream; they are accepted
// pre-pipeline-key inputs, as documented in docs/CACHING.md.
type cachePolicyScope struct {
	// Agent is the resolved agent identity ("" for direct-model routes).
	Agent string `json:"agent"`
	// Format is the detected client wire format the stored bytes are rendered
	// in ("openai" / "anthropic"); it is the same value that selects the SSE
	// transformer, so replaying a body to a client of the other format cannot
	// happen.
	Format string `json:"format"`

	// Bouncer redaction/entropy: Tier-0 request redaction (patterns, label),
	// output redaction, and whether a bouncer failure forwards the payload
	// unsummarized (fail_open).
	BouncerEnabled     *bool   `json:"bouncer_enabled"`
	BouncerFailOpen    *bool   `json:"bouncer_fail_open"`
	RedactionLabel     string  `json:"redaction_label"`
	RedactPreset       string  `json:"redact_preset"`
	RedactPatterns     string  `json:"redact_patterns"`
	RedactOutput       bool    `json:"redact_output"`
	RedactOutputWindow int     `json:"redact_output_window"`
	EntropyEnabled     bool    `json:"entropy_enabled"`
	EntropyThreshold   float64 `json:"entropy_threshold"`
	EntropyMinToken    int     `json:"entropy_min_token"`

	// Blocked execution patterns drive the response-side stream filter: they
	// rewrite or block served output bytes.
	BlockedPatterns string `json:"blocked_patterns"`

	// Prefix-cache redaction relaxation: skipping Tier-0 redaction on system
	// messages changes what the upstream sees and what is served back.
	// `enabled` is keyed only because it gates the skip (ShouldSkipRedaction);
	// the remaining prefix-cache knobs shape the upstream prefix, not the
	// served policy.
	PrefixCacheEnabled    bool  `json:"prefix_cache_enabled"`
	SkipRedactionOnSystem *bool `json:"prefix_cache_skip_redaction_on_system"`

	// Global security sections and the resolved agent's per-agent overrides
	// (nil when absent). Marshal wholesale so every knob in the section is
	// captured — a future field needs no change here to stay partitioned.
	Injection   *config.InjectionConfig           `json:"injection,omitempty"`
	Spotlight   *config.SpotlightConfig           `json:"spotlight,omitempty"`
	ExfilGuard  *config.ExfilGuardConfig          `json:"exfil_guard,omitempty"`
	Canary      *config.CanaryConfig              `json:"canary,omitempty"`
	Judgments   map[string]*config.JudgmentConfig `json:"judgments,omitempty"`
	TfidfRerank *config.TfidfRerankConfig         `json:"tfidf_rerank,omitempty"`

	// EngineIdentity renders the engine references carried by the sections
	// above. Their alias and resolved-target fields are excluded from JSON
	// (json:"-"), so switching an engine by alias would otherwise be invisible
	// to the scope.
	EngineIdentity string `json:"engine_identity"`

	AgentInjection  *config.InjectionConfig          `json:"agent_injection,omitempty"`
	AgentSpotlight  *config.SpotlightConfig          `json:"agent_spotlight,omitempty"`
	AgentExfilGuard *config.ExfilGuardOverrideConfig `json:"agent_exfil_guard,omitempty"`

	// Agent prompt identity: the system prompt shapes every answer the agent
	// produces, and force_system_prompt/cache_aware change what reaches the
	// upstream. cache_aware is keyed because it selects which pipeline stages
	// run (it skips window compaction and the tail TF-IDF prune), not merely
	// how tokens are shaped. The prompt FILE PATH is keyed; editing the file in
	// place is the documented pre-pipeline-key limitation.
	AgentSystemPrompt      string `json:"agent_system_prompt"`
	AgentSystemPromptFile  string `json:"agent_system_prompt_file"`
	AgentForceSystemPrompt bool   `json:"agent_force_system_prompt"`
	AgentCacheAware        string `json:"agent_cache_aware"`
	AgentCacheSalt         string `json:"agent_cache_salt"`
}

// resolveCachePolicyScope returns the canonical content-policy identity string
// mixed into the response-cache fingerprint (NENYA-154). It is deterministic
// for identical inputs, so two requests that share everything the policy
// depends on (agent, wire format, guardrail configuration) produce the same
// scope and keep their cache entries shareable. A nil cfg (defensive, tests)
// degrades to agent + format scoping only.
//
// It returns "" when the scope cannot be serialized (a non-JSON value such as
// NaN in a hand-built config): callers must then skip the response cache
// entirely — a policy-free key would defeat the partition.
//
// Cost: the scope marshals the small security sections and digests a few
// pattern lists. That is bounded and paid only when the response cache is
// enabled, alongside the existing fingerprint, which already marshals the
// entire canonical payload (a much larger allocation) on the same code path —
// so no per-agent memoization is warranted.
func resolveCachePolicyScope(agentName string, agent config.AgentConfig, sourceFormat string, cfg *config.Config) string {
	scope := cachePolicyScope{Agent: agentName, Format: sourceFormat}
	if cfg == nil {
		return marshalCacheScope(scope)
	}

	scope.BouncerEnabled = cfg.Bouncer.Enabled
	scope.BouncerFailOpen = cfg.Bouncer.FailOpen
	scope.RedactionLabel = cfg.Bouncer.RedactionLabel
	scope.RedactPreset = cfg.Bouncer.RedactPreset
	scope.RedactPatterns = digestStringList(cfg.Bouncer.RedactPatterns)
	scope.RedactOutput = cfg.Bouncer.RedactOutput
	scope.RedactOutputWindow = cfg.Bouncer.RedactOutputWindow
	scope.EntropyEnabled = cfg.Bouncer.EntropyEnabled
	scope.EntropyThreshold = cfg.Bouncer.EntropyThreshold
	scope.EntropyMinToken = cfg.Bouncer.EntropyMinToken

	scope.BlockedPatterns = digestStringList(cfg.Governance.BlockedExecutionPatterns)
	scope.PrefixCacheEnabled = cfg.PrefixCache.Enabled
	scope.SkipRedactionOnSystem = cfg.PrefixCache.SkipRedactionOnSystem

	scope.Injection = cfg.Governance.Injection
	scope.Spotlight = cfg.Governance.Spotlight
	scope.ExfilGuard = cfg.Governance.ExfilGuard
	scope.Canary = cfg.Governance.Canary
	scope.Judgments = cfg.Governance.Judgments
	scope.TfidfRerank = cfg.Governance.TfidfRerank
	scope.EngineIdentity = globalEngineIdentity(cfg)

	// Direct-model routes have no agent, so there are no overrides to fold in;
	// the zero AgentConfig must not be treated as "an agent with empty
	// overrides" or its scope would collide with the no-agent scope.
	if agentName != "" {
		scope.AgentInjection = agent.Injection
		scope.AgentSpotlight = agent.Spotlight
		scope.AgentExfilGuard = agent.ExfilGuard
		scope.AgentSystemPrompt = digestString(agent.SystemPrompt)
		scope.AgentSystemPromptFile = agent.SystemPromptFile
		scope.AgentForceSystemPrompt = agent.ForceSystemPrompt
		scope.AgentCacheAware = agent.CacheAware
		if agent.CacheSalt != nil {
			scope.AgentCacheSalt = *agent.CacheSalt
		}
	}

	return marshalCacheScope(scope)
}

// globalEngineIdentity renders every engine reference that can decide a
// guardrail verdict: the bouncer chain (summarization/redaction), the
// injection escalator, the spotlight risk-tier judge, the TF-IDF rerank, and
// each advisory judgment site's chain. Per-agent override engines are folded in
// via the per-agent sections the scope already carries. Judgment names are
// sorted because map iteration order is randomized — an unsorted join would
// make the scope non-deterministic (a unique key per request) in exactly the
// deployments that configure more than one judgment site.
func globalEngineIdentity(cfg *config.Config) string {
	var b strings.Builder
	b.WriteString(engineRefKey(&cfg.Bouncer.Engine))
	if esc := cfg.Governance.Injection.GetEscalation(); esc != nil {
		b.WriteString(engineRefKey(esc.Engine))
	}
	if rt := cfg.Governance.SpotlightRiskTiers(); rt != nil {
		b.WriteString(engineRefKey(rt.Engine))
	}
	if cfg.Governance.TfidfRerank != nil {
		b.WriteString(engineRefKey(cfg.Governance.TfidfRerank.Engine))
	}
	names := make([]string, 0, len(cfg.Governance.Judgments))
	for name := range cfg.Governance.Judgments {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		jc := cfg.Governance.Judgments[name]
		if jc == nil {
			continue
		}
		// Length-prefix the name like every other field (names are validated
		// at load, but the identity must not depend on that).
		writeField(&b, name)
		b.WriteByte('=')
		b.WriteString(engineRefKey(jc.Engine))
	}
	return b.String()
}

// engineRefKey renders one engine reference as a stable, unambiguous identity
// string. The reference's alias, prompt-file path, timeout and resolved targets
// are not JSON-serializable, so they are rendered explicitly (the prompt text
// is digested — it can be long); every field is length-prefixed so no value can
// forge a field boundary, and every resolved target is rendered by
// provider/model so repointing an alias to a different target with the same
// cardinality still changes the scope.
func engineRefKey(e *config.EngineRef) string {
	if e == nil {
		return ""
	}
	var b strings.Builder
	writeField(&b, e.AgentName)
	writeField(&b, e.Provider)
	writeField(&b, e.Model)
	writeField(&b, digestString(e.SystemPrompt))
	writeField(&b, e.SystemPromptFile)
	writeField(&b, strconv.Itoa(e.TimeoutSeconds))
	for _, t := range e.ResolvedTargets {
		writeField(&b, t.Engine.Provider)
		writeField(&b, t.Engine.Model)
	}
	b.WriteByte(';')
	return b.String()
}

// writeField appends one length-prefixed field to an engine-identity record, so
// no value can forge a field boundary regardless of the characters it contains.
func writeField(b *strings.Builder, v string) {
	b.WriteString(strconv.Itoa(len(v)))
	b.WriteByte(':')
	b.WriteString(v)
}

// marshalCacheScope serializes a scope to the string hashed into the cache
// fingerprint. json.Marshal rejects NaN/Inf float values, which a
// hand-constructed config could still supply; the error path fails closed by
// returning the empty string, which callers read as "do not use the cache at
// all" (resolveCache then skips both the lookup and the store) rather than
// collapsing every policy into one shared namespace.
func marshalCacheScope(scope cachePolicyScope) string {
	data, err := json.Marshal(scope)
	if err != nil {
		return ""
	}
	return string(data)
}

// digestString returns a stable digest of one value (used for long or
// free-form strings so the scope stays compact).
func digestString(value string) string {
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// digestStringList returns a stable digest of a pattern list so the scope
// stays compact while still changing on any edit. Elements are length-prefixed
// so no element boundary can be forged by embedded NULs. Empty and nil lists
// share the empty digest.
func digestStringList(values []string) string {
	if len(values) == 0 {
		return ""
	}
	h := sha256.New()
	var scratch [20]byte
	for _, v := range values {
		// AppendInt avoids reflection-based formatting on the request path.
		_, _ = h.Write(strconv.AppendInt(scratch[:0], int64(len(v)), 10))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(v))
	}
	return hex.EncodeToString(h.Sum(nil))
}
