package pipeline

import (
	"context"
	"log/slog"
	"math"
	"unicode/utf8"

	"github.com/nenya/config"
	"github.com/nenya/internal/infra"
)

// TFIDFInterceptor applies TF-IDF relevance scoring to prune low-relevance
// content blocks, reducing token count while preserving query-relevant information.
// Priority: 30 — runs after redaction, before summarization. With the
// advisory rerank enabled, borderline dropped blocks get one rescue
// judgment per pass (strengthen-only: rescue, never additional drops).
type TFIDFInterceptor struct {
	name           string
	priority       int
	querySource    string
	contextCfg     config.ContextConfig
	logger         *slog.Logger
	rerank         *config.TfidfRerankConfig
	rerankJudge    *Judge
	metrics        *infra.Metrics
	selectionCache *TfidfSelectionCache
}

// TFIDFInterceptorOpts groups the TFIDFInterceptor constructor inputs.
type TFIDFInterceptorOpts struct {
	// QuerySource selects the relevance query: "prior_messages", "self", or "".
	QuerySource string
	// ContextCfg carries the operator's truncation keep percentages so the
	// TF-IDF fallback truncation honors them.
	ContextCfg config.ContextConfig
	Logger     *slog.Logger
	// Rerank configures the advisory rescue (nil disables it).
	Rerank *config.TfidfRerankConfig
	// RerankJudge adjudicates borderline blocks (nil disables the rescue).
	RerankJudge *Judge
	// Metrics is nil-safe.
	Metrics *infra.Metrics
	// SelectionCache freezes each message's keep/drop decision on first
	// scoring so prior_messages-mode output is stable across turns
	// (nil disables memoization).
	SelectionCache *TfidfSelectionCache
}

// NewTFIDFInterceptor creates a new TFIDFInterceptor from opts.
func NewTFIDFInterceptor(opts TFIDFInterceptorOpts) *TFIDFInterceptor {
	return &TFIDFInterceptor{
		name:           "tfidf",
		priority:       30,
		querySource:    opts.QuerySource,
		contextCfg:     opts.ContextCfg,
		logger:         opts.Logger,
		rerank:         opts.Rerank,
		rerankJudge:    opts.RerankJudge,
		metrics:        opts.Metrics,
		selectionCache: opts.SelectionCache,
	}
}

func (t *TFIDFInterceptor) Name() string  { return t.name }
func (t *TFIDFInterceptor) Priority() int { return t.priority }
func (t *TFIDFInterceptor) CanHandle(ctx context.Context, req *InterceptRequest) bool {
	if ctx.Err() != nil {
		return false
	}
	if req.SkipTFIDFPrune {
		// cache_aware: let the provider prefix cache do the work instead of
		// pruning the tail.
		return false
	}
	return t.querySource != "" && len(req.Messages) > 1 && req.SoftLimit > 0 && req.TokenCount > req.SoftLimit
}

// tfidfRuneBudget converts a token limit into the ~3 runes/token rune
// budget with overflow guards (AGENTS.md §7); non-positive limits yield 0.
func tfidfRuneBudget(hardLimit, softLimit int) int {
	if hardLimit > 0 {
		if hardLimit > math.MaxInt/3 {
			return math.MaxInt
		}
		return hardLimit * 3
	}
	if softLimit > math.MaxInt/3 {
		return math.MaxInt
	}
	if softLimit <= 0 {
		return 0
	}
	return softLimit * 3
}

func (t *TFIDFInterceptor) Process(ctx context.Context, req *InterceptRequest) (*InterceptResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if len(req.Messages) == 0 {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	lastMsg := req.Messages[len(req.Messages)-1]
	text, ok := lastMsg["content"].(string)
	if !ok || text == "" {
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	query := t.resolveQuery(req, text)

	// HardLimit is the token limit. TF-IDF operates on runes.
	// Tokens are ~3 runes on average, so multiply by 3 for the rune budget.
	hardLimitRunes := tfidfRuneBudget(req.HardLimit, req.SoftLimit)
	if hardLimitRunes <= 0 {
		// No usable budget (direct Process call bypassing CanHandle):
		// truncating to an empty budget would wipe the message.
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	truncated := t.pruneWithRescue(ctx, req, text, query, hardLimitRunes)
	if truncated == text {
		// Nothing pruned (last message already within the rune budget):
		// report honestly instead of false pruning telemetry.
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	lastMsg["content"] = truncated
	// In-place mutation; payload["messages"] keeps its original
	// []interface{} type for downstream consumers.

	// Estimate the post-prune token count with the same ~3 runes-per-token
	// heuristic used for the rune budget (NENYA-70 observability; consumed
	// via the chain's debug logging).
	origRunes := utf8.RuneCountInString(text)
	truncRunes := utf8.RuneCountInString(truncated)
	newCount := req.TokenCount - (origRunes-truncRunes)/3
	if newCount < 0 {
		newCount = 0
	}

	return &InterceptResult{
		Payload:    req.Payload,
		Truncated:  true,
		TokenCount: newCount,
		Reason:     "tfidf_pruned",
	}, nil
}

// buildRescueParams wires the per-pass rescue hook for one request.
func (t *TFIDFInterceptor) buildRescueParams(ctx context.Context, req *InterceptRequest) *TfidfRescueParams {
	agentName := AgentNameFor(req)
	params := &TfidfRescueParams{
		Band:      t.rerank.Band,
		MaxBlocks: t.rerank.MaxBlocks,
		MaxBytes:  t.rerankMaxBytes(),
	}
	params.Rescorer = func(q string, borderline []Block) map[int]bool {
		return t.rerankRescue(ctx, agentName, q, borderline, params)
	}
	return params
}

// rerankEnabled reports whether the advisory rescue hook is armed.
func (t *TFIDFInterceptor) rerankEnabled() bool {
	return t.rerankJudge != nil && t.rerank != nil && t.rerank.Enabled != nil && *t.rerank.Enabled
}

// rerankMaxBytes resolves the effective excerpt budget: the configured
// value when set, else the judge's contract default. Caller invariant:
// only invoked when rerankEnabled() is true (judge non-nil).
func (t *TFIDFInterceptor) rerankMaxBytes() int {
	if t.rerank.MaxBytes > 0 {
		return t.rerank.MaxBytes
	}
	return t.rerankJudge.MaxBytes()
}

// rerankRescue adjudicates the borderline set: one judgment call per
// pruning pass, query terms + numbered excerpts in, per-block verdict
// map out. Fail-open: any judge failure returns an empty rescue set and
// marks params.Failed so the caller does not memoize the fallback. The
// count of judge-relevant verdicts is recorded on the rescue params;
// selectKeptBlocks reports how many actually re-entered the budget
// (recorded by the caller after the pass).
func (t *TFIDFInterceptor) rerankRescue(ctx context.Context, agentName, query string, borderline []Block, params *TfidfRescueParams) map[int]bool {
	input := composeTfidfRerankInput(query, borderline, t.rerankMaxBytes())
	verdicts, jres := t.rerankJudge.AdjudicateMap(ctx, agentName, input, parseTfidfRerankVerdict(len(borderline)), "rescue")
	if jres.Truncated {
		t.logger.Warn("tfidf rerank input truncated; tail blocks were not adjudicated",
			"borderline", len(borderline))
	}
	if !jres.OK || jres.Truncated {
		// Operational failure or partial coverage: do not let the caller
		// freeze this fallback selection.
		params.Failed = true
	}
	out := make(map[int]bool, len(verdicts))
	for idx, relevant := range verdicts {
		if relevant {
			out[idx] = true
		}
	}
	return out
}

// resolveQuery extracts the relevance query per the configured source.
func (t *TFIDFInterceptor) resolveQuery(req *InterceptRequest, text string) string {
	switch t.querySource {
	case "prior_messages":
		if len(req.Messages) > 1 {
			prior := make([]any, len(req.Messages)-1)
			for i, m := range req.Messages[:len(req.Messages)-1] {
				prior[i] = m
			}
			return ExtractPriorUserMessages(prior, 5)
		}
	case "self":
		return ExtractSelfQuery(text, 500)
	}
	return ""
}

// pruneWithRescue runs the TF-IDF pruning pass (IDE-aware per the
// client profile) with the advisory rescue hook armed when the rerank
// is enabled, recording how many borderline blocks were rescued.
func (t *TFIDFInterceptor) pruneWithRescue(ctx context.Context, req *InterceptRequest, text, query string, hardLimitRunes int) string {
	// Advisory TF-IDF rerank: borderline dropped blocks get one rescue
	// judgment per pass (strengthen-only). Armed only when the section
	// is enabled, the judge built, and a relevance query exists (an
	// empty query gives the judge no signal — every rescue would be
	// wasted engine spend).
	var rescue *TfidfRescueParams
	if t.rerankEnabled() && query != "" {
		rescue = t.buildRescueParams(ctx, req)
	}
	var truncated string
	scope := AgentNameFor(req)
	if req.Profile.IsIDE {
		truncated = TruncateTFIDFCodeAwareWithRescueMemo(text, hardLimitRunes, query, t.contextCfg, rescue, scope, t.selectionCache)
	} else {
		truncated = TruncateTFIDFWithRescueMemo(text, hardLimitRunes, query, t.contextCfg, rescue, scope, t.selectionCache)
	}
	if rescue != nil && rescue.Admitted > 0 {
		t.metrics.RecordTfidfRescues(rescue.Admitted)
		t.logger.Info("tfidf rerank rescued borderline blocks", "rescued", rescue.Admitted)
	}
	return truncated
}
