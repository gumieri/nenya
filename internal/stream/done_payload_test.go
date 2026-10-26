package stream

import "testing"

// TestIsDonePayload pins the single-source sentinel check (AUD-65): the
// exact payload, whitespace-tolerant variants, and non-sentinels.
func TestIsDonePayload(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"exact", "[DONE]", true},
		{"padded", "  [DONE]\n", true},
		{"prefixed frame is not the payload", "data: [DONE]", false},
		{"suffix glued", "[DONE]x", false},
		{"lowercase", "[done]", false},
		{"empty", "", false},
		{"json object", `{"id":1}`, false},
	}
	for _, tc := range cases {
		if got := IsDonePayload([]byte(tc.in)); got != tc.want {
			t.Errorf("%s: IsDonePayload(%q) = %v, want %v", tc.name, tc.in, got, tc.want)
		}
	}
	if canonicalDoneFrame != "data: "+DonePayload {
		t.Fatalf("canonicalDoneFrame = %q, want the derived frame", canonicalDoneFrame)
	}
}
