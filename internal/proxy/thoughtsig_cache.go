package proxy

import (
	"context"

	"github.com/nenya/internal/infra"
	"github.com/nenya/internal/stream"
)

// cacheExtraContentFromResponse extracts Gemini thought signatures
// (extra_content) from a parsed OpenAI-format response and stores them in the
// ThoughtSignatureCache (NENYA-51). Non-streaming responses bypass the SSE
// transformer entirely, so without this the signatures were dropped before
// the next turn could inject them, and the sanitizer stripped the
// just-completed tool pairs (silent context loss on Gemini 3).
//
// Handles both response shapes: streaming accumulation artifacts under
// choices[].delta and complete responses under choices[].message.
func cacheExtraContentFromResponse(cache *infra.ThoughtSignatureCache, responseMap map[string]interface{}) {
	if cache == nil || responseMap == nil {
		return
	}
	choices, ok := responseMap["choices"].([]interface{})
	if !ok {
		return
	}
	for _, choiceRaw := range choices {
		choice, ok := choiceRaw.(map[string]interface{})
		if !ok {
			continue
		}
		for _, section := range []string{"message", "delta"} {
			sectionMap, ok := choice[section].(map[string]interface{})
			if !ok {
				continue
			}
			toolCalls, ok := sectionMap["tool_calls"].([]interface{})
			if !ok {
				continue
			}
			for _, tcRaw := range toolCalls {
				tc, ok := tcRaw.(map[string]interface{})
				if !ok {
					continue
				}
				tcID, _ := tc["id"].(string)
				if tcID == "" {
					continue
				}
				extra, hasExtra := tc["extra_content"]
				if !hasExtra {
					continue
				}
				cache.Store(tcID, extra)
			}
		}
	}
}

// compositeTransformer runs two response transformers in sequence over the
// upstream SSE chunk: providerFirst (mutates/caches provider-specific fields
// on the upstream's native format) and then the format converter that
// rewrites the chunk for the client (NENYA-51). Without composition, an
// Anthropic-source client on a Gemini upstream short-circuited straight to
// the OpenAI→Anthropic converter, skipping the provider transformer and
// losing every thought signature.
type compositeTransformer struct {
	providerFirst stream.ResponseTransformer
	clientSecond  stream.ResponseTransformer
}

func (c *compositeTransformer) TransformSSEChunk(ctx context.Context, data []byte) ([]byte, error) {
	transformed, err := c.providerFirst.TransformSSEChunk(ctx, data)
	if err != nil {
		return nil, err
	}
	return c.clientSecond.TransformSSEChunk(ctx, transformed)
}
