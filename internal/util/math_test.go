package util

import (
	"math"
	"testing"
)

func TestAddCap_NoOverflow(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{0, 0, 0},
		{1, 2, 3},
		{100, 200, 300},
		{math.MaxInt - 1, 1, math.MaxInt - 1 + 1},
		{0, math.MaxInt, math.MaxInt},
		{math.MaxInt, 0, math.MaxInt},
	}
	for _, tt := range tests {
		got := AddCap(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("AddCap(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestAddCap_Overflow(t *testing.T) {
	tests := []struct {
		a, b int
	}{
		{math.MaxInt, 1},
		{math.MaxInt, math.MaxInt},
		{math.MaxInt/2 + 1, math.MaxInt/2 + 1},
	}
	for _, tt := range tests {
		got := AddCap(tt.a, tt.b)
		if got != math.MaxInt {
			t.Errorf("AddCap(%d, %d) = %d, want MaxInt on overflow", tt.a, tt.b, got)
		}
	}
}

func TestAddCap_NegativeB(t *testing.T) {
	got := AddCap(10, -5)
	if got != 5 {
		t.Errorf("AddCap(10, -5) = %d, want 5", got)
	}
}

func TestJoinBackticks_Empty(t *testing.T) {
	got := JoinBackticks(nil)
	if got != "" {
		t.Errorf("JoinBackticks(nil) = %q, want empty", got)
	}
	got = JoinBackticks([]string{})
	if got != "" {
		t.Errorf("JoinBackticks([]) = %q, want empty", got)
	}
}

func TestJoinBackticks_Single(t *testing.T) {
	got := JoinBackticks([]string{"foo"})
	if got != "`foo`" {
		t.Errorf("JoinBackticks([foo]) = %q, want `foo`", got)
	}
}

func TestJoinBackticks_Multiple(t *testing.T) {
	got := JoinBackticks([]string{"foo", "bar", "baz"})
	want := "`foo`, `bar`, `baz`"
	if got != want {
		t.Errorf("JoinBackticks = %q, want %q", got, want)
	}
}

func TestErrNoProviderFmt(t *testing.T) {
	got := ErrNoProviderFmt("claude-3")
	want := "No provider configured for this model: claude-3"
	if got != want {
		t.Errorf("ErrNoProviderFmt = %q, want %q", got, want)
	}
}
