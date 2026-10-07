package pipeline

import (
	"testing"
)

// TestInjectionBandDecision pins the two-tier band decision, including the
// DisableEscalation override used by the MCP loop's rescan (NENYA-136):
// disabling escalation collapses only the ambiguous band to the act verdict;
// the below-min pass band still passes. bandDecision never dereferences the
// escalator, so an empty placeholder suffices.
func TestInjectionBandDecision(t *testing.T) {
	i := &InjectionInterceptor{escalator: &InjectionEscalator{}, minScore: 3, maxScore: 10}
	disabled := &InterceptRequest{DisableEscalation: true}
	normal := &InterceptRequest{}

	cases := []struct {
		name       string
		req        *InterceptRequest
		detections int
		wantAct    bool
		wantSkip   bool
	}{
		{name: "below min passes", req: normal, detections: 1, wantAct: false, wantSkip: true},
		{name: "below min passes even when escalation disabled", req: disabled, detections: 1, wantAct: false, wantSkip: true},
		{name: "just below min passes (half-open band edge)", req: normal, detections: 2, wantAct: false, wantSkip: true},
		{name: "at min is ambiguous (escalates)", req: normal, detections: 3, wantAct: false, wantSkip: false},
		{name: "at min acts when escalation disabled", req: disabled, detections: 3, wantAct: true, wantSkip: false},
		{name: "ambiguous escalates", req: normal, detections: 5, wantAct: false, wantSkip: false},
		{name: "ambiguous acts when escalation disabled", req: disabled, detections: 5, wantAct: true, wantSkip: false},
		{name: "at max acts", req: normal, detections: 10, wantAct: true, wantSkip: false},
		{name: "above max acts", req: normal, detections: 50, wantAct: true, wantSkip: false},
		{name: "above max acts when escalation disabled", req: disabled, detections: 50, wantAct: true, wantSkip: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			act, skip := i.bandDecision(tc.req, tc.detections)
			if act != tc.wantAct || skip != tc.wantSkip {
				t.Fatalf("bandDecision(%s, %d) = (act=%v, skip=%v), want (act=%v, skip=%v)",
					tc.name, tc.detections, act, skip, tc.wantAct, tc.wantSkip)
			}
		})
	}

	// Escalator nil (escalation disabled in config): legacy act-on-any bands.
	legacy := &InjectionInterceptor{minScore: 3, maxScore: 10}
	if act, skip := legacy.bandDecision(normal, 1); !act || skip {
		t.Errorf("legacy nil-escalator mode must act on any detection, got act=%v skip=%v", act, skip)
	}
}
