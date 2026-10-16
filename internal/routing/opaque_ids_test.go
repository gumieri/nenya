package routing

import (
	"strings"
	"testing"

	"github.com/nenya/config"
)

// TestOpaqueModelIDsSkipCaseNormalizedMapping pins the NENYA-146 routing
// half: plain IDs keep the case-insensitive built-in spec mapping
// convenience, while opaque IDs (ARNs, slashed, dotted) are matched exactly
// and never rewritten by normalization. The expectations intentionally pin
// the SHIPPED providers.GeminiModelMap spellings (a map edit failing this
// test is a signal to re-verify opacity, not noise).
func TestOpaqueModelIDsSkipCaseNormalizedMapping(t *testing.T) {
	deps := TransformDeps{Providers: map[string]*config.Provider{
		"gemini": {Name: "gemini"},
	}}

	mapCase := func(payload map[string]interface{}, model string) string {
		return resolveModelMapping(deps, payload, "gemini", model)
	}

	// Plain ID, case-variant: the convenience mapping applies.
	if got := mapCase(map[string]interface{}{}, "GEMINI-PRO"); got != "gemini-2.5-pro" {
		t.Fatalf("case-variant plain ID must map, got %q", got)
	}
	// Plain ID, exact: mapped too.
	if got := mapCase(map[string]interface{}{}, "gemini-pro"); got != "gemini-2.5-pro" {
		t.Fatalf("exact plain ID must map, got %q", got)
	}

	// Opaque IDs are never rewritten by normalization — even spellings that
	// would case-fold onto a map key's fragments.
	for _, opaque := range []string{
		"arn:aws:bedrock:us-east-1::foundation-model/deepseek-r1",
		"bedrock/GEMINI-PRO",
		"models/GEMINI-PRO",
		"gemini-3.5-flash.Custom-Suffix",
	} {
		payload := map[string]interface{}{}
		if got := mapCase(payload, opaque); got != opaque {
			t.Fatalf("opaque ID must pass through verbatim, got %q (payload model %q)", got, payload["model"])
		}
		if payload["model"] != opaque {
			t.Fatalf("payload model rewritten for an opaque ID: %q", payload["model"])
		}
	}
}

// TestOpaqueModelAliasesExact pins the NENYA-22 exact-match contract for
// opaque alias keys: an exact opaque key maps, a case variant does not.
func TestOpaqueModelAliasesExact(t *testing.T) {
	deps := TransformDeps{Providers: map[string]*config.Provider{
		"bedrock": {
			Name: "bedrock",
			ModelAliases: map[string]string{
				"arn:aws:bedrock:us-east-1::foundation-model/deepseek-r1": "deepseek-r1-physical",
			},
		},
	}}

	payload := map[string]interface{}{}
	if got := resolveModelMapping(deps, payload, "bedrock",
		"arn:aws:bedrock:us-east-1::foundation-model/deepseek-r1"); got != "deepseek-r1-physical" {
		t.Fatalf("an exact opaque alias key must map, got %q", got)
	}
	payload = map[string]interface{}{}
	if got := resolveModelMapping(deps, payload, "bedrock",
		"ARN:aws:bedrock:us-east-1::foundation-model/deepseek-r1"); got != "ARN:aws:bedrock:us-east-1::foundation-model/deepseek-r1" {
		t.Fatalf("a case-variant alias key must NOT match, got %q", got)
	}
	if final := payload["model"].(string); strings.Contains(final, "physical") {
		t.Fatalf("the case-variant must not alias: %q", final)
	}
}
