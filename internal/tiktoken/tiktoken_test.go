package tiktoken

import (
	"strings"
	"testing"
)

func TestCountTokens_Empty(t *testing.T) {
	if got := CountTokens(""); got != 0 {
		t.Errorf("CountTokens(\"\") = %d, want 0", got)
	}
}

func TestCountTokens_SingleChar(t *testing.T) {
	if got := CountTokens("a"); got != 1 {
		t.Errorf("CountTokens(\"a\") = %d, want 1", got)
	}
}

func TestCountTokens_HelloWorld(t *testing.T) {
	if got := CountTokens("hello world"); got != 2 {
		t.Errorf("CountTokens(\"hello world\") = %d, want 2", got)
	}
}

func TestCountTokens_Hello(t *testing.T) {
	if got := CountTokens("hello"); got != 1 {
		t.Errorf("CountTokens(\"hello\") = %d, want 1", got)
	}
}

func TestCountTokens_LeadingSpace(t *testing.T) {
	if got := CountTokens(" hello world"); got != 2 {
		t.Errorf("CountTokens(\" hello world\") = %d, want 2", got)
	}
}

func TestCountTokens_MultipleSpaces(t *testing.T) {
	if got := CountTokens("hello   world"); got != 3 {
		t.Errorf("CountTokens(\"hello   world\") = %d, want 3", got)
	}
}

func TestCountTokens_Newline(t *testing.T) {
	if got := CountTokens("hello\nworld"); got != 3 {
		t.Errorf("CountTokens(\"hello\\nworld\") = %d, want 3", got)
	}
}

func TestCountTokens_TrailingWhitespace(t *testing.T) {
	if got := CountTokens("hello   "); got != 2 {
		t.Errorf("CountTokens(\"hello   \") = %d, want 2", got)
	}
}

func TestCountTokens_Contraction(t *testing.T) {
	if got := CountTokens("it's"); got != 2 {
		t.Errorf("CountTokens(\"it's\") = %d, want 2", got)
	}
}

func TestCountTokens_NotAContraction(t *testing.T) {
	if got := CountTokens("'rer"); got != 2 {
		t.Errorf("CountTokens(\"'rer\") = %d, want 2", got)
	}
}

func TestCountTokens_Numbers(t *testing.T) {
	if got := CountTokens("123456"); got != 2 {
		t.Errorf("CountTokens(\"123456\") = %d, want 2", got)
	}
}

func TestCountTokens_Punctuation(t *testing.T) {
	if got := CountTokens("hello, world!"); got != 4 {
		t.Errorf("CountTokens(\"hello, world!\") = %d, want 4", got)
	}
}

func TestCountTokens_Emoji(t *testing.T) {
	if got := CountTokens("👍"); got != 3 {
		t.Errorf("CountTokens(\"👍\") = %d, want 3", got)
	}
}

func TestCountTokens_LongNumber(t *testing.T) {
	if got := CountTokens("0000000000"); got != 4 {
		t.Errorf("CountTokens(\"0000000000\") = %d, want 4", got)
	}
}

func TestCountTokens_TodayNewlineSpaceNewline(t *testing.T) {
	if got := CountTokens("today\n \n"); got != 2 {
		t.Errorf("CountTokens(\"today\\n \\n\") = %d, want 2", got)
	}
}

func TestCountTokens_TodayNewlineSpaceSpaceNewline(t *testing.T) {
	if got := CountTokens("today\n  \n"); got != 2 {
		t.Errorf("CountTokens(\"today\\n  \\n\") = %d, want 2", got)
	}
}

func TestCountTokens_OnlyWhitespace(t *testing.T) {
	if got := CountTokens("   \n\t  "); got != 2 {
		t.Errorf("CountTokens(\"   \\n\\t  \") = %d, want 2", got)
	}
}

func TestCountTokens_OnlyNewlines(t *testing.T) {
	if got := CountTokens("\n\n\n"); got != 1 {
		t.Errorf("CountTokens(\"\\n\\n\\n\") = %d, want 1", got)
	}
}

func TestCountTokens_MultiLine(t *testing.T) {
	if got := CountTokens("line1\nline2\nline3"); got != 8 {
		t.Errorf("CountTokens(\"line1\\nline2\\nline3\") = %d, want 8", got)
	}
}

func TestCountTokens_UnicodeCJK(t *testing.T) {
	got := CountTokens("こんにちは世界")
	if got <= 0 {
		t.Errorf("CountTokens(\"こんにちは世界\") = %d, want >0", got)
	}
}

func TestCountTokens_MixedContent(t *testing.T) {
	got := CountTokens("Hello, world! 123 testing.")
	if got <= 0 {
		t.Errorf("CountTokens(\"Hello, world! 123 testing.\") = %d, want >0", got)
	}
}

func TestPreTokenize_BasicWords(t *testing.T) {
	pieces := preTokenize("hello world")
	if len(pieces) != 2 {
		t.Errorf("preTokenize(\"hello world\") = %v, want 2 pieces", pieces)
	}
	if pieces[0] != "hello" {
		t.Errorf("preTokenize(\"hello world\")[0] = %q, want \"hello\"", pieces[0])
	}
	if pieces[1] != " world" {
		t.Errorf("preTokenize(\"hello world\")[1] = %q, want \" world\"", pieces[1])
	}
}

func TestPreTokenize_LeadingSpace(t *testing.T) {
	pieces := preTokenize(" hello world")
	if len(pieces) != 2 {
		t.Errorf("preTokenize(\" hello world\") = %v, want 2 pieces", pieces)
	}
	if pieces[0] != " hello" {
		t.Errorf("preTokenize(\" hello world\")[0] = %q, want \" hello\"", pieces[0])
	}
	if pieces[1] != " world" {
		t.Errorf("preTokenize(\" hello world\")[1] = %q, want \" world\"", pieces[1])
	}
}

func TestPreTokenize_MultipleSpaces(t *testing.T) {
	pieces := preTokenize("hello   world")
	if len(pieces) != 3 {
		t.Errorf("preTokenize(\"hello   world\") = %v, want 3 pieces", pieces)
	}
	if pieces[0] != "hello" {
		t.Errorf("preTokenize(\"hello   world\")[0] = %q, want \"hello\"", pieces[0])
	}
	if pieces[1] != "  " {
		t.Errorf("preTokenize(\"hello   world\")[1] = %q, want \"  \"", pieces[1])
	}
	if pieces[2] != " world" {
		t.Errorf("preTokenize(\"hello   world\")[2] = %q, want \" world\"", pieces[2])
	}
}

func TestPreTokenize_Newline(t *testing.T) {
	pieces := preTokenize("hello\nworld")
	if len(pieces) != 3 {
		t.Errorf("preTokenize(\"hello\\nworld\") = %v, want 3 pieces", pieces)
	}
	if pieces[0] != "hello" {
		t.Errorf("preTokenize(\"hello\\nworld\")[0] = %q, want \"hello\"", pieces[0])
	}
	if pieces[1] != "\n" {
		t.Errorf("preTokenize(\"hello\\nworld\")[1] = %q, want \"\\n\"", pieces[1])
	}
	if pieces[2] != "world" {
		t.Errorf("preTokenize(\"hello\\nworld\")[2] = %q, want \"world\"", pieces[2])
	}
}

func TestPreTokenize_TrailingWhitespace(t *testing.T) {
	pieces := preTokenize("hello   ")
	if len(pieces) != 2 {
		t.Errorf("preTokenize(\"hello   \") = %v, want 2 pieces", pieces)
	}
	if pieces[0] != "hello" {
		t.Errorf("preTokenize(\"hello   \")[0] = %q, want \"hello\"", pieces[0])
	}
	if pieces[1] != "   " {
		t.Errorf("preTokenize(\"hello   \")[1] = %q, want \"   \"", pieces[1])
	}
}

func TestPreTokenize_Contraction(t *testing.T) {
	pieces := preTokenize("it's")
	if len(pieces) != 2 {
		t.Errorf("preTokenize(\"it's\") = %v, want 2 pieces", pieces)
	}
	if pieces[0] != "it" {
		t.Errorf("preTokenize(\"it's\")[0] = %q, want \"it\"", pieces[0])
	}
	if pieces[1] != "'s" {
		t.Errorf("preTokenize(\"it's\")[1] = %q, want \"'s\"", pieces[1])
	}
}

func TestPreTokenize_NotAContraction(t *testing.T) {
	pieces := preTokenize("'rer")
	if len(pieces) != 2 {
		t.Errorf("preTokenize(\"'rer\") = %v, want 2 pieces", pieces)
	}
	if pieces[0] != "'re" {
		t.Errorf("preTokenize(\"'rer\")[0] = %q, want \"'re\"", pieces[0])
	}
	if pieces[1] != "r" {
		t.Errorf("preTokenize(\"'rer\")[1] = %q, want \"r\"", pieces[1])
	}
}

func TestPreTokenize_Numbers(t *testing.T) {
	pieces := preTokenize("123456")
	if len(pieces) != 2 {
		t.Errorf("preTokenize(\"123456\") = %v, want 2 pieces", pieces)
	}
	if pieces[0] != "123" {
		t.Errorf("preTokenize(\"123456\")[0] = %q, want \"123\"", pieces[0])
	}
	if pieces[1] != "456" {
		t.Errorf("preTokenize(\"123456\")[1] = %q, want \"456\"", pieces[1])
	}
}

func TestPreTokenize_Punctuation(t *testing.T) {
	pieces := preTokenize("hello, world!")
	if len(pieces) != 4 {
		t.Errorf("preTokenize(\"hello, world!\") = %v, want 4 pieces", pieces)
	}
	if pieces[0] != "hello" {
		t.Errorf("preTokenize(\"hello, world!\")[0] = %q, want \"hello\"", pieces[0])
	}
	if pieces[1] != "," {
		t.Errorf("preTokenize(\"hello, world!\")[1] = %q, want \",\"", pieces[1])
	}
	if pieces[2] != " world" {
		t.Errorf("preTokenize(\"hello, world!\")[2] = %q, want \" world\"", pieces[2])
	}
	if pieces[3] != "!" {
		t.Errorf("preTokenize(\"hello, world!\")[3] = %q, want \"!\"", pieces[3])
	}
}

func TestPreTokenize_UnicodeCJK(t *testing.T) {
	pieces := preTokenize("こんにちは世界")
	if len(pieces) != 1 {
		t.Errorf("preTokenize(\"こんにちは世界\") = %v, want 1 piece", pieces)
	}
	if pieces[0] != "こんにちは世界" {
		t.Errorf("preTokenize(\"こんにちは世界\")[0] = %q, want \"こんにちは世界\"", pieces[0])
	}
}

func TestPreTokenize_Emoji(t *testing.T) {
	pieces := preTokenize("👍")
	if len(pieces) != 1 {
		t.Errorf("preTokenize(\"👍\") = %v, want 1 piece", pieces)
	}
	if pieces[0] != "👍" {
		t.Errorf("preTokenize(\"👍\")[0] = %q, want \"👍\"", pieces[0])
	}
}

func TestBPECount_SingleByte(t *testing.T) {
	if got := bpeCount([]byte("a")); got != 1 {
		t.Errorf("bpeCount([]byte(\"a\")) = %d, want 1", got)
	}
}

func TestBPECount_Empty(t *testing.T) {
	if got := bpeCount([]byte("")); got != 0 {
		t.Errorf("bpeCount([]byte(\"\")) = %d, want 0", got)
	}
}

func TestBPECount_KnownWord(t *testing.T) {
	if got := bpeCount([]byte("hello")); got != 1 {
		t.Errorf("bpeCount([]byte(\"hello\")) = %d, want 1", got)
	}
}

func TestBPECount_KnownPhrase(t *testing.T) {
	if got := bpeCount([]byte("hello world")); got != 2 {
		t.Errorf("bpeCount([]byte(\"hello world\")) = %d, want 2", got)
	}
}

func TestBPECount_NotAContraction(t *testing.T) {
	if got := bpeCount([]byte("'rer")); got != 2 {
		t.Errorf("bpeCount([]byte(\"'rer\")) = %d, want 2", got)
	}
}

func TestBPECount_TodayNewlineSpaceNewline(t *testing.T) {
	if got := bpeCount([]byte("today\n \n")); got != 2 {
		t.Errorf("bpeCount([]byte(\"today\\n \\n\")) = %d, want 2", got)
	}
}

func TestBPECount_TodayNewlineSpaceSpaceNewline(t *testing.T) {
	if got := bpeCount([]byte("today\n  \n")); got != 2 {
		t.Errorf("bpeCount([]byte(\"today\\n  \\n\")) = %d, want 2", got)
	}
}

func TestBPECount_Emoji(t *testing.T) {
	if got := bpeCount([]byte("👍")); got != 3 {
		t.Errorf("bpeCount([]byte(\"👍\")) = %d, want 3", got)
	}
}

func TestBPECount_LongNumber(t *testing.T) {
	if got := bpeCount([]byte("0000000000")); got != 5 {
		t.Errorf("bpeCount([]byte(\"0000000000\")) = %d, want 5", got)
	}
}

func TestInitIntegrity(t *testing.T) {
	if got := CountTokens("hello world"); got != 2 {
		t.Errorf("Init integrity check: CountTokens(\"hello world\") = %d, want 2", got)
	}
}

func BenchmarkCountTokens_Short(b *testing.B) {
	for i := 0; i < b.N; i++ {
		CountTokens("hello world")
	}
}

func BenchmarkCountTokens_Medium(b *testing.B) {
	text := "Hello, world! This is a test of the token counting system. It should be reasonably fast for typical message content."
	for i := 0; i < b.N; i++ {
		CountTokens(text)
	}
}

func BenchmarkCountTokens_Long(b *testing.B) {
	text := strings.Repeat("Hello, world! This is a test of the token counting system. It should be reasonably fast for typical message content. ", 100)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		CountTokens(text)
	}
}
