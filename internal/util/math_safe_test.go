package util

import (
	"math"
	"testing"
)

// TestSafeMicroUSD pins the conversion contract at every boundary the
// cost-recording sites rely on (AUD-70): NaN/Inf and int64-overflow
// magnitudes report ok=false, ordinary and negative amounts convert with
// rounding, and the exact overflow threshold stays convertible.
func TestSafeMicroUSD(t *testing.T) {
	cases := []struct {
		name   string
		usd    float64
		want   int64
		wantOK bool
	}{
		{"ordinary", 1.5, 1_500_000, true},
		{"rounds half away", 0.0000005, 1, true},
		{"negative", -2.25, -2_250_000, true},
		{"zero", 0, 0, true},
		{"small", 0.000001, 1, true},
		{"NaN", math.NaN(), 0, false},
		{"+Inf", math.Inf(1), 0, false},
		{"-Inf", math.Inf(-1), 0, false},
		{"overflow", math.MaxFloat64, 0, false},
		{"negative overflow", -math.MaxFloat64, 0, false},
	}
	for _, tc := range cases {
		got, ok := SafeMicroUSD(tc.usd)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: SafeMicroUSD(%v) = (%d, %v), want (%d, %v)", tc.name, tc.usd, got, ok, tc.want, tc.wantOK)
		}
	}

	// The guard is conservative: the exact threshold value (and beyond) is
	// rejected via >=; just under it converts.
	thresh := math.MaxInt64 / 1e6
	if _, ok := SafeMicroUSD(thresh); ok {
		t.Error("at the MaxInt64/1e6 threshold the guard must reject")
	}
	if _, ok := SafeMicroUSD(math.Nextafter(thresh, 0)); !ok {
		t.Error("just under the MaxInt64/1e6 threshold must convert")
	}
}

// TestSafeUSDCents pins the cent-resolution contract (AUD-70).
func TestSafeUSDCents(t *testing.T) {
	cases := []struct {
		name   string
		usd    float64
		want   int64
		wantOK bool
	}{
		{"ordinary", 1.5, 150, true},
		{"rounds half away", 0.125, 13, true},
		{"negative", -2.25, -225, true},
		{"zero", 0, 0, true},
		{"NaN", math.NaN(), 0, false},
		{"+Inf", math.Inf(1), 0, false},
		{"overflow", math.MaxFloat64, 0, false},
	}
	for _, tc := range cases {
		got, ok := SafeUSDCents(tc.usd)
		if ok != tc.wantOK || got != tc.want {
			t.Errorf("%s: SafeUSDCents(%v) = (%d, %v), want (%d, %v)", tc.name, tc.usd, got, ok, tc.want, tc.wantOK)
		}
	}

	// The guard is conservative: the exact threshold value is rejected via
	// >=; just under it converts.
	thresh := float64(math.MaxInt64 / 100)
	if _, ok := SafeUSDCents(thresh); ok {
		t.Error("at the MaxInt64/100 threshold the guard must reject")
	}
	if _, ok := SafeUSDCents(math.Nextafter(thresh, 0)); !ok {
		t.Error("just under the MaxInt64/100 threshold must convert")
	}
}
