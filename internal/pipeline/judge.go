package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// JudgmentSourcePrefix labels the spotlight envelope wrapped around
// content under adjudication: judged content is always untrusted data
// and must not impersonate the adjudication request itself.
const JudgmentSourcePrefix = "judgment:"

// JudgmentContract defines one typed advisory-judgment decision
// surface: a versioned system prompt, a closed verdict enum, and the
// excerpt/time budgets. Contracts are code-owned and immutable;
// configuration only selects the engine and tunes budgets (the
// advisory layer is LLM-agnostic by design).
type JudgmentContract struct {
	// Name identifies the judgment in logs and metrics.
	Name string
	// System is the versioned contract prompt sent as the system message.
	System string
	// Verdicts is the closed enum of accepted verdict strings.
	Verdicts []string
	// MaxBytes caps the adjudicated excerpt (byte-based, rune-safe).
	// <=0 applies config.DefaultEscalationMaxBytes.
	MaxBytes int
	// TimeoutSeconds bounds the total adjudication across the engine
	// chain; <=0 uses each target's own timeout.
	TimeoutSeconds int
	// Source overrides the spotlight envelope provenance label for the
	// adjudicated content. Empty applies the "judgment:<name>" default.
	// Set it to preserve an established envelope byte-format when an
	// existing caller migrates onto the primitive.
	Source string
	// Caller overrides the engine-chain caller label used in engine
	// call logs (and any caller-keyed dashboards). Empty applies the
	// "judgment_<name>" default.
	Caller string
}

// WithBudget returns a copy of the contract with budgets applied from
// a config judgment entry: maxBytes > 0 replaces the excerpt cap and
// timeoutSeconds > 0 bounds the total adjudication. Zero values leave
// the contract budgets untouched — configuration can tune budgets but
// never the prompt or the verdict enum.
func (c JudgmentContract) WithBudget(maxBytes, timeoutSeconds int) JudgmentContract {
	if maxBytes > 0 {
		c.MaxBytes = maxBytes
	}
	if timeoutSeconds > 0 {
		c.TimeoutSeconds = timeoutSeconds
	}
	return c
}

// Judgment is the outcome of one adjudication. When OK is false the
// judgment failed operationally (engine chain, parse, or contract
// violation): Verdict is empty and the caller MUST apply its
// deterministic tier-1 decision — the advisory layer can only
// strengthen, never weaken.
type Judgment struct {
	// Contract is the name of the judgment contract that produced it.
	Contract string
	// Verdict is a member of the contract enum; empty when OK is false.
	Verdict string
	// Truncated reports whether the excerpt was cut: a favorable
	// verdict on a truncated excerpt is inconclusive for the tail that
	// was never examined.
	Truncated bool
	// Duration is the total engine-chain call time.
	Duration time.Duration
	// Engine is the provider that produced the verdict (the last
	// attempted provider on failure).
	Engine string
	// OK reports whether a contract-valid verdict was produced.
	OK bool
	// Err is the underlying operational error when OK is false
	// (engine-chain failure or contract-parse failure); nil otherwise.
	Err error
	// OutputBytes is the byte length of the engine output received
	// (0 when the chain failed before producing output). Parse-failure
	// forensics.
	OutputBytes int
}

// JudgeDeps groups the engine-chain dependencies shared by all judges.
type JudgeDeps struct {
	// ClientFor resolves the HTTP client for a provider name.
	ClientFor ClientResolver
	// InjectAPIKey injects provider credentials into engine requests.
	InjectAPIKey func(providerName string, headers http.Header) error
	// Logger receives adjudication attempts and fallback warnings.
	Logger *slog.Logger
	// Metrics records judgment outcomes; nil disables metrics.
	Metrics *infra.Metrics
}

// Judge adjudicates content against a JudgmentContract through the
// engine chain. Instances are immutable after construction and safe
// for concurrent use. Targets are captured at construction: on SIGHUP
// reload the gateway rebuilds its pipeline from the fresh config, so
// Judges must always be re-created rather than cached across reloads.
//
// The Judge primitive shares its fail-closed parsing, rune-safe
// truncation, and dependency struct with the escalation path
// (InjectionEscalator, its first consumer, via contract "injection");
// further Judge-based consumers are the judgment-layer sites (summary
// fidelity, egress screen, TF-IDF rerank, spotlight tiering) wired by
// the follow-up phases of the judgment-layer module.
type Judge struct {
	contract JudgmentContract
	targets  []config.EngineTarget
	deps     JudgeDeps
}

// NewJudge resolves the judgment engine and returns a ready judge, or
// an error when the contract, engine reference, or deps are incomplete
// (fail-closed at construction; callers gate on their site being
// enabled).
func NewJudge(contract JudgmentContract, engine *config.EngineRef, deps JudgeDeps) (*Judge, error) {
	if contract.Name == "" {
		return nil, errors.New("judgment: contract has no name")
	}
	if contract.System == "" {
		return nil, fmt.Errorf("judgment %q: contract has no system prompt", contract.Name)
	}
	if len(contract.Verdicts) == 0 {
		return nil, fmt.Errorf("judgment %q: contract has no verdict enum", contract.Name)
	}
	seen := make(map[string]bool, len(contract.Verdicts))
	for _, v := range contract.Verdicts {
		if v == "" {
			return nil, fmt.Errorf("judgment %q: empty verdict in enum", contract.Name)
		}
		if seen[v] {
			return nil, fmt.Errorf("judgment %q: duplicate verdict %q", contract.Name, v)
		}
		seen[v] = true
	}
	if engine == nil {
		return nil, fmt.Errorf("judgment %q: no engine reference configured", contract.Name)
	}
	if len(engine.ResolvedTargets) == 0 {
		return nil, fmt.Errorf("judgment %q: engine resolved to no targets", contract.Name)
	}
	for _, target := range engine.ResolvedTargets {
		if target.Engine.Model == "" {
			return nil, fmt.Errorf("judgment %q: engine target for provider %q has no model",
				contract.Name, target.Provider.Name)
		}
	}
	if deps.ClientFor == nil {
		return nil, fmt.Errorf("judgment %q: no client resolver configured", contract.Name)
	}
	if deps.InjectAPIKey == nil {
		return nil, fmt.Errorf("judgment %q: no API key injector configured", contract.Name)
	}
	if deps.Logger == nil {
		return nil, fmt.Errorf("judgment %q: no logger configured", contract.Name)
	}
	if contract.MaxBytes <= 0 {
		contract.MaxBytes = config.DefaultEscalationMaxBytes
	}
	return &Judge{contract: contract, targets: engine.ResolvedTargets, deps: deps}, nil
}

// Name returns the contract name.
func (j *Judge) Name() string { return j.contract.Name }

// MaxBytes returns the contract's excerpt budget, for callers that
// compose multi-part judgment inputs and must apportion the budget
// themselves (the Judge caps the total again in Adjudicate).
func (j *Judge) MaxBytes() int { return j.contract.MaxBytes }

// adjudicateChain runs the engine chain over one enveloped excerpt:
// shared transport core for Adjudicate and AdjudicateMap. Returns the
// raw output, the answering engine, the call duration, whether the
// excerpt was capped, and the chain error.
func (j *Judge) adjudicateChain(ctx context.Context, agentName, content string) (output, engine string, duration time.Duration, truncated bool, err error) {
	if j.contract.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(j.contract.TimeoutSeconds)*time.Second)
		defer cancel()
	}

	excerpt, truncated := truncateJudgmentExcerpt(content, j.contract.MaxBytes)
	source := j.contract.Source
	if source == "" {
		source = JudgmentSourcePrefix + j.contract.Name
	}
	caller := j.contract.Caller
	if caller == "" {
		caller = "judgment_" + j.contract.Name
	}
	// The chain loop is sequential, so the closure variable is
	// race-free: the last observed provider is the answering one on
	// success.
	var lastEngine string
	call := EngineChainCall{
		Caller:    caller,
		AgentName: agentName,
		System:    j.contract.System,
		Prompt:    SpotlightDelimiters(excerpt, source),
		Observer: func(_, _ int, provider string, _ error, _ time.Duration) {
			lastEngine = provider
		},
	}

	start := time.Now()
	output, err = CallEngineChainObserved(ctx, j.deps.ClientFor, j.targets, j.deps.Logger,
		j.deps.InjectAPIKey, call)
	return output, lastEngine, time.Since(start), truncated, err
}

// Adjudicate sends one excerpt to the engine chain and parses the
// verdict under the contract. Any engine or parse failure returns
// OK=false: the caller applies its deterministic decision. The content
// is enveloped in spotlight delimiters so it cannot impersonate the
// adjudication request itself.
func (j *Judge) Adjudicate(ctx context.Context, agentName, content string) Judgment {
	output, engine, duration, truncated, err := j.adjudicateChain(ctx, agentName, content)

	res := Judgment{Contract: j.contract.Name, Truncated: truncated, Duration: duration, Engine: engine}
	if err == nil {
		res.OutputBytes = len(output)
		var verdict string
		verdict, err = ParseVerdict(output, j.contract.Verdicts)
		if err == nil {
			res.Verdict = verdict
			res.OK = true
		}
	}
	res.Err = err

	if !res.OK {
		j.deps.Logger.Warn("judgment: adjudication failed; caller must apply its deterministic decision",
			"judgment", j.contract.Name, "agent", agentName,
			"err", err, "duration_ms", res.Duration.Milliseconds(), "truncated", truncated)
	}
	// RecordJudgment normalizes an empty verdict to "error".
	j.deps.Metrics.RecordJudgment(j.contract.Name, res.Verdict, res.Engine, res.Duration)
	return res
}

// AdjudicateMap runs the engine chain over one excerpt and parses a
// per-key verdict map with a contract-specific fail-closed parser (for
// contracts whose verdict is a JSON map, e.g. per-block relevance; the
// parser owns key semantics and returns its own key space). Same
// envelope, timeout, and failure semantics as Adjudicate: any engine or
// parse failure returns (nil, judgment-with-OK=false) and the caller
// applies its deterministic decision. Judgment metrics are recorded
// here with verdict positive when at least one key is true, "clear"
// otherwise, and "error" on failure.
func (j *Judge) AdjudicateMap(ctx context.Context, agentName, content string, parse func(output string) (map[int]bool, error), positive string) (map[int]bool, Judgment) {
	output, engine, duration, truncated, chainErr := j.adjudicateChain(ctx, agentName, content)

	res := Judgment{Contract: j.contract.Name, Truncated: truncated, Duration: duration, Engine: engine}
	var verdicts map[int]bool
	var err error
	if chainErr == nil {
		res.OutputBytes = len(output)
		verdicts, err = parse(output)
	} else {
		err = chainErr
	}
	res.Err = err

	if err != nil {
		j.deps.Logger.Warn("judgment: adjudication failed; caller must apply its deterministic decision",
			"judgment", j.contract.Name, "agent", agentName,
			"err", err, "duration_ms", duration.Milliseconds(), "truncated", truncated)
		j.deps.Metrics.RecordJudgment(j.contract.Name, "", engine, duration)
		return nil, res
	}
	verdict := "clear"
	for _, truthy := range verdicts {
		if truthy {
			verdict = positive
			break
		}
	}
	res.OK = true
	res.Verdict = verdict
	j.deps.Metrics.RecordJudgment(j.contract.Name, verdict, engine, duration)
	return verdicts, res
}

// ParseVerdict extracts a contract verdict from engine output. Leading
// and trailing prose and code fences are tolerated: the first-to-last
// brace span is parsed as a JSON object carrying a single "verdict"
// string field. A missing, malformed, or out-of-contract verdict is an
// error (fail-closed: callers fall back to their deterministic
// decision).
func ParseVerdict(output string, allowed []string) (string, error) {
	start := strings.Index(output, "{")
	if start < 0 {
		return "", errors.New("judgment output carries no JSON object")
	}
	end := strings.LastIndex(output, "}")
	if end <= start {
		return "", errors.New("judgment output has no closing brace (unterminated or empty JSON object)")
	}
	var raw struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(output[start:end+1]), &raw); err != nil {
		return "", fmt.Errorf("judgment output is not valid JSON: %w", err)
	}
	for _, v := range allowed {
		if raw.Verdict == v {
			return raw.Verdict, nil
		}
	}
	return "", fmt.Errorf("judgment verdict %q outside contract (want one of %s)",
		raw.Verdict, strings.Join(allowed, "|"))
}

// truncateJudgmentExcerpt caps content at maxBytes without splitting a
// multi-byte rune, reporting whether truncation occurred.
func truncateJudgmentExcerpt(content string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content, false
	}
	keep := maxBytes
	for keep > 0 && content[keep]&0xC0 == 0x80 {
		keep--
	}
	return content[:keep] + "\n... [excerpt truncated]", true
}
