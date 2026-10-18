package adapter

import (
	"reflect"
	"strings"
	"testing"
)

func TestDropReportNilSafety(t *testing.T) {
	var nilReport *DropReport
	nilReport.Add("thinking_signature") // must not panic
	if !nilReport.Empty() {
		t.Fatal("nil report should be empty")
	}
	if got := nilReport.Kinds(); got != nil {
		t.Fatalf("nil report kinds = %v, want nil", got)
	}
}

func TestDropReportKindsSortedAndDeduped(t *testing.T) {
	report := NewDropReport()
	report.Add("document")
	report.Add("thinking_signature")
	report.Add("document")
	report.Add("redacted_thinking")
	if report.Empty() {
		t.Fatal("report should not be empty after adds")
	}
	want := []string{"document", "redacted_thinking", "thinking_signature"}
	if got := report.Kinds(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Kinds() = %v, want %v", got, want)
	}
}

func TestFlattenContentArraysSurfacesUnsendableParts(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "hello"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,AAAA"}},
				map[string]interface{}{"type": "document", "ciphertext": "blob"},
			},
		},
	}
	if !flattenContentArrays(msgs) {
		t.Fatal("flatten should report a change")
	}
	msg := msgs[0].(map[string]interface{})
	content, _ := msg["content"].(string)
	if content == "hello" || content == "" {
		t.Fatalf("unsendable parts lost silently: content = %q", content)
	}
	for _, marker := range []string{"[unsendable content: image_url]", "[unsendable content: document]"} {
		if !strings.Contains(content, marker) {
			t.Fatalf("content %q missing marker %q", content, marker)
		}
	}
}

func TestDropReportKindsSanitizedAndBounded(t *testing.T) {
	report := NewDropReport()
	report.Add("doc\r\nX-Evil: 1") // CRLF neutralized
	report.Add("a,b")              // comma would corrupt the split
	report.Add("")
	for i := 0; i < 12; i++ {
		report.Add("kind" + strings.Repeat("x", i)) // exceeds the 8-kind cap
	}
	if report.Empty() {
		t.Fatal("report should not be empty")
	}
	kinds := report.Kinds()
	// 8 distinct kinds retained; the overflow additions collapsed to "other".
	if len(kinds) != 9 || kinds[len(kinds)-1] != "other" {
		t.Fatalf("kinds = %v, want 8 retained kinds + overflow other", kinds)
	}
	for _, k := range kinds {
		if strings.ContainsAny(k, ",\r\n") {
			t.Fatalf("kind %q is not header-safe", k)
		}
	}
	long := NewDropReport()
	long.Add(strings.Repeat("v", 200))
	if got := long.Kinds()[0]; len(got) > 48 {
		t.Fatalf("kind length %d exceeds cap", len(got))
	}
}
