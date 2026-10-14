package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nenya/config"
	"github.com/nenya/internal/testutil"
)

// scopeBaseConfig returns a config whose security-relevant policy is fully
// populated, so every field in the scope is exercised.
func scopeBaseConfig() *config.Config {
	cfg := testutil.MinimalConfig()
	cfg.Governance.BlockedExecutionPatterns = []string{`rm\s+-rf`}
	cfg.Governance.Injection = &config.InjectionConfig{
		Enabled:       config.PtrTo(true),
		ExtraPatterns: []string{`(?i)ignore\s+previous`},
		Escalation: &config.InjectionEscalationConfig{
			Enabled: config.PtrTo(true),
			Engine:  &config.EngineRef{AgentName: "judge"},
		},
	}
	cfg.Governance.Spotlight = &config.SpotlightConfig{
		Enabled: config.PtrTo(true),
		RiskTiers: &config.SpotlightRiskTiersConfig{
			Enabled: config.PtrTo(true),
			Engine:  &config.EngineRef{Provider: "local", Model: "tier"},
		},
	}
	cfg.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled:      config.PtrTo(true),
		Action:       config.ExfilActionLog,
		AllowedHosts: []string{"docs.example.com"},
	}
	cfg.Governance.Canary = &config.CanaryConfig{Enabled: config.PtrTo(false), Action: config.CanaryActionLog}
	cfg.Governance.Judgments = map[string]*config.JudgmentConfig{
		"egress_screen": {Enabled: config.PtrTo(true), Action: "strict"},
	}
	cfg.Governance.TfidfRerank = &config.TfidfRerankConfig{Enabled: config.PtrTo(true)}
	cfg.Bouncer.Enabled = config.PtrTo(true)
	cfg.Bouncer.RedactPatterns = []string{`sk-[A-Za-z0-9]{16,}`}
	cfg.Bouncer.RedactPreset = "credentials"
	cfg.Bouncer.Engine = config.EngineRef{AgentName: "summarizer"}
	cfg.PrefixCache.Enabled = true
	cfg.PrefixCache.SkipRedactionOnSystem = config.PtrTo(true)
	return cfg
}

// scopeBaseAgent is the agent under test: every per-agent policy override set.
func scopeBaseAgent() config.AgentConfig {
	return config.AgentConfig{
		Strategy:          "fallback",
		SystemPrompt:      "be terse",
		SystemPromptFile:  "/etc/nenya/prompt.txt",
		ForceSystemPrompt: true,
		CacheAware:        config.CacheAwareAuto,
		CacheSalt:         config.PtrTo("tenant-a"),
		Injection:         &config.InjectionConfig{Strict: config.PtrTo(true)},
		Spotlight:         &config.SpotlightConfig{HistoryEnabled: config.PtrTo(true)},
		ExfilGuard:        &config.ExfilGuardOverrideConfig{Action: config.ExfilActionBlock},
	}
}

// TestCachePolicyScope_PartitionsEveryPolicyInput pins the NENYA-154 invariant
// that a scope changes whenever the effective policy can change: agent
// identity, client wire format, and every security-relevant knob.
func TestCachePolicyScope_PartitionsEveryPolicyInput(t *testing.T) {
	base := scopeBaseConfig()
	agent := scopeBaseAgent()
	baseScope := resolveCachePolicyScope("chat", agent, "openai", base)

	mutate := func(fn func(c *config.Config)) string {
		c := scopeBaseConfig()
		fn(c)
		return resolveCachePolicyScope("chat", agent, "openai", c)
	}
	mutateAgent := func(fn func(a *config.AgentConfig)) string {
		a := scopeBaseAgent()
		fn(&a)
		return resolveCachePolicyScope("chat", a, "openai", base)
	}

	changed := []struct {
		name  string
		scope string
	}{
		{"agent identity", resolveCachePolicyScope("other-agent", agent, "openai", base)},
		{"client format", resolveCachePolicyScope("chat", agent, "anthropic", base)},
		{"bouncer enabled", mutate(func(c *config.Config) { c.Bouncer.Enabled = config.PtrTo(false) })},
		{"bouncer fail_open", mutate(func(c *config.Config) { c.Bouncer.FailOpen = config.PtrTo(false) })},
		{"redact patterns", mutate(func(c *config.Config) { c.Bouncer.RedactPatterns = nil })},
		{"redact preset", mutate(func(c *config.Config) { c.Bouncer.RedactPreset = "" })},
		{"redaction label", mutate(func(c *config.Config) { c.Bouncer.RedactionLabel = "[X]" })},
		{"output redaction", mutate(func(c *config.Config) { c.Bouncer.RedactOutput = true })},
		{"output redaction window", mutate(func(c *config.Config) { c.Bouncer.RedactOutputWindow = 8192 })},
		{"entropy threshold", mutate(func(c *config.Config) { c.Bouncer.EntropyThreshold = 7.5 })},
		{"entropy min token", mutate(func(c *config.Config) { c.Bouncer.EntropyMinToken = 40 })},
		{"blocked execution patterns", mutate(func(c *config.Config) {
			c.Governance.BlockedExecutionPatterns = append(c.Governance.BlockedExecutionPatterns, `curl`)
		})},
		{"prefix cache enabled", mutate(func(c *config.Config) { c.PrefixCache.Enabled = false })},
		{"prefix cache redaction skip", mutate(func(c *config.Config) {
			c.PrefixCache.SkipRedactionOnSystem = config.PtrTo(false)
		})},
		{"injection section", mutate(func(c *config.Config) {
			c.Governance.Injection = &config.InjectionConfig{Enabled: config.PtrTo(false)}
		})},
		{"spotlight section", mutate(func(c *config.Config) { c.Governance.Spotlight = nil })},
		{"exfil action", mutate(func(c *config.Config) { c.Governance.ExfilGuard.Action = config.ExfilActionStrip })},
		{"exfil allowed hosts", mutate(func(c *config.Config) {
			c.Governance.ExfilGuard.AllowedHosts = append(c.Governance.ExfilGuard.AllowedHosts, "evil.example")
		})},
		{"canary section", mutate(func(c *config.Config) {
			c.Governance.Canary = &config.CanaryConfig{Enabled: config.PtrTo(true), Action: config.CanaryActionBlock}
		})},
		{"judgment action", mutate(func(c *config.Config) {
			c.Governance.Judgments["egress_screen"].Action = "log"
		})},
		{"tfidf rerank", mutate(func(c *config.Config) { c.Governance.TfidfRerank = nil })},
		{"bouncer engine alias", mutate(func(c *config.Config) {
			c.Bouncer.Engine = config.EngineRef{AgentName: "other-summarizer"}
		})},
		{"bouncer engine prompt", mutate(func(c *config.Config) {
			c.Bouncer.Engine.SystemPrompt = "summarize differently"
		})},
		{"bouncer engine timeout", mutate(func(c *config.Config) { c.Bouncer.Engine.TimeoutSeconds = 30 })},
		{"judgment engine alias", mutate(func(c *config.Config) {
			c.Governance.Judgments["egress_screen"].Engine = &config.EngineRef{AgentName: "other-judge"}
		})},
		{"agent injection override", mutateAgent(func(a *config.AgentConfig) { a.Injection = nil })},
		{"agent spotlight override", mutateAgent(func(a *config.AgentConfig) { a.Spotlight = nil })},
		{"agent exfil override", mutateAgent(func(a *config.AgentConfig) {
			a.ExfilGuard = &config.ExfilGuardOverrideConfig{Action: config.ExfilActionLog}
		})},
		{"agent system prompt", mutateAgent(func(a *config.AgentConfig) { a.SystemPrompt = "be verbose" })},
		{"agent system prompt file", mutateAgent(func(a *config.AgentConfig) {
			a.SystemPromptFile = "/etc/nenya/other.txt"
		})},
		{"agent force system prompt", mutateAgent(func(a *config.AgentConfig) { a.ForceSystemPrompt = false })},
		{"agent cache aware", mutateAgent(func(a *config.AgentConfig) { a.CacheAware = config.CacheAwareForce })},
		{"agent cache salt", mutateAgent(func(a *config.AgentConfig) { a.CacheSalt = config.PtrTo("tenant-b") })},
	}

	for _, tc := range changed {
		if tc.scope == "" {
			t.Errorf("%s: scope is empty", tc.name)
			continue
		}
		if tc.scope == baseScope {
			t.Errorf("%s: policy change did not change the scope", tc.name)
		}
	}

	// An engine alias repointed to a different target with the SAME cardinality
	// must still change the scope (resolved targets are not JSON-visible), on
	// any reference: the bouncer engine and a judgment engine.
	engineTargets := func(model string) string {
		c := scopeBaseConfig()
		c.Governance.Judgments["egress_screen"].Engine = &config.EngineRef{
			AgentName:       "judge",
			ResolvedTargets: []config.EngineTarget{{Engine: config.EngineConfig{Provider: "p", Model: model}}},
		}
		return resolveCachePolicyScope("chat", agent, "openai", c)
	}
	if engineTargets("m-a") == engineTargets("m-b") {
		t.Error("engine target identity must change the scope")
	}

	bouncerEngine := func(mutate func(e *config.EngineRef)) string {
		c := scopeBaseConfig()
		mutate(&c.Bouncer.Engine)
		return resolveCachePolicyScope("chat", agent, "openai", c)
	}
	if bouncerEngine(func(e *config.EngineRef) { e.SystemPromptFile = "/etc/nenya/other-prompt.txt" }) ==
		bouncerEngine(func(e *config.EngineRef) { e.SystemPromptFile = "/etc/nenya/prompt.txt" }) {
		t.Error("engine prompt-file identity must change the scope")
	}
	if bouncerEngine(func(e *config.EngineRef) {
		e.ResolvedTargets = []config.EngineTarget{{Engine: config.EngineConfig{Provider: "p", Model: "m-a"}}}
	}) == bouncerEngine(func(e *config.EngineRef) {
		e.ResolvedTargets = []config.EngineTarget{{Engine: config.EngineConfig{Provider: "p", Model: "m-b"}}}
	}) {
		t.Error("bouncer engine target identity must change the scope")
	}
}

// TestCachePolicyScope_StableAndMinimal asserts the other half of the contract:
// identical policy inputs (including agent fields that cannot affect the
// response policy) keep the scope stable, so the cache does not thrash.
func TestCachePolicyScope_StableAndMinimal(t *testing.T) {
	cfg := scopeBaseConfig()
	agent := scopeBaseAgent()
	base := resolveCachePolicyScope("chat", agent, "openai", cfg)

	if again := resolveCachePolicyScope("chat", scopeBaseAgent(), "openai", scopeBaseConfig()); again != base {
		t.Fatalf("identical inputs produced different scopes:\n%q\n%q", base, again)
	}

	// Multiple judgment sites: the engine-identity walk must be order-stable
	// (map iteration order is randomized), or every request would get a unique
	// key in exactly the deployments that use the advisory judgment layer.
	multi := scopeBaseConfig()
	multi.Governance.Judgments = map[string]*config.JudgmentConfig{
		"egress_screen":     {Enabled: config.PtrTo(true), Engine: &config.EngineRef{AgentName: "judge-a"}},
		"summary_fidelity":  {Enabled: config.PtrTo(true), Engine: &config.EngineRef{AgentName: "judge-b"}},
		"spotlight_tier":    {Enabled: config.PtrTo(true), Engine: &config.EngineRef{AgentName: "judge-c"}},
		"injection_screen":  {Enabled: config.PtrTo(true), Engine: &config.EngineRef{AgentName: "judge-d"}},
		"summary_fidelity2": {Enabled: config.PtrTo(true), Engine: &config.EngineRef{AgentName: "judge-e"}},
	}
	first := resolveCachePolicyScope("chat", agent, "openai", multi)
	for i := 0; i < 8; i++ {
		if got := resolveCachePolicyScope("chat", agent, "openai", multi); got != first {
			t.Fatalf("multi-judgment scope is not deterministic (call %d):\n%q\n%q", i, first, got)
		}
	}

	// A non-serializable policy (NaN in a hand-built config) must fail closed:
	// no scope, so the caller skips the cache entirely.
	broken := scopeBaseConfig()
	broken.Bouncer.EntropyThreshold = math.NaN()
	if got := resolveCachePolicyScope("chat", agent, "openai", broken); got != "" {
		t.Fatalf("unserializable policy must yield no scope, got %q", got)
	}

	// Non-policy agent fields (routing strategy, retries, cooldown, model list)
	// must not partition the cache.
	other := scopeBaseAgent()
	other.Strategy = "round-robin"
	other.MaxRetries = 9
	other.CooldownSeconds = 30
	other.Models = []config.AgentModel{{Provider: "p", Model: "m"}}
	if got := resolveCachePolicyScope("chat", other, "openai", cfg); got != base {
		t.Fatalf("non-policy agent fields must not change the scope:\n%q\n%q", base, got)
	}

	// Direct-model routes carry no agent identity; the zero AgentConfig must
	// not collide with a named agent's scope.
	if got := resolveCachePolicyScope("", config.AgentConfig{}, "openai", cfg); got == base {
		t.Fatal("direct-model scope must differ from a named agent's scope")
	}

	// Defensive: a nil config still scopes by agent and format (no panic).
	if got := resolveCachePolicyScope("chat", agent, "openai", nil); got == "" || got == base {
		t.Fatalf("nil config scope must be non-empty and distinct, got %q", got)
	}
}

// TestCachePolicyScope_FieldSetIsPinned pins the exact key set of the scope so
// that adding (or dropping) a cache-relevant dimension is a deliberate act:
// the reviewer of a new knob must decide whether it belongs in the cache key.
func TestCachePolicyScope_FieldSetIsPinned(t *testing.T) {
	want := []string{
		"agent", "format",
		"bouncer_enabled", "bouncer_fail_open", "redaction_label", "redact_preset",
		"redact_patterns", "redact_output", "redact_output_window",
		"entropy_enabled", "entropy_threshold", "entropy_min_token",
		"blocked_patterns", "prefix_cache_enabled", "prefix_cache_skip_redaction_on_system",
		"injection", "spotlight", "exfil_guard", "canary", "judgments", "tfidf_rerank",
		"engine_identity",
		"agent_injection", "agent_spotlight", "agent_exfil_guard",
		"agent_system_prompt", "agent_system_prompt_file", "agent_force_system_prompt",
		"agent_cache_aware", "agent_cache_salt",
	}

	serialized := resolveCachePolicyScope("chat", scopeBaseAgent(), "openai", scopeBaseConfig())
	var got map[string]any
	if err := json.Unmarshal([]byte(serialized), &got); err != nil {
		t.Fatalf("scope is not valid JSON: %v (%s)", err, serialized)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	wantSorted := append([]string(nil), want...)
	sort.Strings(wantSorted)
	if strings.Join(keys, ",") != strings.Join(wantSorted, ",") {
		t.Fatalf("scope key set drifted:\n got: %v\nwant: %v", keys, wantSorted)
	}
}

// TestCacheHitInvalidatedByPolicyChange reproduces the GoModel #971 incident
// shape end to end: a streaming response is cached under a permissive policy,
// the policy is then tightened in process, and the identical follow-up request
// must reach the upstream again instead of replaying the pre-policy body.
func TestCacheHitInvalidatedByPolicyChange(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		writeCleanChatStream(w)
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()

	rec1 := runStreamingRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec1, http.StatusOK)
	if got := gw.ResponseCache.Len(); got != 1 {
		t.Fatalf("first clean stream must be cached, cache len = %d", got)
	}

	// Same policy: replay from cache, no upstream call.
	rec2 := runStreamingRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec2, http.StatusOK)
	if got := calls.Load(); got != 1 {
		t.Fatalf("unchanged policy must still hit the cache, upstream calls = %d", got)
	}

	// Policy change after the entry was stored (the guardrail appears).
	gw.Config.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled: config.PtrTo(true),
		Action:  config.ExfilActionStrip,
	}

	rec3 := runStreamingRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec3, http.StatusOK)
	if got := calls.Load(); got != 2 {
		t.Fatalf("policy change must invalidate the cached entry, upstream calls = %d (want 2)", got)
	}
	if !strings.Contains(rec3.Body.String(), "ok") {
		t.Errorf("fresh stream missing content: %s", rec3.Body.String())
	}
}

// TestExecutionPolicyBlockedStreamIsTerminalAndUncached pins the second
// handleBlockedStream branch (execution policy, stream.ErrStreamBlocked): a
// response the stream filter refuses to emit terminates the exchange with the
// structured block frame instead of a generic interruption, and is never
// cached.
func TestExecutionPolicyBlockedStreamIsTerminalAndUncached(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"run rm -rf /\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()
	// The patterns are compiled into the gateway at construction, but the
	// filter is only attached when output redaction is enabled.
	gw.BlockedPatterns = []*regexp.Regexp{regexp.MustCompile(`rm\s+-rf`)}
	gw.Config.Bouncer.RedactOutput = true

	rec := runRequest(t, p)
	body := rec.Body.String()
	if !strings.Contains(body, "[Response blocked by execution policy]") {
		t.Fatalf("expected the execution-policy block frame, got: %s", body)
	}
	if strings.Contains(body, "upstream stream interrupted") {
		t.Fatalf("a policy block must not surface as a generic interruption: %s", body)
	}
	if got := gw.ResponseCache.Len(); got != 0 {
		t.Fatalf("blocked stream must never be cached, cache len = %d", got)
	}
}

// TestStalledStreamIsTerminalAndUncached pins the stall branch of
// handleStreamDone end to end: an upstream that accepts the request and then
// sends nothing must terminate with the structured stall frame (not a generic
// interruption) and must not populate the response cache.
func TestStalledStreamIsTerminalAndUncached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // never send a byte
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()
	// Pin the intent: the stall branch is reached because no stream-head probe
	// intercepts the empty stream first (both probes are off by default in the
	// fixture; set explicitly so a default change cannot silently reroute this
	// scenario into the retry-loop error path).
	gw.Config.Governance.StreamBootstrapBufferBytes = 0
	gw.Config.Governance.EmptyStreamAsError = config.PtrTo(false)
	gw.Config.Governance.EarlyStreamErrorFailover = config.PtrTo(false)
	gw.Config.Governance.StreamIdleTimeoutSeconds = config.PtrTo(1)

	rec := runRequest(t, p)
	body := rec.Body.String()
	if !strings.Contains(body, "upstream stream stalled") {
		t.Fatalf("expected the stall frame, got: %s", body)
	}
	if strings.Contains(body, "upstream stream interrupted") {
		t.Fatalf("a stall must not surface as a generic interruption: %s", body)
	}
	if got := gw.ResponseCache.Len(); got != 0 {
		t.Fatalf("stalled stream must never be cached, cache len = %d", got)
	}
}

// TestCanaryDetectedStreamIsTerminalAndUncached pins the canary branch end to
// end: an upstream that echoes the injected canary marker back must terminate
// with the exfil_detected frame (not a generic interruption) and must not
// populate the response cache.
func TestCanaryDetectedStreamIsTerminalAndUncached(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// Echo the request body (which carries the injected canary marker) as
		// the model output.
		chunk, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": string(body)}}},
		})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()
	gw.Config.Governance.Canary = &config.CanaryConfig{
		Enabled: config.PtrTo(true),
		Action:  config.CanaryActionBlock,
	}

	rec := runRequest(t, p)
	body := rec.Body.String()
	if !strings.Contains(body, string("exfil_detected")) {
		t.Fatalf("expected an exfil_detected terminal, got: %s", body)
	}
	if strings.Contains(body, "upstream stream interrupted") {
		t.Fatalf("a canary detection must not surface as a generic interruption: %s", body)
	}
	if got := gw.ResponseCache.Len(); got != 0 {
		t.Fatalf("canary-blocked stream must never be cached, cache len = %d", got)
	}
}

// runStreamingRequest posts the cache-test payload with an explicit
// `"stream":true` so cache-path tests also cover the true client-requested
// streaming shape (the shared makeTestRequest fixture omits it, leaving
// req.Stream false end to end; the upstream content type still drives the
// streaming path, which the other tests deliberately exercise).
func runStreamingRequest(t *testing.T, p *Proxy) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"test-agent","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req := testutil.NewTestRequest(t, http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// TestUnserializablePolicyDisablesTheResponseCache pins the fail-closed
// contract end to end: when the policy scope cannot be serialized (NaN in a
// hand-built config), resolveCache skips both the lookup and the store — the
// stream succeeds, no cache-status header is set, and nothing is cached.
func TestUnserializablePolicyDisablesTheResponseCache(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		writeCleanChatStream(w)
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()
	gw.Config.Bouncer.EntropyThreshold = math.NaN()

	rec1 := runStreamingRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec1, http.StatusOK)
	if got := rec1.Header().Get("X-Nenya-Cache-Status"); got != "" {
		t.Fatalf("no cache status expected when the policy scope is unusable, got %q", got)
	}
	if got := gw.ResponseCache.Len(); got != 0 {
		t.Fatalf("nothing may be cached without a usable policy scope, cache len = %d", got)
	}

	rec2 := runStreamingRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec2, http.StatusOK)
	if got := rec2.Header().Get("X-Nenya-Cache-Status"); got != "" {
		t.Fatalf("no cache status expected on the second request either, got %q", got)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests must reach the upstream when caching is disabled, calls = %d", got)
	}
}

// TestExfilBlockedStreamNotCached verifies the issue's "blocked responses must
// never be cached" requirement on the streaming path: a stream terminated by
// the egress guard must leave the cache empty, surface the structured
// exfil_blocked terminal, and not be replayable to the next identical request.
func TestExfilBlockedStreamNotCached(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// Link-local IP literal: denied by the guard regardless of host policy.
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fetch ![m](http://169.254.169.254/latest/meta-data/)\"}}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer up.Close()

	p := newCachedStreamProxy(t, up.URL)
	gw := p.Gateway()
	gw.Config.Governance.ExfilGuard = &config.ExfilGuardConfig{
		Enabled: config.PtrTo(true),
		Action:  config.ExfilActionBlock,
	}

	rec := runRequest(t, p)
	testutil.AssertResponseStatusCode(t, rec, http.StatusOK)
	body := rec.Body.String()
	if !strings.Contains(body, string("exfil_blocked")) {
		t.Fatalf("expected an exfil_blocked terminal, got: %s", body)
	}
	if strings.Contains(body, "169.254.169.254") {
		t.Fatalf("blocked URL must never reach the client: %s", body)
	}
	// Regression guard: the store path is only reachable on a clean stream
	// today, so this assertion fails if a future change starts caching a
	// blocked exchange.
	if got := gw.ResponseCache.Len(); got != 0 {
		t.Fatalf("blocked stream must never be cached, cache len = %d", got)
	}

	// The blocked exchange must not be replayable: the next identical request
	// reaches the upstream again and is blocked again.
	rec2 := runRequest(t, p)
	if !strings.Contains(rec2.Body.String(), string("exfil_blocked")) {
		t.Fatalf("second request must be re-dispatched and blocked, got: %s", rec2.Body.String())
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("blocked stream must not poison the cache, upstream calls = %d (want 2)", got)
	}
}
