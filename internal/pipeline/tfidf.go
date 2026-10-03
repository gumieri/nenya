package pipeline

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/nenya/config"
	"github.com/nenya/internal/util"
)

// DefaultTfidfSelectionCacheSize bounds the TF-IDF selection memo by entry
// count. The memo freezes a piece of content's keep/drop decision on first
// scoring so later turns reuse it even as the prior-messages query evolves.
const DefaultTfidfSelectionCacheSize = 256

// TfidfSelectionCache is a bounded LRU of TF-IDF block selections keyed by the
// selection scope plus the input content (and the size/config that affect the
// selection, never the query). Freezing the decision makes prior_messages-mode
// TF-IDF output stable across turns, so the provider prompt-cache prefix
// survives. Safe for concurrent use; a nil cache disables memoization.
//
// Each entry stores only the set of kept middle-block indexes, not the output
// text, so memory is bounded by entry count times the per-entry block count.
// Concurrent first-time misses on the same key each compute independently (and
// each may call the rescue judge); the first stored decision wins. The key
// covers the rescue band/limits but not the judge engine identity, so a reload
// that swaps the TF-IDF rerank engine reuses frozen verdicts for content
// already scored until those entries age out.
type TfidfSelectionCache struct {
	mu         sync.Mutex
	order      *list.List
	entries    map[string]*list.Element
	maxEntries int
}

type tfidfSelectionEntry struct {
	key  string
	kept map[int]bool
}

// NewTfidfSelectionCache returns a cache bounded to maxEntries (default
// DefaultTfidfSelectionCacheSize when non-positive).
func NewTfidfSelectionCache(maxEntries int) *TfidfSelectionCache {
	if maxEntries <= 0 {
		maxEntries = DefaultTfidfSelectionCacheSize
	}
	return &TfidfSelectionCache{
		order:      list.New(),
		entries:    make(map[string]*list.Element),
		maxEntries: maxEntries,
	}
}

// tfidfSelectionKey hashes the selection scope (e.g. the agent name, so a
// judge-rescued selection is never replayed to a different agent), the input
// content, and the parameters that change the block selection (size, keep
// percentages, and an armed rescue's band/limits). The query and its source
// (self vs prior_messages) are deliberately excluded: the first decision for a
// piece of content is frozen regardless of later queries or mode. Floats are
// hashed by their exact bit pattern so near-equal percentages never conflate
// two classes. A nil rescue and a rescue with no Rescorer behave identically
// and share a key; only an armed rescue hashes distinctly.
func tfidfSelectionKey(scope, text string, maxSize int, cfg config.ContextConfig, rescue *TfidfRescueParams) string {
	h := sha256.New()
	_, _ = h.Write([]byte(scope))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(text))
	_, _ = h.Write([]byte{0})
	_, _ = fmt.Fprintf(h, "|m=%d|kf=%x|kl=%x", maxSize,
		math.Float64bits(cfg.TruncationKeepFirstPct), math.Float64bits(cfg.TruncationKeepLastPct))
	if rescue != nil && rescue.Rescorer != nil {
		_, _ = fmt.Fprintf(h, "|rb=%x|rmb=%d|rmx=%d|rr=1",
			math.Float64bits(rescue.Band), rescue.MaxBlocks, rescue.MaxBytes)
	} else {
		_, _ = h.Write([]byte("|rr=0"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// get returns the memoized selection for key and marks it recently used. The
// returned map is owned by the cache and MUST be treated as read-only by the
// caller (it is shared with concurrent readers; mutating it would race).
func (c *TfidfSelectionCache) get(key string) (map[int]bool, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*tfidfSelectionEntry).kept, true
}

// put records a selection. An existing key is left untouched so the first
// decision wins even under a concurrent miss.
func (c *TfidfSelectionCache) put(key string, kept map[int]bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok {
		c.order.MoveToFront(el)
		return
	}
	el := c.order.PushFront(&tfidfSelectionEntry{key: key, kept: kept})
	c.entries[key] = el
	for c.order.Len() > c.maxEntries {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.entries, back.Value.(*tfidfSelectionEntry).key)
	}
}

// size returns the current entry count. Test-only helper (production does not
// need to observe the bound).
func (c *TfidfSelectionCache) size() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Block represents a contiguous text segment, tagged as code or prose.
type Block struct {
	Content string
	IsCode  bool
}

// scoredBlock wraps a Block with its TF-IDF relevance score and original index.
type scoredBlock struct {
	block Block
	score float64
	index int
}

func splitIntoBlocks(text string) []Block {
	codeSpans := DetectCodeFences(text)

	type region struct {
		start, end int
		isCode     bool
	}

	var regions []region
	lastEnd := 0

	for _, span := range codeSpans {
		if span.Start > lastEnd {
			prose := strings.TrimSpace(text[lastEnd:span.Start])
			if prose != "" {
				regions = append(regions, region{start: lastEnd, end: span.Start, isCode: false})
			}
		}
		regions = append(regions, region{start: span.Start, end: span.End, isCode: true})
		lastEnd = span.End
	}

	if lastEnd < len(text) {
		prose := strings.TrimSpace(text[lastEnd:])
		if prose != "" {
			regions = append(regions, region{start: lastEnd, end: len(text), isCode: false})
		}
	}

	if len(regions) == 0 {
		return []Block{{Content: strings.TrimSpace(text), IsCode: false}}
	}

	var blocks []Block
	for _, r := range regions {
		content := text[r.start:r.end]
		if r.isCode {
			blocks = append(blocks, Block{Content: content, IsCode: true})
		} else {
			paragraphs := strings.Split(content, "\n\n")
			for _, p := range paragraphs {
				p = strings.TrimSpace(p)
				if p != "" {
					blocks = append(blocks, Block{Content: p, IsCode: false})
				}
			}
		}
	}

	if len(blocks) == 0 {
		blocks = append(blocks, Block{Content: strings.TrimSpace(text), IsCode: false})
	}

	return blocks
}

var punctReplacer = strings.NewReplacer(
	".", " ", ",", " ", ";", " ", ":", " ", "!", " ", "?", " ",
	"(", " ", ")", " ", "[", " ", "]", " ", "{", " ", "}", " ",
	"\"", " ", "'", " ", "`", " ", "<", " ", ">", " ",
	"=", " ", "+", " ", "-", " ", "/", " ", "\\", " ",
	"|", " ", "&", " ", "%", " ", "#", " ", "@", " ",
	"^", " ", "~", " ", "*", " ",
)

func tokenize(text string) []string {
	lower := strings.ToLower(text)
	cleaned := punctReplacer.Replace(lower)
	fields := strings.Fields(cleaned)
	return fields
}

func termFreq(tokens []string) map[string]float64 {
	if len(tokens) == 0 {
		return nil
	}
	counts := make(map[string]int, len(tokens))
	for _, t := range tokens {
		counts[t]++
	}
	tf := make(map[string]float64, len(counts))
	total := float64(len(tokens))
	for term, c := range counts {
		tf[term] = float64(c) / total
	}
	return tf
}

func inverseDocFreq(docs [][]string) map[string]float64 {
	n := len(docs)
	if n == 0 {
		return nil
	}
	df := make(map[string]int)
	for _, doc := range docs {
		seen := make(map[string]struct{}, len(doc))
		for _, t := range doc {
			if _, exists := seen[t]; !exists {
				seen[t] = struct{}{}
				df[t]++
			}
		}
	}
	idf := make(map[string]float64, len(df))
	for term, d := range df {
		idf[term] = math.Log(float64(n+1) / float64(d+1))
	}
	return idf
}

func scoreBlocks(query string, blocks []Block) []scoredBlock {
	queryTokens := tokenize(query)
	if len(queryTokens) == 0 || len(blocks) == 0 {
		result := make([]scoredBlock, len(blocks))
		for i, b := range blocks {
			result[i] = scoredBlock{block: b, score: 0, index: i}
		}
		return result
	}

	blockTokens := make([][]string, len(blocks))
	for i, b := range blocks {
		blockTokens[i] = tokenize(b.Content)
	}

	idf := inverseDocFreq(blockTokens)

	result := make([]scoredBlock, len(blocks))
	for i, b := range blocks {
		bTokens := blockTokens[i]
		tf := termFreq(bTokens)
		score := 0.0
		for _, qt := range queryTokens {
			tfVal := tf[qt]
			if tfVal == 0 {
				continue
			}
			idfVal := idf[qt]
			if idfVal == 0 {
				idfVal = 1.0
			}
			score += tfVal * idfVal
		}
		result[i] = scoredBlock{block: b, score: score, index: i}
	}
	return result
}

// TfidfRescueParams carries the advisory rerank hooks for one pruning
// pass (nil TfidfRescueParams = deterministic pruning only). The
// rescorer may rescue dropped borderline blocks within the leftover
// budget; it can never displace deterministically kept blocks.
type TfidfRescueParams struct {
	// Band is the ambiguous half-width below the deterministic cutoff
	// score: dropped blocks scoring within [cutoff-band, cutoff) are
	// borderline and offered to the rescorer.
	Band float64
	// MaxBlocks caps how many borderline blocks are offered per pass.
	MaxBlocks int
	// MaxBytes caps the total borderline excerpt bytes offered (runes
	// treated as ~bytes for this cap).
	MaxBytes int
	// Rescorer adjudicates the borderline set and returns the slice
	// indexes to rescue (fail-open implementations return nil/empty).
	Rescorer BlockRescorer
	// Admitted is an output field: selectKeptBlocks reports how many
	// rescued blocks actually re-entered within the budget (judge
	// verdicts can be overturned by the budget check).
	Admitted int
	// Failed is an output field: set by the rescorer when the judgement
	// failed operationally or the excerpt was truncated. A failed rescue is
	// never memoized, so the next turn re-judges instead of freezing the
	// fallback (no-rescue) selection.
	Failed bool
}

// BlockRescorer adjudicates borderline dropped blocks: it receives the
// query and the borderline excerpts (band-capped) and returns the
// indexes within the borderline slice to rescue.
type BlockRescorer func(query string, borderline []Block) map[int]bool

func TruncateTFIDF(text string, maxSize int, query string, cfg config.ContextConfig) string {
	return truncateTFIDF(text, maxSize, query, cfg, nil, "", nil)
}

// TruncateTFIDFWithRescue is TruncateTFIDF with the advisory rescue
// hook: borderline dropped blocks may re-enter within leftover budget.
func TruncateTFIDFWithRescue(text string, maxSize int, query string, cfg config.ContextConfig, rescue *TfidfRescueParams) string {
	return truncateTFIDF(text, maxSize, query, cfg, rescue, "", nil)
}

// TruncateTFIDFWithRescueMemo is TruncateTFIDFWithRescue with the selection
// memo: the first keep/drop decision for a piece of content is frozen and
// reused on later calls, regardless of the query (prompt-cache stability).
// scope isolates memo entries (pass the agent name when a rescue judge is
// armed so one agent's verdict is never replayed to another).
func TruncateTFIDFWithRescueMemo(text string, maxSize int, query string, cfg config.ContextConfig, rescue *TfidfRescueParams, scope string, memo *TfidfSelectionCache) string {
	return truncateTFIDF(text, maxSize, query, cfg, rescue, scope, memo)
}

func truncateTFIDF(text string, maxSize int, query string, cfg config.ContextConfig, rescue *TfidfRescueParams, scope string, memo *TfidfSelectionCache) string {
	runes := []rune(text)
	if len(runes) <= maxSize {
		return text
	}

	query = capQueryRunes(query)
	separator := "\n... [NENYA: TF-IDF PRUNED] ...\n"
	sepLen := utf8.RuneCountInString(separator)

	if maxSize <= sepLen {
		return TruncateMiddleOut(text, maxSize, cfg)
	}
	available := maxSize - sepLen

	blocks := splitIntoBlocks(text)
	if len(blocks) <= 1 {
		return TruncateMiddleOut(text, maxSize, cfg)
	}

	blockRunes := make([]int, len(blocks))
	for i, b := range blocks {
		blockRunes[i] = utf8.RuneCountInString(b.Content)
	}

	n := len(blocks)
	pinFirst, pinLast, middleBudget, reservedForPinned := calculateBudget(n, blockRunes, cfg, available)
	if pinFirst+pinLast >= n {
		return TruncateMiddleOut(text, maxSize, cfg)
	}

	middleStart := pinFirst
	middleEnd := n - pinLast
	middleBlocks := blocks[middleStart:middleEnd]
	middleBlockRunes := blockRunes[middleStart:middleEnd]

	// Frozen selection: reuse the keep/drop decision recorded the first time
	// this content was scored so the output does not drift with the evolving
	// prior-messages query. On a hit the rescore hook is not called and
	// rescue.Admitted stays 0, so nenya_tfidf_rescues_total counts only fresh
	// rescues (a frozen rescue is not re-counted each turn).
	key := ""
	if memo != nil {
		key = tfidfSelectionKey(scope, text, maxSize, cfg, rescue)
		if kept, ok := memo.get(key); ok {
			return tfidfAssemble(blocks, blockRunes, pinFirst, middleStart, middleEnd, n, kept, separator, available, reservedForPinned, maxSize, cfg)
		}
	}

	scored := scoreBlocks(query, middleBlocks)
	for i := range scored {
		scored[i].index = i
	}
	sortScoredDesc(scored)

	keptMiddle := selectKeptBlocks(scored, middleBlockRunes, middleBudget, query, rescue)
	// Freeze the decision unless an armed rescorer failed this pass: a
	// transient judge failure must not become a permanent no-rescue selection.
	if memo != nil && (rescue == nil || !rescue.Failed) {
		memo.put(key, keptMiddle)
	}

	return tfidfAssemble(blocks, blockRunes, pinFirst, middleStart, middleEnd, n, keptMiddle, separator, available, reservedForPinned, maxSize, cfg)
}

// tfidfAssemble joins the selected blocks and applies the same over-budget
// middle-out fallback the deterministic pass uses.
func tfidfAssemble(blocks []Block, blockRunes []int, pinFirst, middleStart, middleEnd, n int, keptMiddle map[int]bool, separator string, available, reservedForPinned, maxSize int, cfg config.ContextConfig) string {
	result := assembleResult(blocks, blockRunes, pinFirst, middleStart, middleEnd, n, keptMiddle, separator, available, reservedForPinned)
	if utf8.RuneCountInString(result) > maxSize {
		return TruncateMiddleOut(result, maxSize, cfg)
	}
	return result
}

func capQueryRunes(query string) string {
	const maxQueryRunes = 2000
	if utf8.RuneCountInString(query) > maxQueryRunes {
		query = string([]rune(query)[:maxQueryRunes])
	}
	return query
}

func calculateBudget(n int, blockRunes []int, cfg config.ContextConfig, available int) (pinFirst, pinLast, middleBudget, reservedForPinned int) {
	pinFirst = max(1, int(float64(n)*cfg.TruncationKeepFirstPct/100.0))
	pinLast = max(1, int(float64(n)*cfg.TruncationKeepLastPct/100.0))

	pinFirstRunes := 0
	for i := range pinFirst {
		pinFirstRunes += blockRunes[i]
	}
	pinLastRunes := 0
	for i := n - pinLast; i < n; i++ {
		pinLastRunes += blockRunes[i]
	}

	reservedForPinned = util.AddCap(pinFirstRunes, pinLastRunes)
	maxReserved := int(float64(available) * 0.5)
	if reservedForPinned > maxReserved {
		reservedForPinned = maxReserved
	}

	middleBudget = available - reservedForPinned
	if middleBudget <= 0 {
		middleBudget = available / 3
	}
	return
}

// selectKeptBlocks greedily keeps highest-scoring blocks within the
// middle budget, then offers borderline dropped blocks (scoring within
// band below the kept cutoff) to the advisory rescorer. With the rerank
// armed, the greedy pass reserves headroom (a quarter of the budget,
// capped by the excerpt budget) so rescued blocks have room to re-enter
// within the budget invariant; with the rerank disabled the selection
// is byte-identical to the historical deterministic pass. Rescued
// blocks only consume leftover budget, so the keep set can grow but
// never shrink.
func selectKeptBlocks(scored []scoredBlock, runes []int, budget int, query string, rescue *TfidfRescueParams) map[int]bool {
	if rescue != nil {
		// Reset the output fields so a params struct reused across passes does
		// not accumulate a stale admitted count or stay marked failed.
		rescue.Failed = false
		rescue.Admitted = 0
	}
	kept := make(map[int]bool, len(scored))
	currentRunes := 0
	// cutoff tracks the lowest kept score (the keep/drop threshold).
	// sortScoredDesc guarantees non-increasing score order, so the last
	// keep carries the minimum; the len(kept)==0 early return below
	// guarantees cutoff is initialized whenever collectBorderline runs.
	cutoff := 0.0
	greedyBudget := greedyBudgetFor(budget, rescue)
	for _, sb := range scored {
		if currentRunes+runes[sb.index] > greedyBudget {
			continue
		}
		kept[sb.index] = true
		currentRunes += runes[sb.index]
		if cutoff == 0.0 || sb.score < cutoff {
			cutoff = sb.score
		}
	}
	if rescue == nil || rescue.Rescorer == nil || len(kept) == 0 || len(kept) == len(scored) {
		return kept
	}
	borderline := collectBorderline(scored, runes, kept, cutoff, rescue)
	if len(borderline) == 0 {
		return kept
	}

	blocks := make([]Block, len(borderline))
	for i, sb := range borderline {
		blocks[i] = sb.block
	}
	rescued := rescue.Rescorer(query, blocks)
	for i, sb := range borderline {
		if !rescued[i] || currentRunes+runes[sb.index] > budget {
			continue
		}
		kept[sb.index] = true
		currentRunes += runes[sb.index]
		rescue.Admitted++
	}
	return kept
}

// greedyBudgetFor computes the deterministic greedy budget: with the
// rerank armed, a quarter of the budget (capped by a third of the
// excerpt byte budget — bytes are ~3 runes, the same heuristic the
// interceptor uses for token budgets) is reserved as headroom so
// rescued blocks have room to re-enter; with the rerank disabled the
// full budget applies.
func greedyBudgetFor(budget int, rescue *TfidfRescueParams) int {
	if rescue == nil || rescue.Rescorer == nil {
		return budget
	}
	reserve := budget / 4
	if capBytes := rescue.MaxBytes / 3; capBytes < reserve {
		reserve = capBytes
	}
	if reserve <= 0 {
		return budget
	}
	return budget - reserve
}

// collectBorderline gathers dropped blocks scoring within the rescue
// band below the kept cutoff (cutoff-band <= score < cutoff — the
// ambiguous band around the keep/drop threshold), in score order,
// capped by the rescue block/byte limits.
func collectBorderline(scored []scoredBlock, runes []int, kept map[int]bool, cutoff float64, rescue *TfidfRescueParams) []scoredBlock {
	var borderline []scoredBlock
	borderlineRunes := 0
	for _, sb := range scored {
		if kept[sb.index] || len(borderline) >= rescue.MaxBlocks {
			continue
		}
		if sb.score >= cutoff || sb.score < cutoff-rescue.Band {
			continue
		}
		if borderlineRunes+runes[sb.index] > rescue.MaxBytes {
			continue
		}
		borderline = append(borderline, sb)
		borderlineRunes += runes[sb.index]
	}
	return borderline
}

func assembleResult(blocks []Block, blockRunes []int, pinFirst, middleStart, middleEnd, n int, keptMiddle map[int]bool, separator string, available, reservedForPinned int) string {
	totalKept := 0
	for i := range pinFirst {
		totalKept = util.AddCap(totalKept, blockRunes[i])
	}
	for i, kept := range keptMiddle {
		if kept {
			totalKept = util.AddCap(totalKept, blockRunes[middleStart+i])
		}
	}

	var sb strings.Builder
	for i := range pinFirst {
		sb.WriteString(blocks[i].Content)
	}

	insertedSep := false
	for i := middleStart; i < middleEnd; i++ {
		if !keptMiddle[i-middleStart] {
			continue
		}
		if !insertedSep {
			sb.WriteString(separator)
			insertedSep = true
		}
		sb.WriteString(blocks[i].Content)
	}

	if !insertedSep {
		sb.WriteString(separator)
	}

	for i := middleEnd; i < n; i++ {
		if totalKept+blockRunes[i] > available {
			break
		}
		sb.WriteString(blocks[i].Content)
		totalKept = util.AddCap(totalKept, blockRunes[i])
	}

	return sb.String()
}

func TruncateTFIDFCodeAware(text string, maxSize int, query string, cfg config.ContextConfig) string {
	return TruncateTFIDFCodeAwareWithRescue(text, maxSize, query, cfg, nil)
}

// TruncateTFIDFCodeAwareWithRescue is TruncateTFIDFCodeAware with the
// advisory rescue hook.
func TruncateTFIDFCodeAwareWithRescue(text string, maxSize int, query string, cfg config.ContextConfig, rescue *TfidfRescueParams) string {
	return codeAwareAdjust(truncateTFIDF(text, maxSize, query, cfg, rescue, "", nil))
}

// TruncateTFIDFCodeAwareWithRescueMemo is TruncateTFIDFCodeAwareWithRescue
// with the selection memo (see TfidfSelectionCache). scope isolates entries
// (agent name when a rescue judge is armed).
func TruncateTFIDFCodeAwareWithRescueMemo(text string, maxSize int, query string, cfg config.ContextConfig, rescue *TfidfRescueParams, scope string, memo *TfidfSelectionCache) string {
	return codeAwareAdjust(truncateTFIDF(text, maxSize, query, cfg, rescue, scope, memo))
}

// codeAwareAdjust trims partial prose adjacent to the pruning separator so
// the kept spans land on paragraph boundaries.
func codeAwareAdjust(result string) string {
	sepMarker := "\n... [NENYA: TF-IDF PRUNED] ...\n"
	sepIdx := strings.Index(result, sepMarker)
	if sepIdx < 0 {
		return result
	}

	before := result[:sepIdx]
	after := result[sepIdx+len(sepMarker):]

	if lastBlank := strings.LastIndex(before, "\n\n"); lastBlank > 0 {
		before = before[:lastBlank+2]
	}

	if firstBlank := strings.Index(after, "\n\n"); firstBlank > 0 {
		after = after[firstBlank:]
	}

	return before + sepMarker + after
}

func TruncateTFIDFHistory(historyText string, maxRunes int, query string, cfg config.ContextConfig) string {
	if maxRunes <= 0 {
		maxRunes = 4000
	}
	return TruncateTFIDF(historyText, maxRunes, query, cfg)
}

func sortScoredDesc(blocks []scoredBlock) {
	// NENYA-23 (F2): tie-break equal scores by original block index so
	// near-identical payloads cannot permute equal-score block selection.
	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].score == blocks[j].score {
			return blocks[i].index < blocks[j].index
		}
		return blocks[i].score > blocks[j].score
	})
}

func ExtractPriorUserMessages(messages []interface{}, maxMessages int) string {
	if len(messages) == 0 {
		return ""
	}

	userMsgs := make([]string, 0, maxMessages)
	for i := len(messages) - 1; i >= 0 && len(userMsgs) < maxMessages; i-- {
		msg, ok := messages[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "user" {
			continue
		}
		text := ExtractContentText(msg)
		if text != "" {
			userMsgs = append(userMsgs, text)
		}
	}

	for i, j := 0, len(userMsgs)-1; i < j; i, j = i+1, j-1 {
		userMsgs[i], userMsgs[j] = userMsgs[j], userMsgs[i]
	}

	return strings.Join(userMsgs, " ")
}

func ExtractSelfQuery(text string, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 {
		maxRunes = 500
	}
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes])
}
