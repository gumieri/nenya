package config

import (
	"testing"
	"time"
)

func peakAt(w *PeakWindow, hh, mm int, loc *time.Location) bool {
	return w.Contains(time.Date(2026, 10, 2, hh, mm, 0, 0, loc))
}

func TestPeakWindow_Contains(t *testing.T) {
	utc := time.UTC
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}

	normal := &PeakWindow{Start: "09:00", End: "17:00"}
	wrap := &PeakWindow{Start: "22:00", End: "04:00"}

	tests := []struct {
		name string
		w    *PeakWindow
		hh   int
		mm   int
		loc  *time.Location
		want bool
	}{
		{"inside", normal, 12, 0, utc, true},
		{"start inclusive", normal, 9, 0, utc, true},
		{"end exclusive", normal, 17, 0, utc, false},
		{"before", normal, 8, 59, utc, false},
		{"after", normal, 17, 1, utc, false},
		{"wrap inside late", wrap, 23, 30, utc, true},
		{"wrap inside early", wrap, 1, 0, utc, true},
		{"wrap after end", wrap, 4, 0, utc, false},
		{"wrap before start", wrap, 21, 59, utc, false},
		{"converted to UTC", normal, 5, 30, ny, true}, // 05:30 EDT = 09:30 UTC
		{"converted to UTC outside", normal, 13, 30, ny, false},
		{"nil never peak", nil, 12, 0, utc, false},
		{"malformed never peak", &PeakWindow{Start: "abc", End: "12:00"}, 12, 0, utc, false},
		{"equal bounds never peak", &PeakWindow{Start: "09:00", End: "09:00"}, 9, 0, utc, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peakAt(tt.w, tt.hh, tt.mm, tt.loc); got != tt.want {
				t.Errorf("Contains = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPeakWindow_Validate(t *testing.T) {
	tests := []struct {
		name    string
		w       *PeakWindow
		wantErr bool
	}{
		{"nil ok", nil, false},
		{"valid normal", &PeakWindow{Start: "00:30", End: "16:30"}, false},
		{"valid wrap", &PeakWindow{Start: "22:00", End: "04:00"}, false},
		{"unpadded ok", &PeakWindow{Start: "9:05", End: "17:00"}, false},
		{"bad start", &PeakWindow{Start: "24:00", End: "04:00"}, true},
		{"bad end", &PeakWindow{Start: "09:00", End: "4pm"}, true},
		{"missing colon", &PeakWindow{Start: "0900", End: "17:00"}, true},
		{"equal bounds", &PeakWindow{Start: "09:00", End: "09:00"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.w.Validate(); (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// TestBuiltinDeepSeekPeakWindow pins the built-in DeepSeek peak interval
// (00:30–16:30 UTC — the complement of the published off-peak discount window).
func TestBuiltinDeepSeekPeakWindow(t *testing.T) {
	entry, ok := ProviderRegistry["deepseek"]
	if !ok {
		t.Fatal("deepseek missing from ProviderRegistry")
	}
	p := entry.ToProviderConfig()
	if len(p.PeakWindows) == 0 {
		t.Fatal("deepseek built-in must declare peak windows")
	}
	for _, w := range p.PeakWindows {
		if err := w.Validate(); err != nil {
			t.Fatalf("invalid built-in window: %v", err)
		}
	}
	utc := time.UTC
	// 2026-10-07 is a Wednesday: 02:00 UTC inside the 01:00-04:00 block.
	if !p.IsPeakAt(time.Date(2026, 10, 7, 2, 0, 0, 0, utc)) {
		t.Error("Wednesday 02:00 UTC must be peak")
	}
	if p.IsPeakAt(time.Date(2026, 10, 7, 20, 0, 0, 0, utc)) {
		t.Error("Wednesday 20:00 UTC must be off-peak")
	}
}

func TestParseClockMinutes(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"00:00", 0, true},
		{"09:05", 545, true},
		{"9:05", 545, true},
		{"23:59", 1439, true},
		{"24:00", 0, false},
		{"09:60", 0, false},
		{"+9:00", 0, false},
		{"-0:00", 0, false},
		{"09:000", 0, false},
		{":30", 0, false},
		{"09:", 0, false},
		{"0900", 0, false},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, ok := parseClockMinutes(tt.in)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if ok && got != tt.want {
				t.Errorf("minutes = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestMergeProviderConfig_PreservesBuiltinPeakWindow pins the regression where
// a user override of any deepseek provider field silently dropped the built-in
// peak window.
func TestMergeProviderConfig_PreservesBuiltinPeakWindow(t *testing.T) {
	builtIn := ProviderRegistry["deepseek"].ToProviderConfig()
	if len(builtIn.PeakWindows) == 0 {
		t.Fatal("built-in deepseek must carry peak windows")
	}
	merged := mergeProviderConfig(ProviderConfig{URL: "https://example.internal/v1"}, builtIn)
	if len(merged.PeakWindows) != len(builtIn.PeakWindows) {
		t.Fatalf("windows = %+v, want %+v", merged.PeakWindows, builtIn.PeakWindows)
	}
	for i, w := range merged.PeakWindows {
		if w != builtIn.PeakWindows[i] {
			t.Errorf("window[%d] = %+v, want %+v", i, w, builtIn.PeakWindows[i])
		}
	}
}
