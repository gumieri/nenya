package config

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// PeakWindow declares one peak pricing interval in UTC (see
// docs/COST_MODEL.md). Start/End are "HH:MM" 24-hour times; the interval is
// half-open [Start, End) and Start > End wraps midnight. WeekdaysOnly
// restricts the window to Monday–Friday (the start-side day decides for
// wrap-around windows). A malformed interval is never peak.
type PeakWindow struct {
	Start string `json:"start"`
	End   string `json:"end"`
	// WeekdaysOnly restricts the window to Monday through Friday in the
	// window-start day's calendar (weekends are off-peak).
	WeekdaysOnly bool `json:"weekdays_only,omitempty"`
}

// Contains reports whether instant t (converted to UTC) falls inside the
// half-open [Start, End) window. A nil, empty, or malformed window is never
// peak.
func (w *PeakWindow) Contains(t time.Time) bool {
	if w == nil {
		return false
	}
	start, okStart := parseClockMinutes(w.Start)
	end, okEnd := parseClockMinutes(w.End)
	if !okStart || !okEnd || start == end {
		return false
	}
	u := t.UTC()
	minutes := u.Hour()*60 + u.Minute()
	var inInterval bool
	if start < end {
		inInterval = minutes >= start && minutes < end
	} else {
		// Wrap-around: the window crosses midnight.
		inInterval = minutes >= start || minutes < end
	}
	if !inInterval {
		return false
	}
	if w.WeekdaysOnly {
		// The window belongs to the calendar day its START falls in: for a
		// wrap window, instants after midnight belong to the previous day.
		day := u
		if start > end && minutes < end {
			day = u.AddDate(0, 0, -1)
		}
		if wd := day.Weekday(); wd == time.Saturday || wd == time.Sunday {
			return false
		}
	}
	return true
}

// Validate checks that both bounds are well-formed "HH:MM" times. Start == End
// is rejected as ambiguous (an empty window should simply be absent).
func (w *PeakWindow) Validate() error {
	if w == nil {
		return nil
	}
	if _, ok := parseClockMinutes(w.Start); !ok {
		return fmt.Errorf("PeakWindow.Start must be HH:MM (24h), got %q", w.Start)
	}
	if _, ok := parseClockMinutes(w.End); !ok {
		return fmt.Errorf("PeakWindow.End must be HH:MM (24h), got %q", w.End)
	}
	if w.Start == w.End {
		return errors.New("PeakWindow.Start must differ from End (an empty window should be absent)")
	}
	return nil
}

// parseClockMinutes parses "HH:MM" (24h) into minutes since midnight. Each
// component must be 1–2 ASCII digits with no sign prefix ("9:05" and "09:05"
// are both accepted; only the numeric range is enforced beyond that).
func parseClockMinutes(s string) (int, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 2 {
		return 0, false
	}
	h, okH := parseClockComponent(parts[0])
	m, okM := parseClockComponent(parts[1])
	if !okH || !okM {
		return 0, false
	}
	if h > 23 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// parseClockComponent parses one 1–2 digit clock component.
func parseClockComponent(s string) (int, bool) {
	if len(s) == 0 || len(s) > 2 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int(s[i]-'0')
	}
	return n, true
}

// PricingOverride allows overriding a model's default per-token pricing.
// Zero values mean "use the built-in pricing".
//
// InputCostPer1M/OutputCostPer1M are the standard (off-peak) baseline rates.
// PeakInputCostPer1M/PeakOutputCostPer1M are the peak-window rates; zero means
// the provider/model has no peak window. CachedInputCostPer1M is the discounted
// cache-read input rate; zero means cache reads bill at the window input rate.
// See docs/COST_MODEL.md for the evaluation contract.
type PricingOverride struct {
	InputCostPer1M       float64 `json:"input_cost_per_1m"`
	OutputCostPer1M      float64 `json:"output_cost_per_1m"`
	PeakInputCostPer1M   float64 `json:"peak_input_cost_per_1m,omitempty"`
	PeakOutputCostPer1M  float64 `json:"peak_output_cost_per_1m,omitempty"`
	CachedInputCostPer1M float64 `json:"cached_input_cost_per_1m,omitempty"`
}

// IsZero reports whether no rate is configured. A peak-only or cached-only
// entry is not zero (the model is priced, just not on the baseline fields).
func (p PricingOverride) IsZero() bool {
	return p.InputCostPer1M == 0 && p.OutputCostPer1M == 0 &&
		p.PeakInputCostPer1M == 0 && p.PeakOutputCostPer1M == 0 &&
		p.CachedInputCostPer1M == 0
}

// HasPeak reports whether the entry declares a peak-window rate.
func (p PricingOverride) HasPeak() bool {
	return p.PeakInputCostPer1M != 0 || p.PeakOutputCostPer1M != 0
}

// HasStandardRate reports whether a usable standard (off-peak) baseline rate is
// set. Peak-only entries are non-zero but have no standard rate, so callers
// that price with the standard pair (Peak unset) must gate on this — otherwise
// a peak-only entry would silently price at $0.
func (p PricingOverride) HasStandardRate() bool {
	return p.InputCostPer1M != 0 || p.OutputCostPer1M != 0
}

// Validate checks that every configured rate is non-negative. Zero is valid
// (absent); negatives are rejected.
func (p PricingOverride) Validate() error {
	if p.InputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.InputCostPer1M must be non-negative, got %f", p.InputCostPer1M)
	}
	if p.OutputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.OutputCostPer1M must be non-negative, got %f", p.OutputCostPer1M)
	}
	if p.PeakInputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.PeakInputCostPer1M must be non-negative, got %f", p.PeakInputCostPer1M)
	}
	if p.PeakOutputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.PeakOutputCostPer1M must be non-negative, got %f", p.PeakOutputCostPer1M)
	}
	if p.CachedInputCostPer1M < 0 {
		return fmt.Errorf("PricingOverride.CachedInputCostPer1M must be non-negative, got %f", p.CachedInputCostPer1M)
	}
	return nil
}

// ModelThinkingConfig captures reasoning/thinking capabilities for supported models
// in the static ModelRegistry, describing what the model supports natively.
//
// Fields:
//   - Min: Minimum number of thinking tokens (optional, default 0)
//   - Max: Maximum number of thinking tokens (optional, default 0)
//   - ZeroAllowed: Whether zero thinking tokens are permitted (default false)
//   - DynamicAllowed: Whether dynamic thinking is supported (default false)
//   - Adaptive: Whether the model supports adaptive thinking with display override (default false)
//   - Levels: Available thinking intensity levels like "low", "medium", "high" (optional)
//
// Zero values for Min/Max mean the field is unset (no thinking).
// Non-zero Min implies thinking is enabled with the specified budget.
// If both Min and Max are zero, ZeroAllowed must be true.
type ModelThinkingConfig struct {
	Min            int      `json:"min,omitempty"`
	Max            int      `json:"max,omitempty"`
	ZeroAllowed    bool     `json:"zero_allowed,omitempty"`
	DynamicAllowed bool     `json:"dynamic_allowed,omitempty"`
	Adaptive       bool     `json:"adaptive,omitempty"`
	Levels         []string `json:"levels,omitempty"`
}

// HasThinking reports whether the model declares any thinking/reasoning
// configuration. This is the canonical reasoning signal for static registry
// entries when no dynamic capability metadata is available.
func (c ModelThinkingConfig) HasThinking() bool {
	return c.Min > 0 || c.Max > 0 || len(c.Levels) > 0
}

// Validate checks that Min <= Max when both fields are set and rejects
// negative values for all fields.
//
// Returns an error if:
//   - Min or Max is negative
//   - Both Min and Max are set but Min > Max
//   - Adaptive is true but Max is zero (adaptive requires a budget)
func (c ModelThinkingConfig) Validate() error {
	if c.Min < 0 {
		return fmt.Errorf("ModelThinkingConfig.Min must be non-negative, got %d", c.Min)
	}
	if c.Max < 0 {
		return fmt.Errorf("ModelThinkingConfig.Max must be non-negative, got %d", c.Max)
	}
	if c.Min > 0 && c.Max > 0 && c.Min > c.Max {
		return fmt.Errorf("ModelThinkingConfig.Min (%d) must be <= Max (%d)", c.Min, c.Max)
	}
	if c.Adaptive && c.Max == 0 {
		return fmt.Errorf("ModelThinkingConfig.Adaptive requires Max > 0")
	}
	return nil
}

// ModelEntry defines a model in the static ModelRegistry: its provider,
// context limits, wire format, capabilities, scoring bonus, and pricing.
type ModelEntry struct {
	Provider     string
	MaxContext   int
	MaxOutput    int
	Format       string              `json:"format,omitempty"`
	Thinking     ModelThinkingConfig `json:"thinking,omitempty"`
	ScoreBonus   float64             `json:"score_bonus,omitempty"`
	Capabilities []string            `json:"capabilities,omitempty"`
	Pricing      PricingOverride     `json:"pricing,omitempty"`
}

func (e ModelEntry) Validate() error {
	if e.Provider == "" {
		return errors.New("ModelEntry.Provider is required")
	}
	if e.MaxContext < 0 {
		return fmt.Errorf("ModelEntry.MaxContext must be non-negative, got %d", e.MaxContext)
	}
	if e.MaxOutput < 0 {
		return fmt.Errorf("ModelEntry.MaxOutput must be non-negative, got %d", e.MaxOutput)
	}
	if err := e.Thinking.Validate(); err != nil {
		return err
	}
	if err := e.Pricing.Validate(); err != nil {
		return err
	}
	return nil
}

// ModelRef is a lightweight reference to a model with its context limits.
type ModelRef struct {
	ID         string
	MaxContext int
	MaxOutput  int
}

// ProviderEntry defines a built-in provider's URL, auth style, API
// format, format-specific URL overrides, and associated model references.
type ProviderEntry struct {
	URL        string
	AuthStyle  string
	ApiFormat  string
	FormatURLs map[string]string `json:"format_urls,omitempty"`
	Models     []ModelRef
	// RatelimitMaxRPM is an optional built-in requests-per-minute default
	// for this provider. nil = no built-in default (the governance global
	// applies); 0 = the dimension is disabled for this provider; >0 = the
	// limit. A user-config value always takes precedence.
	RatelimitMaxRPM *int `json:"ratelimit_max_rpm,omitempty"`
	// RatelimitMaxTPM is the tokens-per-minute counterpart of
	// RatelimitMaxRPM, with identical semantics.
	RatelimitMaxTPM *int `json:"ratelimit_max_tpm,omitempty"`
	// NonChatModels classifies provider models that are not served by the
	// chat-completions endpoint (e.g. TypeSafe Jev System One decision
	// models on OpenCode Zen). See ProviderConfig.NonChatModels.
	NonChatModels []string `json:"non_chat_models,omitempty"`
	// PeakWindows are the built-in peak pricing intervals (UTC) for
	// providers with time-of-day rate cards. See
	// ProviderConfig.PeakWindows. The slice aliases the package-level
	// ProviderRegistry global — treat it as shared-immutable.
	PeakWindows []PeakWindow `json:"peak_windows,omitempty"`
}

func (e ProviderEntry) ToProviderConfig() ProviderConfig {
	return ProviderConfig{
		URL:             e.URL,
		AuthStyle:       e.AuthStyle,
		ApiFormat:       e.ApiFormat,
		FormatURLs:      e.FormatURLs,
		RatelimitMaxRPM: e.RatelimitMaxRPM,
		RatelimitMaxTPM: e.RatelimitMaxTPM,
		NonChatModels:   e.NonChatModels,
		PeakWindows:     e.PeakWindows,
	}
}
