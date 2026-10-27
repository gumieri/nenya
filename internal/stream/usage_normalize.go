package stream

// This file holds the single normalization point for upstream token usage
// (NENYA-167). Every usage object Nenya reads — OpenAI-compatible, Anthropic
// streaming, or Gemini native — is reduced to one canonical shape so that each
// dimension is counted exactly once, whichever wire format carried it:
//
//   - Prompt is the TOTAL input, INCLUDING the cached and cache-write
//     dimensions (the OpenAI invariant). The cache counters are therefore
//     subsets of Prompt, never additional input.
//   - Completion is the TOTAL output, INCLUDING reasoning/thinking tokens
//     (the OpenAI invariant: completion_tokens_details.reasoning_tokens is a
//     subset of completion_tokens).
//   - CacheHit/CacheMiss/CacheCreation are the cache dimensions; a provider
//     that reports them separately from the input total (Anthropic native:
//     input_tokens excludes both cache_read_input_tokens and
//     cache_creation_input_tokens) has them folded into Prompt here rather
//     than at each recording site.
//   - Reasoning is the reasoning/thinking subset of Completion, kept as its
//     own counter for observability. It is never added on top of Completion,
//     because providers that report it separately (Gemini candidatesTokenCount
//     excludes thoughtsTokenCount) are summed into Completion above.
//   - CacheMiss is the uncached remainder of Prompt, so the cache counters
//     always sum to the input total (the hit-rate invariant).

// UsageTotals is a usage object normalized to Nenya's canonical shape. See the
// file comment for the invariants it guarantees.
type UsageTotals struct {
	// Prompt is the total input: uncached input plus the cache dimensions.
	Prompt int
	// Completion is the total output, including reasoning tokens.
	Completion int
	// Total is the provider-reported grand total when available, else
	// Prompt+Completion.
	Total int
	// CacheHit is the cache-read (prompt cache hit) subset of Prompt.
	CacheHit int
	// CacheMiss is the cache-miss (uncached input) subset of Prompt when the
	// provider reports the split explicitly.
	CacheMiss int
	// CacheCreation is the cache-write subset of Prompt.
	CacheCreation int
	// CacheCreation5m / CacheCreation1h are the per-TTL breakdown of
	// CacheCreation when the provider reports it (the Anthropic-native
	// `cache_creation` object: ephemeral_5m_input_tokens /
	// ephemeral_1h_input_tokens). Zero when unreported. Carried so the
	// buffered conversions can publish the same breakdown as the streaming
	// transformer (NENYA-310).
	CacheCreation5m int
	CacheCreation1h int
	// Reasoning is the reasoning/thinking subset of Completion.
	Reasoning int
	// TotalReported records that Total came from the provider instead of being
	// derived from Prompt+Completion. A derived total is per-chunk (the output
	// side of an Anthropic-native stream arrives on a later chunk), so
	// consumers accumulating deltas must derive it from the parts.
	TotalReported bool
	// AmbiguousCacheSemantics records that the object carried an OpenAI-style
	// input total together with the Anthropic-native cache field names and no
	// OpenAI-style cache decomposition, so the cache semantics had to be
	// inferred (the Anthropic reading was chosen: the cache dimensions are
	// folded into the input total). Worth a warning on a billing path.
	AmbiguousCacheSemantics bool
}

// UsageMapFromChunk returns the usage object carried by an upstream SSE chunk
// across every placement Nenya accepts:
//
//   - the OpenAI-compatible top-level "usage" (including usage-only final
//     chunks, the shape some providers such as Together send with no
//     choices);
//   - the Anthropic streaming shape, where the initial input usage is nested
//     under "message" (message_start) and the final output usage is top-level
//     (message_delta);
//   - Gemini's native "usageMetadata".
func UsageMapFromChunk(chunk map[string]interface{}) (map[string]interface{}, bool) {
	if usage, ok := asUsageMap(chunk["usage"]); ok {
		return usage, true
	}
	if message, ok := chunk["message"].(map[string]interface{}); ok {
		if usage, ok := asUsageMap(message["usage"]); ok {
			return usage, true
		}
	}
	if usage, ok := asUsageMap(chunk["usageMetadata"]); ok {
		return usage, true
	}
	return nil, false
}

func asUsageMap(raw interface{}) (map[string]interface{}, bool) {
	if raw == nil {
		return nil, false
	}
	usage, ok := raw.(map[string]interface{})
	if !ok || usage == nil {
		return nil, false
	}
	return usage, true
}

// NormalizeUsageTotals reduces a provider usage object to UsageTotals. Unknown
// shapes yield the zero value, and a shape that reports both an OpenAI-style
// total and a native field prefers the shape whose cache semantics the object
// actually follows (see usageFields.anthropicInputSemantics).
func NormalizeUsageTotals(usage map[string]interface{}) UsageTotals {
	if usage == nil {
		return UsageTotals{}
	}
	fields := readUsageFields(usage)
	totals := UsageTotals{
		CacheHit:        fields.cacheHit,
		CacheMiss:       fields.cacheMiss,
		CacheCreation:   fields.cacheCreation,
		Reasoning:       ExtractReasoningTokens(usage),
		CacheCreation5m: fields.cacheCreation5m,
		CacheCreation1h: fields.cacheCreation1h,
	}
	switch {
	case fields.geminiOnly():
		totals = fields.geminiTotals(totals)
	case fields.anthropicInputSemantics():
		totals = fields.anthropicTotals(totals)
	default:
		totals = fields.openAITotals(totals)
	}
	if totals.Total == 0 {
		totals.Total = totals.Prompt + totals.Completion
	}
	return totals
}

// hasGemini includes the total/cached-only fields: a usageMetadata object may
// carry only those, and routing it to the OpenAI branch would drop its cache
// count.
func (f usageFields) geminiOnly() bool {
	return f.hasGemini() || f.geminiTotal != 0 || f.geminiCached != 0
}

// usageFields holds the raw candidate fields of one usage object across every
// provider spelling, plus the OpenAI-style cache decomposition probe.
type usageFields struct {
	openAIInput  int
	openAIOutput int

	nativeInput         int
	nativeOutput        int
	nativeCacheRead     int
	nativeCacheCreation int

	geminiPrompt     int
	geminiCandidates int
	geminiThoughts   int
	geminiTotal      int
	geminiCached     int

	openAITotal int

	cacheHit      int
	cacheMiss     int
	cacheCreation int

	cacheCreation5m int
	cacheCreation1h int

	// openAICacheDecomposition records that the object decomposes its input
	// total into cache subsets (prompt_cache_hit_tokens,
	// prompt_tokens_details.cached_tokens, or cache_creation_tokens), which
	// proves the input total already includes the cache dimensions.
	openAICacheDecomposition bool
}

func readUsageFields(usage map[string]interface{}) usageFields {
	f := usageFields{
		openAIInput:         ToInt(usage["prompt_tokens"]),
		openAIOutput:        ToInt(usage["completion_tokens"]),
		nativeInput:         ToInt(usage["input_tokens"]),
		nativeOutput:        ToInt(usage["output_tokens"]),
		nativeCacheRead:     ToInt(usage["cache_read_input_tokens"]),
		nativeCacheCreation: ToInt(usage["cache_creation_input_tokens"]),
		geminiPrompt:        ToInt(usage["promptTokenCount"]),
		geminiCandidates:    ToInt(usage["candidatesTokenCount"]),
		geminiThoughts:      ToInt(usage["thoughtsTokenCount"]),
		geminiTotal:         ToInt(usage["totalTokenCount"]),
		geminiCached:        ToInt(usage["cachedContentTokenCount"]),
		openAITotal:         ToInt(usage["total_tokens"]),
		cacheHit:            ToInt(usage["prompt_cache_hit_tokens"]),
		cacheMiss:           ToInt(usage["prompt_cache_miss_tokens"]),
		cacheCreation:       ToInt(usage["cache_creation_tokens"]),
	}
	// The Anthropic-native per-TTL breakdown (NENYA-310): parsed in every
	// dialect — the key is unambiguous, and a buffered OpenAI-format response
	// from an Anthropic-relaying aggregator carries it verbatim.
	if cc, ok := usage["cache_creation"].(map[string]interface{}); ok {
		f.cacheCreation5m = ToInt(cc["ephemeral_5m_input_tokens"])
		f.cacheCreation1h = ToInt(cc["ephemeral_1h_input_tokens"])
	}
	f.openAICacheDecomposition = f.cacheHit > 0 || f.cacheCreation > 0
	if f.cacheHit == 0 {
		if details, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
			if cached := ToInt(details["cached_tokens"]); cached > 0 {
				f.cacheHit = cached
				f.openAICacheDecomposition = true
			}
		}
	}
	return f
}

func (f usageFields) hasGemini() bool {
	return f.geminiPrompt != 0 || f.geminiCandidates != 0 || f.geminiThoughts != 0
}

// ambiguousCacheSemantics reports that the object carries an OpenAI-style input
// total together with the Anthropic-native cache field names and no OpenAI
// cache decomposition: the cache semantics can only be inferred.
func (f usageFields) ambiguousCacheSemantics() bool {
	if f.nativeInput != 0 || f.nativeOutput != 0 || f.openAICacheDecomposition {
		return false
	}
	if f.nativeCacheRead == 0 && f.nativeCacheCreation == 0 {
		return false
	}
	// Only an input total can be folded into; an output-only object carries
	// nothing to misinterpret.
	return f.openAIInput != 0
}

// anthropicInputSemantics reports whether the object's input field EXCLUDES the
// cache dimensions (Anthropic semantics). That holds for the native field
// names, and also for the OpenAI key names when the object carries the native
// cache fields without an OpenAI-style decomposition — the shape a gateway in
// front of Anthropic produces when it passes the Anthropic input semantics
// through under prompt_tokens.
func (f usageFields) anthropicInputSemantics() bool {
	if f.nativeInput != 0 || f.nativeOutput != 0 {
		return true
	}
	if f.openAICacheDecomposition {
		return false
	}
	return f.nativeCacheRead != 0 || f.nativeCacheCreation != 0
}

// anthropicTotals folds the cache dimensions into the input total, which
// excludes them in the Anthropic shape, and publishes the uncached remainder as
// the cache miss so the three cache counters always sum to the input total
// (the hit-rate invariant documented in docs/CACHING.md).
func (f usageFields) anthropicTotals(totals UsageTotals) UsageTotals {
	baseInput := f.nativeInput
	if baseInput == 0 {
		baseInput = f.openAIInput
	}
	baseOutput := f.nativeOutput
	if baseOutput == 0 {
		baseOutput = f.openAIOutput
	}
	totals.Prompt = baseInput + f.nativeCacheRead + f.nativeCacheCreation
	totals.Completion = baseOutput
	totals.Total = totals.Prompt + totals.Completion
	// The native fields describe the excluded cache dimensions; an OpenAI-style
	// decomposition may describe the same tokens under the inclusive reading,
	// so take whichever value is present and derive the miss as the remainder,
	// keeping hit + miss + creation == prompt.
	totals.CacheHit = max(f.nativeCacheRead, f.cacheHit)
	totals.CacheCreation = max(f.nativeCacheCreation, f.cacheCreation)
	totals.CacheMiss = max(0, totals.Prompt-totals.CacheHit-totals.CacheCreation)
	totals.AmbiguousCacheSemantics = f.ambiguousCacheSemantics()
	return totals
}

// geminiTotals maps the Gemini-native shape: candidatesTokenCount excludes
// thoughtsTokenCount, so the thought tokens are added to the completion total
// (they are a subset of completion in the OpenAI shape).
func (f usageFields) geminiTotals(totals UsageTotals) UsageTotals {
	totals.Prompt = f.geminiPrompt
	totals.Completion = f.geminiCandidates + f.geminiThoughts
	totals.Total = f.geminiTotal
	totals.TotalReported = f.geminiTotal != 0
	totals.CacheHit = f.geminiCached
	totals.CacheMiss = max(0, f.geminiPrompt-f.geminiCached)
	return totals
}

// openAITotals maps the OpenAI-compatible shape, where the input total already
// includes the cache dimensions; the native cache fields are then only
// observability duplicates and must not be folded in again.
func (f usageFields) openAITotals(totals UsageTotals) UsageTotals {
	totals.Prompt = f.openAIInput
	totals.Completion = f.openAIOutput
	totals.Total = f.openAITotal
	totals.TotalReported = f.openAITotal != 0
	// AUD-28 (NENYA-240): keep the hit-rate invariant — when the upstream
	// decomposes the cache dimensions but reports no explicit miss, derive
	// the miss as the uncached remainder of Prompt (hit + miss + creation
	// == prompt), exactly like the anthropic and gemini branches. An
	// explicitly reported miss always wins (it is the provider's own
	// number). Pre-fix CacheMiss stayed 0 for OpenAI-shape decompositions
	// without an explicit miss, so miss counters and hit-rate ratios read
	// 100% hits for partially-cached traffic. With no decomposition there
	// is nothing to derive and nothing reported: both stay zero.
	if f.cacheMiss == 0 && (totals.CacheHit > 0 || totals.CacheCreation > 0) {
		totals.CacheMiss = max(0, totals.Prompt-totals.CacheHit-totals.CacheCreation)
	}
	return totals
}
