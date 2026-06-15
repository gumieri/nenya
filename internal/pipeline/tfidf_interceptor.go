package pipeline

import (
	"context"
	"log/slog"
	"math"

	"github.com/nenya/config"
)

// TFIDFInterceptor applies TF-IDF relevance scoring to prune low-relevance
// content blocks, reducing token count while preserving query-relevant information.
// Priority: 30 — runs after redaction, before summarization.
type TFIDFInterceptor struct {
	name        string
	priority    int
	querySource string
	logger      *slog.Logger
}

// NewTFIDFInterceptor creates a new TFIDFInterceptor.
func NewTFIDFInterceptor(querySource string, logger *slog.Logger) *TFIDFInterceptor {
	return &TFIDFInterceptor{
		name:        "tfidf",
		priority:    30,
		querySource: querySource,
		logger:      logger,
	}
}

func (t *TFIDFInterceptor) Name() string  { return t.name }
func (t *TFIDFInterceptor) Priority() int { return t.priority }
func (t *TFIDFInterceptor) CanHandle(_ context.Context, req *InterceptRequest) bool {
	return t.querySource != "" && len(req.Messages) > 1 && req.SoftLimit > 0 && req.TokenCount > req.SoftLimit
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
	hardLimitRunes := req.HardLimit
	if hardLimitRunes > 0 {
		if hardLimitRunes > math.MaxInt/3 {
			hardLimitRunes = math.MaxInt
		} else {
			hardLimitRunes *= 3
		}
	} else {
		hardLimitRunes = req.SoftLimit * 3
	}

	var truncated string
	if req.Profile.IsIDE {
		truncated = TruncateTFIDFCodeAware(text, hardLimitRunes, query, config.ContextConfig{})
	} else {
		truncated = TruncateTFIDF(text, hardLimitRunes, query, config.ContextConfig{})
	}

	lastMsg["content"] = truncated
	req.Payload["messages"] = req.Messages

	return &InterceptResult{
		Payload:   req.Payload,
		Truncated: true,
		Reason:    "tfidf_pruned",
	}, nil
}
