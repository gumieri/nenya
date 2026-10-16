package discovery

import (
	"testing"

	"github.com/nenya/internal/util"
)

// TestOpaqueModelIDClassification pins the NENYA-146 classifier: ARN-style,
// slashed, and dotted IDs are opaque; plain IDs are not.
func TestOpaqueModelIDClassification(t *testing.T) {
	opaque := []string{
		"arn:aws:bedrock:us-east-1::foundation-model/deepseek-r1",
		"arn:aws:bedrock::123456789012:model/claude-sonnet-4",
		"bedrock/deepseek-r1",
		"openrouter/x-ai/grok-4",
		"gemini-3.5-flash",
		"models/gemini-2.5-pro",
	}
	for _, id := range opaque {
		if !util.OpaqueModelID(id) {
			t.Errorf("OpaqueModelID(%q) = false, want true", id)
		}
	}
	plain := []string{"gpt-4", "claude-sonnet-4", "GLM-4-7", "deepseek-chat"}
	for _, id := range plain {
		if util.OpaqueModelID(id) {
			t.Errorf("OpaqueModelID(%q) = true, want false", id)
		}
	}
}

// TestCatalogOpaqueIDsRoundTrip pins the NENYA-146 catalog half: opaque IDs
// are catalog keys verbatim — no case folding, no path-style merging — and
// two providers serving the same opaque ID coexist as separate entries.
func TestCatalogOpaqueIDsRoundTrip(t *testing.T) {
	catalog := NewModelCatalog()
	ids := []string{
		"arn:aws:bedrock:us-east-1::foundation-model/deepseek-r1",
		"bedrock/DeepSeek-R1",
		"gateway/GroK-4",
		" models/ünïcode-mödel ",
	}
	for _, id := range ids {
		catalog.Add(DiscoveredModel{ID: id, Provider: "agg-a"})
	}
	// Same ID served by a second provider: both entries survive under the
	// exact key.
	catalog.Add(DiscoveredModel{ID: ids[0], Provider: "agg-b"})

	for _, id := range ids {
		got, ok := catalog.Lookup(id)
		if !ok || got.ID != id {
			t.Fatalf("Lookup(%q) = (%#v, %v); opaque IDs must round-trip verbatim", id, got, ok)
		}
	}
	if all := catalog.LookupAll(ids[0]); len(all) != 2 {
		t.Fatalf("both providers' entries must merge under the exact opaque key, got %d", len(all))
	}
	// A case variant is a DIFFERENT key: no silent merging.
	if _, ok := catalog.Lookup("BEDROCK/deepseek-r1"); ok {
		t.Fatal("a case-variant opaque ID must not resolve to another entry")
	}
	// Trimming is the caller's business: an untrimmed ID is its own key.
	if _, ok := catalog.Lookup(" models/ünïcode-mödel "); !ok {
		t.Fatal("the exact (untrimmed) ID must resolve")
	}
	if _, ok := catalog.Lookup("models/ünïcode-mödel"); ok {
		t.Fatal("a trimmed variant must not merge with the untrimmed entry")
	}
}
