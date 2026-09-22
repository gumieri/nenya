package pipeline

import (
	"context"
	"log/slog"
	"math"
	"unicode/utf8"

	"github.com/nenya/config"
)

// TFIDFInterceptor applies TF-IDF relevance scoring to prune low-relevance
// content blocks, reducing token count while preserving query-relevant information.
// Priority: 30 — runs after redaction, before summarization.
type TFIDFInterceptor struct {
	name        string
	priority    int
	querySource string
	contextCfg  config.ContextConfig
	logger      *slog.Logger
}

// NewTFIDFInterceptor creates a new TFIDFInterceptor. contextCfg carries
// the operator's truncation keep percentages so TF-IDF fallback truncation
// honors them instead of collapsing to the separator alone.
func NewTFIDFInterceptor(querySource string, contextCfg config.ContextConfig, logger *slog.Logger) *TFIDFInterceptor {
	return &TFIDFInterceptor{
		name:        "tfidf",
		priority:    30,
		querySource: querySource,
		contextCfg:  contextCfg,
		logger:      logger,
	}
}

func (t *TFIDFInterceptor) Name() string  { return t.name }
func (t *TFIDFInterceptor) Priority() int { return t.priority }
func (t *TFIDFInterceptor) CanHandle(ctx context.Context, req *InterceptRequest) bool {
	if ctx.Err() != nil {
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

	var query string
	switch t.querySource {
	case "prior_messages":
		if len(req.Messages) > 1 {
			prior := make([]any, len(req.Messages)-1)
			for i, m := range req.Messages[:len(req.Messages)-1] {
				prior[i] = m
			}
			query = ExtractPriorUserMessages(prior, 5)
		}
	case "self":
		query = ExtractSelfQuery(text, 500)
	}

	// HardLimit is the token limit. TF-IDF operates on runes.
	// Tokens are ~3 runes on average, so multiply by 3 for the rune budget.
	hardLimitRunes := tfidfRuneBudget(req.HardLimit, req.SoftLimit)
	if hardLimitRunes <= 0 {
		// No usable budget (direct Process call bypassing CanHandle):
		// truncating to an empty budget would wipe the message.
		return &InterceptResult{Payload: req.Payload, Skip: true}, nil
	}

	var truncated string
	if req.Profile.IsIDE {
		truncated = TruncateTFIDFCodeAware(text, hardLimitRunes, query, t.contextCfg)
	} else {
		truncated = TruncateTFIDF(text, hardLimitRunes, query, t.contextCfg)
	}
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
