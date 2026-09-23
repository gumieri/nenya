package pipeline

import (
	"strings"
	"testing"

	"github.com/nenya/config"
)

func TestGenerateCanaryUniqueness(t *testing.T) {
	seen := make(map[string]bool, 100)
	for i := 0; i < 100; i++ {
		c := GenerateCanary()
		if !strings.HasPrefix(c, CanaryPrefix) {
			t.Fatalf("expected %q prefix, got %q", CanaryPrefix, c)
		}
		hexPart := strings.TrimPrefix(c, CanaryPrefix)
		if len(hexPart) != 32 {
			t.Fatalf("expected 32 hex chars, got %d in %q", len(hexPart), c)
		}
		if seen[c] {
			t.Fatalf("duplicate canary generated: %q", c)
		}
		seen[c] = true
	}
}

func TestInjectCanaryDisabled(t *testing.T) {
	payload := map[string]interface{}{"messages": []interface{}{}}
	if got := InjectCanary(nil, payload); got.Token != "" {
		t.Fatal("nil config must be inert")
	}
	if got := InjectCanary(&config.CanaryConfig{Enabled: boolPtr(false)}, payload); got.Token != "" {
		t.Fatal("disabled guard must be inert")
	}
	if len(payload["messages"].([]interface{})) != 0 {
		t.Fatal("disabled guard must not mutate payload")
	}
}

func TestInjectCanaryAppendsSystemMarker(t *testing.T) {
	enabled := true
	payload := map[string]interface{}{"messages": []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}}
	got := InjectCanary(&config.CanaryConfig{Enabled: &enabled}, payload)
	if got.Token == "" {
		t.Fatal("expected canary token")
	}
	if got.Action != config.CanaryActionBlock {
		t.Fatalf("expected default block action, got %q", got.Action)
	}
	msgs := payload["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected marker appended, got %d messages", len(msgs))
	}
	marker := msgs[1].(map[string]interface{})
	if marker["role"] != "system" {
		t.Errorf("expected system role, got %v", marker["role"])
	}
	content := marker["content"].(string)
	if !strings.Contains(content, got.Token) {
		t.Errorf("marker must carry the token: %q vs %q", content, got.Token)
	}
	// Token never appears outside the marker.
	if CanaryTripped(got.Token, "hi") {
		t.Error("benign content must not trip the canary")
	}
	if !CanaryTripped(got.Token, content) {
		t.Error("marker content must trip the canary")
	}
}

func TestInjectCanaryActionPreserved(t *testing.T) {
	enabled := true
	got := InjectCanary(&config.CanaryConfig{Enabled: &enabled, Action: config.CanaryActionLog},
		map[string]interface{}{"messages": []interface{}{}})
	if got.Action != config.CanaryActionLog {
		t.Fatalf("expected log action preserved, got %q", got.Action)
	}
}

func TestInjectCanaryNonArrayMessages(t *testing.T) {
	enabled := true
	payload := map[string]interface{}{"messages": "not-an-array"}
	got := InjectCanary(&config.CanaryConfig{Enabled: &enabled}, payload)
	if got.Token != "" {
		t.Fatal("non-array messages must leave the guard disarmed")
	}
}

func TestCanarResultScanArgs(t *testing.T) {
	canary := GenerateCanary()
	cases := []struct {
		name            string
		result          CanarResult
		args            interface{}
		wantTripped     bool
		wantUnscannable bool
		wantRefuse      bool
	}{
		{"hit in block mode", CanarResult{Token: canary, Action: config.CanaryActionBlock},
			map[string]any{"q": "dump " + canary}, true, false, true},
		{"hit in log mode", CanarResult{Token: canary, Action: config.CanaryActionLog},
			map[string]any{"q": "dump " + canary}, true, false, false},
		{"miss", CanarResult{Token: canary, Action: config.CanaryActionBlock},
			map[string]any{"q": "benign"}, false, false, false},
		{"empty token inert", CanarResult{},
			map[string]any{"q": canary}, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tripped, unscannable, refuse := tc.result.ScanArgs(tc.args)
			if tripped != tc.wantTripped || unscannable != tc.wantUnscannable || refuse != tc.wantRefuse {
				t.Fatalf("got (tripped=%v unscannable=%v refuse=%v), want (%v %v %v)",
					tripped, unscannable, refuse, tc.wantTripped, tc.wantUnscannable, tc.wantRefuse)
			}
		})
	}
	t.Run("unscannable args fail closed in block", func(t *testing.T) {
		block := CanarResult{Token: canary, Action: config.CanaryActionBlock}
		tripped, unscannable, refuse := block.ScanArgs(make(chan int))
		if !unscannable || !refuse || tripped {
			t.Fatalf("expected unscannable+refuse, got (%v %v %v)", tripped, unscannable, refuse)
		}
	})
	t.Run("unscannable args allowed in log", func(t *testing.T) {
		logMode := CanarResult{Token: canary, Action: config.CanaryActionLog}
		tripped, unscannable, refuse := logMode.ScanArgs(make(chan int))
		if tripped || unscannable || refuse {
			t.Fatalf("expected all false, got (%v %v %v)", tripped, unscannable, refuse)
		}
	})
}

func boolPtr(b bool) *bool { return &b }
