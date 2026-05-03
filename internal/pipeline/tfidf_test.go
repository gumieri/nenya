package pipeline

import (
	"math"
	"strings"
	"testing"
	"unicode/utf8"

	"nenya/config"
)

func TestTokenize(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "simple words",
			input: "hello world foo",
			want:  []string{"hello", "world", "foo"},
		},
		{
			name:  "punctuation stripped",
			input: "hello, world! foo.bar",
			want:  []string{"hello", "world", "foo", "bar"},
		},
		{
			name:  "case lowered",
			input: "Hello WORLD Foo",
			want:  []string{"hello", "world", "foo"},
		},
		{
			name:  "special chars stripped",
			input: "func(x int) -> [string]",
			want:  []string{"func", "x", "int", "string"},
		},
		{
			name:  "empty string",
			input: "",
			want:  nil,
		},
		{
			name:  "whitespace only",
			input: "   \t\n  ",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tokenize(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("len mismatch: got %d, want %d (got=%v, want=%v)", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("token[%d]: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestTermFreq(t *testing.T) {
	tokens := []string{"a", "b", "a", "c", "a", "b"}
	tf := termFreq(tokens)

	if len(tf) != 3 {
		t.Fatalf("expected 3 terms, got %d", len(tf))
	}

	if math.Abs(tf["a"]-0.5) > 1e-9 {
		t.Errorf("tf[a] = %f, want 0.5", tf["a"])
	}
	if math.Abs(tf["b"]-2.0/6.0) > 1e-9 {
		t.Errorf("tf[b] = %f, want %f", tf["b"], 2.0/6.0)
	}
	if math.Abs(tf["c"]-1.0/6.0) > 1e-9 {
		t.Errorf("tf[c] = %f, want %f", tf["c"], 1.0/6.0)
	}
}

func TestInverseDocFreq(t *testing.T) {
	docs := [][]string{
		{"a", "b", "c"},
		{"a", "b", "d"},
		{"a", "e", "f"},
	}
	idf := inverseDocFreq(docs)

	if len(idf) != 6 {
		t.Fatalf("expected 6 terms, got %d", len(idf))
	}

	idfA := idf["a"]
	expectedA := math.Log(float64(4) / float64(4))
	if math.Abs(idfA-expectedA) > 1e-9 {
		t.Errorf("idf[a] = %f, want %f", idfA, expectedA)
	}

	idfB := idf["b"]
	expectedB := math.Log(float64(4) / float64(3))
	if math.Abs(idfB-expectedB) > 1e-9 {
		t.Errorf("idf[b] = %f, want %f", idfB, expectedB)
	}
}

func TestInverseDocFreqEmpty(t *testing.T) {
	idf := inverseDocFreq(nil)
	if idf != nil {
		t.Errorf("expected nil for nil input, got %v", idf)
	}
	idf = inverseDocFreq([][]string{})
	if idf != nil {
		t.Errorf("expected nil for empty input, got %v", idf)
	}
}

func TestSplitIntoBlocks(t *testing.T) {
	t.Run("paragraphs", func(t *testing.T) {
		text := "first paragraph\n\nsecond paragraph\n\nthird paragraph"
		blocks := splitIntoBlocks(text)
		if len(blocks) != 3 {
			t.Fatalf("expected 3 blocks, got %d", len(blocks))
		}
		for _, b := range blocks {
			if b.IsCode {
				t.Error("expected no code blocks")
			}
		}
		if strings.TrimSpace(blocks[0].Content) != "first paragraph" {
			t.Errorf("block[0] = %q", blocks[0].Content)
		}
	})

	t.Run("code fences preserved", func(t *testing.T) {
		text := "intro\n\n```go\nfunc main() {}\n```\n\noutro"
		blocks := splitIntoBlocks(text)
		if len(blocks) != 3 {
			t.Fatalf("expected 3 blocks (intro, code, outro), got %d: %+v", len(blocks), blocks)
		}
		if blocks[0].IsCode {
			t.Error("block[0] should not be code")
		}
		if !blocks[1].IsCode {
			t.Error("block[1] should be code")
		}
		if blocks[2].IsCode {
			t.Error("block[2] should not be code")
		}
	})

	t.Run("single block no split", func(t *testing.T) {
		text := "just one block"
		blocks := splitIntoBlocks(text)
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block, got %d", len(blocks))
		}
	})

	t.Run("empty string", func(t *testing.T) {
		blocks := splitIntoBlocks("")
		if len(blocks) != 1 {
			t.Fatalf("expected 1 block for empty input, got %d", len(blocks))
		}
	})
}

func TestScoreBlocks(t *testing.T) {
	blocks := []Block{
		{Content: "the login handler uses jwt tokens for authentication", IsCode: false},
		{Content: "the database connection pool is configured with max 10 connections", IsCode: false},
		{Content: "user authentication fails when the jwt token is expired", IsCode: false},
	}

	scored := scoreBlocks("fix jwt authentication token", blocks)

	if len(scored) != 3 {
		t.Fatalf("expected 3 scored blocks, got %d", len(scored))
	}

	_, dbScore, jwtScore := scored[0].score, scored[1].score, scored[2].score

	if jwtScore <= dbScore {
		t.Errorf("block about JWT auth (%f) should score higher than DB block (%f)", jwtScore, dbScore)
	}
	if jwtScore <= 0 {
		t.Errorf("JWT block should have positive score, got %f", jwtScore)
	}
}

func TestScoreBlocksEmptyQuery(t *testing.T) {
	blocks := []Block{
		{Content: "some text here", IsCode: false},
		{Content: "other text there", IsCode: false},
	}
	scored := scoreBlocks("", blocks)
	for _, s := range scored {
		if s.score != 0 {
			t.Errorf("empty query should produce zero scores, got %f", s.score)
		}
	}
}

func TestTruncateTFIDF(t *testing.T) {
	cfg := config.GovernanceConfig{
		KeepFirstPercent: 15.0,
		KeepLastPercent:  25.0,
	}

	t.Run("within limit returns unchanged", func(t *testing.T) {
		text := "short text"
		result := TruncateTFIDF(text, 100, "query", cfg)
		if result != text {
			t.Errorf("expected unchanged, got %q", result)
		}
	})

	t.Run("single block falls back to middle-out", func(t *testing.T) {
		text := strings.Repeat("word ", 5000)
		maxSize := 1000
		result := TruncateTFIDF(text, maxSize, "query", cfg)
		if utf8.RuneCountInString(result) > maxSize {
			t.Errorf("result %d runes exceeds maxSize %d", utf8.RuneCountInString(result), maxSize)
		}
		if !strings.Contains(result, "[NENYA: MASSIVE PAYLOAD TRUNCATED]") {
			t.Error("single block should fall back to middle-out truncation")
		}
	})

	t.Run("relevant blocks kept over irrelevant", func(t *testing.T) {
		query := "fix authentication jwt token"
		irrelevant := strings.Repeat("The weather is nice and the sun is shining brightly. ", 100)
		relevant := "Fix the authentication handler to validate JWT tokens properly before accessing protected routes."
		moreIrrelevant := strings.Repeat("Random unrelated content about cooking and recipes. ", 100)

		text := irrelevant + "\n\n" + relevant + "\n\n" + moreIrrelevant
		maxSize := utf8.RuneCountInString(text) / 2

		result := TruncateTFIDF(text, maxSize, query, cfg)

		if !strings.Contains(result, "authentication") || !strings.Contains(result, "JWT") {
			t.Errorf("result should contain relevant content:\n%s", result[:min(500, len(result))])
		}
		if utf8.RuneCountInString(result) > maxSize+50 {
			t.Errorf("result %d runes should not significantly exceed maxSize %d", utf8.RuneCountInString(result), maxSize)
		}
	})

	t.Run("result within maxSize", func(t *testing.T) {
		blocks := make([]string, 20)
		for i := range blocks {
			blocks[i] = strings.Repeat("block content paragraph. ", 50)
		}
		text := strings.Join(blocks, "\n\n")
		maxSize := utf8.RuneCountInString(text) / 2

		result := TruncateTFIDF(text, maxSize, "content", cfg)
		resultRunes := utf8.RuneCountInString(result)
		if resultRunes > maxSize {
			t.Errorf("result %d runes exceeds maxSize %d", resultRunes, maxSize)
		}
	})
}

func TestTruncateTFIDFCodeAware(t *testing.T) {
	cfg := config.GovernanceConfig{
		KeepFirstPercent: 15.0,
		KeepLastPercent:  25.0,
	}

	text := strings.Repeat("irrelevant paragraph. ", 100) + "\n\n" +
		"```go\n" + strings.Repeat("func irrelevant() {}\n", 50) + "```\n\n" +
		"Fix the login JWT handler.\n\n" +
		strings.Repeat("more irrelevant text. ", 100)

	maxSize := utf8.RuneCountInString(text) / 2
	result := TruncateTFIDFCodeAware(text, maxSize, "fix login jwt handler", cfg)

	if utf8.RuneCountInString(result) > maxSize {
		t.Errorf("result %d runes exceeds maxSize %d", utf8.RuneCountInString(result), maxSize)
	}

	if strings.Contains(result, "[NENYA: TF-IDF PRUNED]") {
		if idx := strings.Index(result, "[NENYA: TF-IDF PRUNED]"); idx > 0 {
			before := result[:idx]
			if !strings.HasSuffix(before, "\n\n") {
				t.Log("code-aware should snap to paragraph boundaries")
			}
		}
	}
}

func TestTruncateTFIDFHistory(t *testing.T) {
	cfg := config.GovernanceConfig{
		KeepFirstPercent: 25.0,
		KeepLastPercent:  30.0,
	}
	history := strings.Repeat("old conversation about weather and cooking. ", 200) +
		"\n\nFix the authentication bug in login.go\n\n" +
		strings.Repeat("more old chat about gardening. ", 200)

	result := TruncateTFIDFHistory(history, 4000, "fix authentication bug login.go", cfg)

	if utf8.RuneCountInString(result) > 4100 {
		t.Errorf("history result %d runes should be near 4000 limit", utf8.RuneCountInString(result))
	}

	if !strings.Contains(result, "authentication") || !strings.Contains(result, "login") {
		t.Error("relevant history content should be preserved")
	}
}

func TestExtractPriorUserMessages(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "you are helpful"},
		map[string]interface{}{"role": "user", "content": "what is the auth issue?"},
		map[string]interface{}{"role": "assistant", "content": "let me check"},
		map[string]interface{}{"role": "user", "content": "check the login handler"},
		map[string]interface{}{"role": "assistant", "content": "I see it"},
		map[string]interface{}{"role": "user", "content": "massive code paste..."},
	}

	prior := ExtractPriorUserMessages(messages[:len(messages)-1], 5)
	if prior == "" {
		t.Fatal("expected non-empty prior messages")
	}
	if !strings.Contains(prior, "auth issue") {
		t.Error("should contain first user message")
	}
	if !strings.Contains(prior, "login handler") {
		t.Error("should contain second user message")
	}
	if strings.Contains(prior, "massive code paste") {
		t.Error("should not include the last message (excluded from slice)")
	}
}

func TestExtractSelfQuery(t *testing.T) {
	long := strings.Repeat("word ", 1000)
	result := ExtractSelfQuery(long, 100)
	if utf8.RuneCountInString(result) > 100 {
		t.Errorf("should be capped at 100 runes, got %d", utf8.RuneCountInString(result))
	}

	short := "short query"
	result = ExtractSelfQuery(short, 100)
	if result != short {
		t.Errorf("short text should be unchanged, got %q", result)
	}
}

func TestSortScoredDesc(t *testing.T) {
	blocks := []scoredBlock{
		{score: 3.0, index: 0},
		{score: 1.0, index: 1},
		{score: 2.0, index: 2},
	}
	sortScoredDesc(blocks)
	if blocks[0].score != 3.0 || blocks[1].score != 2.0 || blocks[2].score != 1.0 {
		t.Errorf("not sorted desc: %+v", blocks)
	}
}
