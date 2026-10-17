package discovery

import (
	"testing"

	"github.com/nenya/config"
)

// TestCalculateCostCacheWriteDimension pins NENYA-163: cache-write tokens
// price at the card's cache_write rate (distinct from reads and the input
// base), falling back to the input rate when the card declares none, and
// clamping disjointly against reads.
func TestCalculateCostCacheWriteDimension(t *testing.T) {
	card := PricingEntry{
		InputCostPer1M:       3.0,
		OutputCostPer1M:      15.0,
		CachedInputCostPer1M: 0.30,
		CacheWriteCostPer1M:  3.75,
	}

	// 1M written tokens price at the write rate alone.
	got := card.CalculateCost(PricingUsage{Input: 1_000_000, CacheWriteInput: 1_000_000})
	if got != 3.75 {
		t.Fatalf("write-only input = %v, want 3.75", got)
	}

	// Mixed: reads at the read rate, writes at the write rate, the rest at
	// the input rate — disjoint subsets of the same total.
	got = card.CalculateCost(PricingUsage{Input: 1_000_000, CachedInput: 400_000, CacheWriteInput: 300_000})
	want := (0.4*0.30 + 0.3*3.75 + 0.3*3.0)
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("mixed split = %v, want %v", got, want)
	}

	// Overlap clamps: reads + writes cannot exceed the total input.
	got = card.CalculateCost(PricingUsage{Input: 1_000_000, CachedInput: 800_000, CacheWriteInput: 500_000})
	want = (0.8*0.30 + 0.2*3.75)
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("overlap clamp = %v, want %v", got, want)
	}

	// Fallback: a card without a write rate bills writes at the input rate.
	fallback := PricingEntry{InputCostPer1M: 3.0, OutputCostPer1M: 15.0}
	got = fallback.CalculateCost(PricingUsage{Input: 1_000_000, CacheWriteInput: 1_000_000})
	if got != 3.0 {
		t.Fatalf("the documented fallback bills writes at the input rate, got %v", got)
	}

	// A write-only card is priced (not zero).
	writeOnly := PricingEntry{CacheWriteCostPer1M: 3.75}
	if writeOnly.IsZero() {
		t.Fatal("a write-only card must not be zero")
	}
}

// TestPricingOverrideCacheWriteConversion pins the config→discovery
// conversion: a card's cache-write rate rides into the catalog entry (and
// the catalog merge preserves it), so /statsz cost_model can surface it.
func TestPricingOverrideCacheWriteConversion(t *testing.T) {
	override := config.PricingOverride{
		InputCostPer1M:       3.0,
		OutputCostPer1M:      15.0,
		CachedInputCostPer1M: 0.30,
		CacheWriteCostPer1M:  3.75,
	}
	entry := pricingEntryFromOverride(override)
	if entry == nil || entry.CacheWriteCostPer1M != 3.75 {
		t.Fatalf("the conversion dropped the cache-write rate: %#v", entry)
	}
}
