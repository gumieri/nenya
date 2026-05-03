package pipeline

import (
	"math"
	"strings"
	"unicode/utf8"

	"nenya/config"
)

type Block struct {
	Content string
	IsCode  bool
}

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

func TruncateTFIDF(text string, maxSize int, query string, cfg config.GovernanceConfig) string {
	runes := []rune(text)
	if len(runes) <= maxSize {
		return text
	}

	query = capQueryRunes(query)
	separator := "\n... [NENYA: TF-IDF PRUNED] ...\n"
	sepLen := utf8.RuneCountInString(separator)
	available := maxSize - sepLen
	if available <= 0 {
		return string([]rune(separator)[:maxSize])
	}

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

	scored := scoreBlocks(query, middleBlocks)
	for i := range scored {
		scored[i].index = i
	}
	sortScoredDesc(scored)

	keptMiddle := selectKeptBlocks(scored, middleBlockRunes, middleBudget)

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

func calculateBudget(n int, blockRunes []int, cfg config.GovernanceConfig, available int) (pinFirst, pinLast, middleBudget, reservedForPinned int) {
	pinFirst = max(1, int(float64(n)*cfg.KeepFirstPercent/100.0))
	pinLast = max(1, int(float64(n)*cfg.KeepLastPercent/100.0))

	pinFirstRunes := 0
	for i := 0; i < pinFirst; i++ {
		pinFirstRunes += blockRunes[i]
	}
	pinLastRunes := 0
	for i := n - pinLast; i < n; i++ {
		pinLastRunes += blockRunes[i]
	}

	reservedForPinned = pinFirstRunes + pinLastRunes
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

func selectKeptBlocks(scored []scoredBlock, runes []int, budget int) map[int]bool {
	kept := make(map[int]bool, len(scored))
	currentRunes := 0
	for _, sb := range scored {
		if currentRunes+runes[sb.index] > budget {
			continue
		}
		kept[sb.index] = true
		currentRunes += runes[sb.index]
	}
	return kept
}

func assembleResult(blocks []Block, blockRunes []int, pinFirst, middleStart, middleEnd, n int, keptMiddle map[int]bool, separator string, available, reservedForPinned int) string {
	totalKept := 0
	for i := 0; i < pinFirst; i++ {
		totalKept += blockRunes[i]
	}
	for i, kept := range keptMiddle {
		if kept {
			totalKept += blockRunes[middleStart+i]
		}
	}

	var sb strings.Builder
	for i := 0; i < pinFirst; i++ {
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
		totalKept += blockRunes[i]
	}

	return sb.String()
}

func TruncateTFIDFCodeAware(text string, maxSize int, query string, cfg config.GovernanceConfig) string {
	result := TruncateTFIDF(text, maxSize, query, cfg)

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

func TruncateTFIDFHistory(historyText string, maxRunes int, query string, cfg config.GovernanceConfig) string {
	if maxRunes <= 0 {
		maxRunes = 4000
	}
	return TruncateTFIDF(historyText, maxRunes, query, cfg)
}

func sortScoredDesc(blocks []scoredBlock) {
	for i := 0; i < len(blocks)-1; i++ {
		maxIdx := i
		for j := i + 1; j < len(blocks); j++ {
			if blocks[j].score > blocks[maxIdx].score {
				maxIdx = j
			}
		}
		if maxIdx != i {
			blocks[i], blocks[maxIdx] = blocks[maxIdx], blocks[i]
		}
	}
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
