package tiktoken

import (
	"strings"
	"testing"
)

// classCorpus are the content classes the fallback is calibrated against.
// tolerance is the allowed relative error versus the real encoder; the
// pathological whitespace case is documented wider (a 200-space run collapses
// to a couple of tokens in cl100k, which the per-run model cannot express).
var classCorpus = []struct {
	name      string
	text      string
	tolerance float64
}{
	{"prose", "The quick brown fox jumps over the lazy dog and then goes home for dinner.", 0.35},
	{"code", "func (p *Proxy) handle(w http.ResponseWriter, r *http.Request) error {\n\tif r == nil { return nil }\n\treturn p.do(map[string]any{\"a\": 1, \"b\": \"two\"})\n}", 0.35},
	{"json", `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"temperature":0.7,"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]}`, 0.35},
	{"indented code", strings.Repeat("    if x {\n        y()\n    }\n", 20), 0.35},
	{"japanese mixed", "こんにちは世界、これはテストです。日本語のテキストはトークン数が多くなります。", 0.5},
	{"chinese sentence", "人工智能正在改变软件工程的工作方式，代码审查和测试自动化越来越普遍。", 0.4},
	{"korean sentence", "인공지능은 소프트웨어 엔지니어링의 작업 방식을 바꾸고 있습니다. 코드 리뷰와 테스트 자동화가 점점 보편화되고 있습니다.", 0.4},
	{"kana only", strings.Repeat("ひらがな", 20), 0.35},
	{"letters-only long run", strings.Repeat("qwertyuiopasdfghjklzxcvbnm", 2), 0.4},
	{"long camelCase identifier", "handleThousandsOfRequestsWithoutAllocatingAnythingAtAll", 0.4},
	{"dashed-less uuid", "3f2504e04f8911d39a0c0305e82c3301", 0.4},
	{"digits-only run", strings.Repeat("123456789012", 4), 0.4},
	{"hex uuid", "550e8400-e29b-41d4-a716-446655440000", 0.4},
	{"sha1 hex", "da39a3ee5e6b4b0d3255bfef95601890afd80709", 0.35},
	{"base64 fragment", "aGVsbG8gd29ybGQgdGhpcyBpcyBzb21lIGRhdGE=", 0.4},
	{"cyrillic", "Привет мир, это тестовый текст на русском языке для оценки токенов.", 0.6},
	{"emoji", "Hello 👋🏽 world 🚀🎉👍🏼 test ⭐️", 0.5},
	{"base64 payload", "aGVsbG8gd29ybGQgdGhpcyBpcyBhIGxvbmdlciBiYXNlNjQgc3RyaW5nIGZvciB0ZXN0aW5nIGRlbnNpdHk=", 0.35},
	{"long whitespace run", "a" + strings.Repeat(" ", 200) + "b", 7.0},
}

// TestEstimateByClass_AgainstBPE pins the fallback's accuracy against the real
// encoder (NENYA-157): the class-weighted estimate must stay within its
// per-class tolerance band, and must never UNDER-estimate by more than the
// band — over-estimating costs a trim, under-estimating risks a provider-side
// context error. The fallback only runs when the BPE vocabulary fails to load.
func TestEstimateByClass_AgainstBPE(t *testing.T) {
	for _, tc := range classCorpus {
		t.Run(tc.name, func(t *testing.T) {
			exact, err := CountTokens(tc.text)
			if err != nil {
				t.Fatalf("the embedded vocabulary must load: %v", err)
			}
			if exact == 0 {
				t.Fatal("fixture produced no tokens")
			}
			got := EstimateByClass(tc.text)
			if got == 0 {
				t.Fatal("the fallback must never return zero for non-empty text")
			}
			relErr := float64(got-exact) / float64(exact)
			if relErr > tc.tolerance {
				t.Errorf("EstimateByClass = %d, exact = %d (+%.0f%% > %.0f%% tolerance)", got, exact, relErr*100, tc.tolerance*100)
			}
			if relErr < -tc.tolerance {
				t.Errorf("EstimateByClass = %d, exact = %d (%.0f%% UNDER-estimate, tolerance %.0f%%)", got, exact, -relErr*100, tc.tolerance*100)
			}
		})
	}
}

func TestEstimateByClass_Edges(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		{"single newline", "\n", 1},
		{"single letter", "a", 1},
		{"single space", " ", 1},
		{"single ideograph costs one", "漢", 1},
		{"single kana costs one", "あ", 1},
		{"single emoji", "🚀", emojiTokens},
		{"skin tone adds a modifier token", "🚀🏽", emojiTokens + emojiModifierTokens},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateByClass(tt.text); got != tt.want {
				t.Fatalf("EstimateByClass(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

// TestEstimateByClass_Monotonic pins the ordering property the budget code
// relies on: more text never estimates fewer tokens.
func TestEstimateByClass_Monotonic(t *testing.T) {
	base := "the quick brown fox\n"
	prev := 0
	var sb strings.Builder
	for i := 0; i < 40; i++ {
		sb.WriteString(base)
		got := EstimateByClass(sb.String())
		if got < prev {
			t.Fatalf("estimate shrank: %d -> %d after adding a line", prev, got)
		}
		prev = got
	}
}

func TestIsWideRune_NotForASCII(t *testing.T) {
	if isWideRune('a') || isWideRune('~') || isWideRune('é') {
		t.Fatal("ASCII and accented Latin are not wide runes")
	}
	if !isWideRune('漢') || !isWideRune('あ') || !isWideRune('한') {
		t.Fatal("CJK, kana, and hangul are wide runes")
	}
}
