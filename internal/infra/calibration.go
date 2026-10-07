package infra

import (
	"math"
	"sync"
)

// CalibrationDefaults are the resolved defaults for the token-estimation
// calibration loop (NENYA-135). cl100k_base drifts against non-OpenAI
// tokenizers (Gemini, DeepSeek CJK, Mistral); the tracker learns a
// per-model actual/estimated ratio from real upstream usage and the
// gateway applies it before budget-critical estimates.
const (
	CalibrationDefaultMinObservations = 50
	CalibrationDefaultDecay           = 0.99
	CalibrationDefaultClampMin        = 0.5
	CalibrationDefaultClampMax        = 4.0
)

// CalibrationParams are the resolved knob values for a CalibrationTracker.
// The zero value is not useful; build via config's
// EffectiveTokenCalibrationParams (consumer-side resolution).
type CalibrationParams struct {
	Enabled         bool
	MinObservations int
	Decay           float64
	ClampMin        float64
	ClampMax        float64
}

// calibrationSample holds the decaying token sums for one model. Each sum
// is an independent EMA over its own stream (estimates arrive at dispatch,
// actuals arrive with the response), decayed by that stream's elapsed
// records — so alternating estimate/actual calls cannot skew the ratio.
type calibrationSample struct {
	estimateTokens float64
	actualTokens   float64
	records        int
	recordClock    uint64 // global record counter for this model
	estSeq         uint64 // clock value of the estimate sum's last update
	actSeq         uint64 // clock value of the actual sum's last update
}

// CalibrationTracker learns a per-model correction ratio
// (actual cl100k drift) from paired aggregate streams: the gateway records
// post-transform cl100k estimates at dispatch and real prompt tokens from
// upstream usage. Per-model ratio = decaying(actualSum) /
// decaying(estimateSum). Aggregate pairing (not per-request) is deliberate:
// estimate and actual flow through different code paths (dispatch vs
// response), and per-model sums over the same traffic converge to the same
// ratio without threading request-scoped state.
//
// All methods are nil-safe: a nil tracker is a disabled tracker (Ratio
// returns 1.0), so call sites never branch on configuration.
//
// Locking: mu guards params and models; reads take RLock, writes Lock.
type CalibrationTracker struct {
	mu     sync.RWMutex
	params CalibrationParams
	models map[string]*calibrationSample
}

// NewCalibrationTracker builds a tracker with the given resolved params.
// Params.Enabled=false yields a tracker that records nothing and whose
// Ratio is always 1.0 (the gateway normally passes nil instead; a disabled
// tracker is kept for Snapshot observability only).
func NewCalibrationTracker(params CalibrationParams) *CalibrationTracker {
	if params.MinObservations <= 0 {
		params.MinObservations = CalibrationDefaultMinObservations
	}
	if params.Decay <= 0 || params.Decay >= 1 {
		params.Decay = CalibrationDefaultDecay
	}
	if params.ClampMin <= 0 || math.IsNaN(params.ClampMin) || math.IsInf(params.ClampMin, 0) {
		params.ClampMin = CalibrationDefaultClampMin
	}
	if math.IsNaN(params.ClampMax) || math.IsInf(params.ClampMax, 0) {
		params.ClampMax = CalibrationDefaultClampMax
	}
	if params.ClampMax < params.ClampMin {
		// Inverted clamp (e.g. min configured above the default max):
		// collapse onto the min so the clamp remains a valid interval
		// instead of silently pinning every ratio to the max.
		params.ClampMax = params.ClampMin
	}
	return &CalibrationTracker{
		params: params,
		models: make(map[string]*calibrationSample),
	}
}

// RecordEstimate records a post-transform cl100k estimate dispatched for
// the model. Nil-safe.
func (t *CalibrationTracker) RecordEstimate(model string, tokens int) {
	if t == nil || tokens <= 0 || model == "" {
		return
	}
	t.record(model, float64(tokens), 0)
}

// RecordActual records real prompt tokens reported by the upstream usage
// payload for the model. Nil-safe.
func (t *CalibrationTracker) RecordActual(model string, tokens int) {
	if t == nil || tokens <= 0 || model == "" {
		return
	}
	t.record(model, 0, float64(tokens))
}

func (t *CalibrationTracker) record(model string, estimate, actual float64) {
	if !t.params.Enabled {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.models[model]
	if !ok {
		s = &calibrationSample{}
		t.models[model] = s
	}
	s.recordClock++
	d := t.params.Decay
	if estimate > 0 {
		s.estimateTokens = s.estimateTokens*math.Pow(d, float64(s.recordClock-s.estSeq)) + estimate
		s.estSeq = s.recordClock
	}
	if actual > 0 {
		s.actualTokens = s.actualTokens*math.Pow(d, float64(s.recordClock-s.actSeq)) + actual
		s.actSeq = s.recordClock
	}
	s.records++
}

// Ratio returns the model's calibrated actual/estimated multiplier, or 1.0
// when the tracker is nil, the model is unobserved, the decaying
// estimate mass is below MinObservations (one cl100k token as the unit —
// cheap, scale-free warm-up threshold), or no ground-truth actuals have
// arrived yet (estimates alone cannot form a ratio). The ratio is clamped
// to [ClampMin, ClampMax] so a pathological window cannot blow up budgets.
func (t *CalibrationTracker) Ratio(model string) float64 {
	if t == nil || model == "" || !t.params.Enabled {
		return 1.0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.models[model]
	if !ok {
		return 1.0
	}
	return t.ratioLocked(s)
}

func (t *CalibrationTracker) ratioLocked(s *calibrationSample) float64 {
	if s.estimateTokens < float64(t.params.MinObservations) || s.actualTokens <= 0 {
		return 1.0
	}
	ratio := s.actualTokens / s.estimateTokens
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return 1.0
	}
	return math.Min(math.Max(ratio, t.params.ClampMin), t.params.ClampMax)
}

// CalibrationStat is the per-model observability record served by /statsz.
// Records counts Record* calls (estimates and actuals independently), not
// request pairs.
type CalibrationStat struct {
	Ratio          float64 `json:"ratio"`
	EstimateTokens float64 `json:"estimate_tokens"`
	ActualTokens   float64 `json:"actual_tokens"`
	Records        int     `json:"records"`
	Calibrated     bool    `json:"calibrated"`
}

// Snapshot returns the per-model calibration state (encoding/json sorts
// map keys, so the served /statsz output is stable). Nil-safe (empty map).
func (t *CalibrationTracker) Snapshot() map[string]CalibrationStat {
	out := make(map[string]CalibrationStat)
	if t == nil {
		return out
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	for model, s := range t.models {
		out[model] = CalibrationStat{
			Ratio:          t.ratioLocked(s),
			EstimateTokens: s.estimateTokens,
			ActualTokens:   s.actualTokens,
			Records:        s.records,
			Calibrated:     s.estimateTokens >= float64(t.params.MinObservations) && s.actualTokens > 0,
		}
	}
	return out
}

// Reconfigured returns a tracker carrying this tracker's learned sums
// under new params (SIGHUP reload: knob changes apply without discarding
// the learned drift). nil-safe: reconfiguring a nil tracker yields nil.
func (t *CalibrationTracker) Reconfigured(params CalibrationParams) *CalibrationTracker {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	next := NewCalibrationTracker(params)
	for model, s := range t.models {
		cp := *s
		next.models[model] = &cp
	}
	return next
}
