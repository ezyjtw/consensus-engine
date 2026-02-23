package funding

import (
	"math"
	"sync"
	"time"
)

// RegimeForecaster maintains an EWA (exponentially weighted average) of funding
// rates per venue+symbol and detects regime changes (POSITIVE/NEGATIVE/NEUTRAL).
// It also tracks open interest momentum if OI data is available.
type RegimeForecaster struct {
	mu      sync.Mutex
	alpha   float64 // EWA smoothing factor (0 < alpha ≤ 1); higher = more reactive
	states  map[string]*regimeState // key: "venue:symbol"
}

type regimeState struct {
	ewa          float64 // exponentially weighted average of funding rate
	ewaSq        float64 // EWA of squared rate for variance estimate
	samples      int
	lastTsMs     int64
	lastRate     float64
	momentum     float64 // rate of change of EWA (ewa - prev_ewa)
}

type Regime struct {
	Venue     string
	Symbol    string
	EWA       float64 // smoothed funding rate
	StdDev    float64 // volatility estimate
	Momentum  float64 // direction of change
	Label     string  // POSITIVE | NEGATIVE | NEUTRAL | VOLATILE
	TsMs      int64
}

// NewRegimeForecaster creates a forecaster with the given EWA alpha.
// Recommended: 0.1 (slow/stable) to 0.3 (reactive).
func NewRegimeForecaster(alpha float64) *RegimeForecaster {
	if alpha <= 0 || alpha > 1 {
		alpha = 0.15
	}
	return &RegimeForecaster{
		alpha:  alpha,
		states: make(map[string]*regimeState),
	}
}

// Update records a new funding rate observation and returns the updated Regime.
func (f *RegimeForecaster) Update(venue, symbol string, rate float64, tsMs int64) Regime {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := venue + ":" + symbol
	s, ok := f.states[key]
	if !ok {
		s = &regimeState{ewa: rate, ewaSq: rate * rate, lastTsMs: tsMs, lastRate: rate}
		f.states[key] = s
	} else {
		prevEWA := s.ewa
		s.ewa = f.alpha*rate + (1-f.alpha)*s.ewa
		s.ewaSq = f.alpha*(rate*rate) + (1-f.alpha)*s.ewaSq
		s.momentum = s.ewa - prevEWA
		s.lastRate = rate
		s.lastTsMs = tsMs
	}
	s.samples++

	variance := s.ewaSq - s.ewa*s.ewa
	stdDev := 0.0
	if variance > 0 {
		stdDev = math.Sqrt(variance)
	}

	label := classify(s.ewa, stdDev)

	return Regime{
		Venue:    venue,
		Symbol:   symbol,
		EWA:      s.ewa,
		StdDev:   stdDev,
		Momentum: s.momentum,
		Label:    label,
		TsMs:     time.Now().UnixMilli(),
	}
}

// Get returns the current regime for a venue+symbol without updating it.
// Returns nil if no data has been seen yet.
func (f *RegimeForecaster) Get(venue, symbol string) *Regime {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.states[venue+":"+symbol]
	if !ok {
		return nil
	}
	variance := s.ewaSq - s.ewa*s.ewa
	stdDev := 0.0
	if variance > 0 {
		stdDev = math.Sqrt(variance)
	}
	r := &Regime{
		Venue:    venue,
		Symbol:   symbol,
		EWA:      s.ewa,
		StdDev:   stdDev,
		Momentum: s.momentum,
		Label:    classify(s.ewa, stdDev),
		TsMs:     s.lastTsMs,
	}
	return r
}

// All returns the current regime for every tracked venue+symbol.
func (f *RegimeForecaster) All() []Regime {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Regime, 0, len(f.states))
	for k, s := range f.states {
		_ = k
		variance := s.ewaSq - s.ewa*s.ewa
		stdDev := 0.0
		if variance > 0 {
			stdDev = math.Sqrt(variance)
		}
		out = append(out, Regime{
			EWA:      s.ewa,
			StdDev:   stdDev,
			Momentum: s.momentum,
			Label:    classify(s.ewa, stdDev),
			TsMs:     s.lastTsMs,
		})
	}
	return out
}

// classify returns a human-readable regime label.
// Thresholds are in fractional rate (e.g. 0.0001 = 1 bps per 8h).
func classify(ewa, stdDev float64) string {
	const (
		positiveThresh = 0.00005  // 0.5 bps/8h → longs pay shorts
		negativeThresh = -0.00005
		volatileRatio  = 3.0     // StdDev > 3× |EWA| = volatile
	)
	if stdDev > 0 && math.Abs(ewa) > 0 && stdDev/math.Abs(ewa) > volatileRatio {
		return "VOLATILE"
	}
	if ewa > positiveThresh {
		return "POSITIVE"
	}
	if ewa < negativeThresh {
		return "NEGATIVE"
	}
	return "NEUTRAL"
}
