package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// itoa formats an int64 for JSON test-body interpolation.
func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestDeriveUpstreamRateLimitCooldown pins NENYA-43: the longest applicable
// window wins — Retry-After vs Anthropic unified 5h/7d resets vs generic
// x-ratelimit-reset; past resets and missing headers contribute nothing.
func TestDeriveUpstreamRateLimitCooldown(t *testing.T) {
	now := time.Now()
	in5h := now.Add(5 * time.Hour).UTC().Format(time.RFC3339)
	in7d := now.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	past := now.Add(-time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name   string
		header map[string]string
		want   time.Duration
		tol    time.Duration
	}{
		{
			name:   "empty headers",
			header: map[string]string{},
			want:   0,
		},
		{
			name:   "retry-after only",
			header: map[string]string{"Retry-After": "30"},
			want:   30 * time.Second,
			tol:    time.Second,
		},
		{
			name:   "5h window beats retry-after",
			header: map[string]string{"Retry-After": "30", "Anthropic-Ratelimit-Unified-5h-Reset": in5h},
			want:   5 * time.Hour,
			tol:    time.Minute,
		},
		{
			name:   "7d window beats 5h window",
			header: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": in5h, "Anthropic-Ratelimit-Unified-7d-Reset": in7d},
			want:   7 * 24 * time.Hour,
			tol:    time.Minute,
		},
		{
			name:   "past reset ignored",
			header: map[string]string{"Anthropic-Ratelimit-Unified-5h-Reset": past},
			want:   0,
		},
		{
			name:   "x-ratelimit-reset duration form",
			header: map[string]string{"X-Ratelimit-Reset": "90s"},
			want:   90 * time.Second,
			tol:    time.Second,
		},
		{
			name:   "case-insensitive canonical lookup (all-lowercase wire key)",
			header: map[string]string{"anthropic-ratelimit-unified-5h-reset": in5h},
			want:   5 * time.Hour,
			tol:    time.Minute,
		},
		{
			name:   "capped at maxRateLimitWindowCooldown",
			header: map[string]string{"Retry-After": "1000000"},
			want:   maxRateLimitWindowCooldown,
			tol:    time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range tt.header {
				h.Set(k, v)
			}
			got := deriveUpstreamRateLimitCooldown(h)
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tol {
				t.Errorf("deriveUpstreamRateLimitCooldown() = %v; want ~%v", got, tt.want)
			}
		})
	}

	if got := deriveUpstreamRateLimitCooldown(nil); got != 0 {
		t.Errorf("nil header should yield 0, got %v", got)
	}
}

// TestDerive429Cooldown pins the composed 429 cooldown derivation: the 429
// status gate (5xx keep the configured cooldown, isQuota=false), detection
// decoupled from extension (a sub-config quota signal still flags isQuota),
// and header/body windows merging longest-wins.
// TestDerive429Cooldown_OverageOnlyRejectsWindows pins the NENYA-168
// integration point: an overage-only rejection ignores every upstream-declared
// window (Retry-After and the unified resets) and reports quota, so the NENYA-41
// floor and the quota error kind still apply. See TestDerive429Cooldown for the
// non-overage longest-wins behavior and its 429 status gate.
func TestDerive429Cooldown_OverageOnlyRejectsWindows(t *testing.T) {
	now := time.Now()
	in7d := now.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)

	header := http.Header{}
	header.Set("Retry-After", "3600")
	header.Set(headerUnifiedOverageStatus, "rejected")
	header.Set("Anthropic-Ratelimit-Unified-5h-Reset", in7d)
	header.Set("Anthropic-Ratelimit-Unified-7d-Reset", in7d)

	// An empty body: the only quota signal is the overage verdict itself.
	got, isQuota := derive429Cooldown(slog.Default(), nil, header, http.StatusTooManyRequests, 45*time.Second)
	if isQuota != true {
		t.Fatalf("isQuota = %v, want true (the NENYA-41 floor must apply)", isQuota)
	}
	if got != 45*time.Second {
		t.Fatalf("cooldown = %v, want the configured 45s (no upstream window)", got)
	}

	// A body quota signal must still extend the cooldown on top of the verdict.
	body := []byte(`{"error":{"message":"quota exceeded"}}`)
	got, isQuota = derive429Cooldown(slog.Default(), body, header, http.StatusTooManyRequests, 45*time.Second)
	if !isQuota || got < 45*time.Second {
		t.Fatalf("cooldown = %v isQuota = %v, want >= 45s/true", got, isQuota)
	}
}

func TestDerive429Cooldown(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	in7d := time.Now().Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-7d-Reset", in7d)

	t.Run("non-429 keeps configured cooldown and never flags quota", func(t *testing.T) {
		cd, isQuota := derive429Cooldown(logger, []byte(`{"error":"quota exceeded"}`), h, http.StatusInternalServerError, time.Minute)
		if cd != time.Minute || isQuota {
			t.Fatalf("expected (1m, false) for 500, got (%v, %v)", cd, isQuota)
		}
	})

	t.Run("sub-config quota body still flags quota without extending", func(t *testing.T) {
		// "quota exceeded" parses to 5m; configured 10m wins on duration but
		// isQuota must be true (error_kind + NENYA-41 floor depend on it).
		cd, isQuota := derive429Cooldown(logger, []byte(`{"error":"quota exceeded"}`), nil, http.StatusTooManyRequests, 10*time.Minute)
		if !isQuota {
			t.Fatal("expected isQuota=true for a sub-config quota signal")
		}
		if cd != 10*time.Minute {
			t.Fatalf("expected configured 10m to win, got %v", cd)
		}
	})

	t.Run("header window wins longest", func(t *testing.T) {
		cd, isQuota := derive429Cooldown(logger, []byte(`{"error":"quota exceeded"}`), h, http.StatusTooManyRequests, time.Minute)
		if !isQuota {
			t.Fatal("expected isQuota=true")
		}
		if cd < 6*24*time.Hour {
			t.Fatalf("expected ~7d window to win, got %v", cd)
		}
	})
}

// TestParseQuotaResetFields pins NENYA-43's body-side parsing: reset fields
// nested under error/metadata or top-level, case-insensitive keys, unix ms/s,
// RFC3339, and duration-string values; past values and unrelated keys are
// ignored.
func TestParseQuotaResetFields(t *testing.T) {
	now := time.Now()
	in1h := now.Add(time.Hour).UTC().Format(time.RFC3339)

	tests := []struct {
		name string
		body string
		want time.Duration
		tol  time.Duration
	}{
		{
			name: "top-level resets_in_seconds",
			body: `{"resets_in_seconds": 120}`,
			want: 120 * time.Second,
			tol:  time.Second,
		},
		{
			name: "nested error.rate_limit_reset unix seconds",
			body: `{"error":{"rate_limit_reset": ` + itoa(now.Add(30*time.Minute).Unix()) + `}}`,
			want: 30 * time.Minute,
			tol:  time.Minute,
		},
		{
			name: "error.metadata Reset_At unix ms",
			body: `{"error":{"metadata":{"Reset_At": ` + itoa(now.Add(2*time.Hour).UnixMilli()) + `}}}`,
			want: 2 * time.Hour,
			tol:  time.Minute,
		},
		{
			name: "rfc3339 string value",
			body: `{"error":{"reset_at":"` + in1h + `"}}`,
			want: time.Hour,
			tol:  time.Minute,
		},
		{
			name: "duration string value",
			body: `{"error":{"reset_after":"6m0s"}}`,
			want: 6 * time.Minute,
			tol:  time.Second,
		},
		{
			name: "past reset ignored",
			body: `{"error":{"reset_at":"` + now.Add(-time.Hour).UTC().Format(time.RFC3339) + `"}}`,
			want: 0,
		},
		{
			name: "no reset key",
			body: `{"error":{"message":"quota exceeded"}}`,
			want: 0,
		},
		{
			name: "invalid json",
			body: `{invalid`,
			want: 0,
		},
		{
			name: "capped at maxRateLimitWindowCooldown",
			body: `{"resets_in_seconds": 1000000}`,
			want: maxRateLimitWindowCooldown,
			tol:  time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseQuotaResetFields([]byte(tt.body))
			diff := got - tt.want
			if diff < 0 {
				diff = -diff
			}
			if diff > tt.tol {
				t.Errorf("parseQuotaResetFields() = %v; want ~%v", got, tt.want)
			}
		})
	}
}

// TestOverageRejectionsAreNotCooldowns pins NENYA-168: Anthropic's overage
// (pay-as-you-go billing) signals are billing-cycle boundaries, not retry
// horizons. An overage-only rejection must not schedule a cooldown from
// Retry-After or from an overage reset field, while a standard-window rejection
// keeps its full cooldown.
func TestOverageRejectionsAreNotCooldowns(t *testing.T) {
	now := time.Now()
	in7d := now.Add(7 * 24 * time.Hour).UTC().Format(time.RFC3339)

	t.Run("overage reset header alone derives nothing", func(t *testing.T) {
		// The invariant the subtest pins: the overage reset must never join the
		// window list, or this subtest silently starts deriving 7-day cooldowns.
		for _, name := range anthropicUnifiedResetHeaders {
			if name == headerUnifiedOverageReset {
				t.Fatalf("%s must never be a cooldown source (NENYA-168)", name)
			}
		}
		h := http.Header{}
		h.Set(headerUnifiedOverageReset, in7d)
		if got := deriveUpstreamRateLimitCooldown(h); got != 0 {
			t.Fatalf("cooldown = %v, want 0 (an overage reset is not a retry horizon)", got)
		}
	})

	t.Run("retry-after is skipped on an overage-only rejection", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "3600")
		h.Set(headerUnifiedOverageStatus, "rejected")
		h.Set(headerUnifiedStatus, "allowed")
		if got := deriveUpstreamRateLimitCooldown(h); got != 0 {
			t.Fatalf("cooldown = %v, want 0 (the overage claim is the only rejection)", got)
		}
	})

	t.Run("representative claim naming overage skips retry-after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "3600")
		h.Set(headerUnifiedClaim, "overage")
		if got := deriveUpstreamRateLimitCooldown(h); got != 0 {
			t.Fatalf("cooldown = %v, want 0 (representative claim is overage)", got)
		}
	})

	t.Run("a standard window rejection keeps retry-after", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "3600")
		h.Set(headerUnifiedOverageStatus, "rejected")
		h.Set(headerUnified5hStatus, "rejected")
		got := deriveUpstreamRateLimitCooldown(h)
		if got < time.Hour-time.Minute {
			t.Fatalf("cooldown = %v, want ~1h (the 5h window is rejected too)", got)
		}
	})

	t.Run("body overage reset fields are ignored", func(t *testing.T) {
		body := `{"error":{"overage_reset":"` + in7d + `","overage_resets_at":"` + in7d + `"}}`
		if got := parseQuotaResetFields([]byte(body)); got != 0 {
			t.Fatalf("cooldown = %v, want 0 (overage reset fields are billing boundaries)", got)
		}
	})

	t.Run("a standard reset field beside an overage field still counts", func(t *testing.T) {
		// Discriminating: dropping the overage veto must not change this result,
		// while reintroducing the overage field as a cooldown source would
		// change TestOverageRejectionsAreNotCooldowns' body case above.
		body := `{"error":{"overage_reset":"` + in7d + `","reset_at":"` + now.Add(time.Hour).UTC().Format(time.RFC3339) + `"}}`
		got := parseQuotaResetFields([]byte(body))
		if got < time.Hour-time.Minute || got > time.Hour+time.Minute {
			t.Fatalf("cooldown = %v, want ~1h", got)
		}
	})
}
