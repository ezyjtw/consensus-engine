// Package regime provides market regime detection that classifies the current
// market state and adjusts system behaviour accordingly.
package regime

import (
	"math"
	"sync"
	"time"
)

// MarketRegime represents the detected market state.
type MarketRegime string

const (
	RegimeCalm       MarketRegime = "CALM"
	RegimeTrending   MarketRegime = "TRENDING"
	RegimeVolatile   MarketRegime = "VOLATILE"
	RegimeCascade    MarketRegime = "LIQUIDATION_CASCADE"
)

// RegimeState is the full regime detection output.
type RegimeState struct {
	Regime           MarketRegime `json:"regime"`
	VolatilityBps    float64      `json:"volatility_bps"`     // realised vol in bps (1m window)
	TrendStrength    float64      `json:"trend_strength"`     // -1..+1 (negative = down trend)
	SpreadWidening   float64      `json:"spread_widening"`    // current spread / baseline
	LiquidationScore float64      `json:"liquidation_score"`  // 0..1 cascade probability
	Confidence       float64      `json:"confidence"`         // 0..1
	TsMs             int64        `json:"ts_ms"`
}

// StrategyAdjustment tells downstream services how to adapt.
type StrategyAdjustment struct {
	Regime             MarketRegime `json:"regime"`
	SizingMultiplier   float64      `json:"sizing_multiplier"`     // 0..2 (0.5 = half size)
	MinEdgeMultiplier  float64      `json:"min_edge_multiplier"`   // 1.5 = require 50% more edge
	MaxPositionMult    float64      `json:"max_position_mult"`     // scale max positions
	PassiveEnabled     bool         `json:"passive_enabled"`       // enable/disable passive orders
	AggressivenessLvl  int          `json:"aggressiveness_lvl"`    // 1=conservative, 5=aggressive
}

// Observation is a single data point fed to the detector.
type Observation struct {
	Symbol     string  `json:"symbol"`
	Mid        float64 `json:"mid"`
	SpreadBps  float64 `json:"spread_bps"`
	Volume1m   float64 `json:"volume_1m"`   // 1-minute volume in USD
	OI         float64 `json:"open_interest"`
	TsMs       int64   `json:"ts_ms"`
}

// Detector classifies the current market regime and produces strategy adjustments.
type Detector struct {
	mu           sync.RWMutex
	observations map[string][]Observation // symbol → observations
	baselines    map[string]*baseline     // symbol → rolling baselines
	windowMs     int64
	maxObs       int
}

type baseline struct {
	avgVolBps     float64
	avgSpreadBps  float64
	avgVolume1m   float64
	sampleCount   int
	lastUpdateMs  int64
}

// NewDetector creates a regime detector with the given lookback window.
func NewDetector(windowMs int64) *Detector {
	return &Detector{
		observations: make(map[string][]Observation),
		baselines:    make(map[string]*baseline),
		windowMs:     windowMs,
		maxObs:       5000,
	}
}

// Record adds an observation and potentially updates the regime.
func (d *Detector) Record(obs Observation) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.observations[obs.Symbol] = append(d.observations[obs.Symbol], obs)
	d.trimOld(obs.Symbol)
	d.updateBaseline(obs.Symbol)
}

// Detect returns the current regime state for a symbol.
func (d *Detector) Detect(symbol string) RegimeState {
	d.mu.RLock()
	defer d.mu.RUnlock()

	obs := d.observations[symbol]
	bl := d.baselines[symbol]
	now := time.Now().UnixMilli()

	if len(obs) < 10 || bl == nil {
		return RegimeState{Regime: RegimeCalm, Confidence: 0.1, TsMs: now}
	}

	// Compute recent volatility (last 60 seconds)
	recentVol := d.recentVolatility(obs, 60000)

	// Compute trend strength via linear regression slope
	trend := d.trendStrength(obs, 60000)

	// Spread widening
	recentSpread := d.recentAvg(obs, 60000, func(o Observation) float64 { return o.SpreadBps })
	spreadRatio := 1.0
	if bl.avgSpreadBps > 0 {
		spreadRatio = recentSpread / bl.avgSpreadBps
	}

	// Liquidation cascade score
	liqScore := d.liquidationScore(obs, recentVol, spreadRatio, bl)

	// Classify
	regime := d.classify(recentVol, trend, spreadRatio, liqScore, bl)

	confidence := math.Min(1, float64(len(obs))/100)

	return RegimeState{
		Regime:           regime,
		VolatilityBps:    recentVol,
		TrendStrength:    trend,
		SpreadWidening:   spreadRatio,
		LiquidationScore: liqScore,
		Confidence:       confidence,
		TsMs:             now,
	}
}

// Adjustment returns strategy parameters for the current regime.
func (d *Detector) Adjustment(symbol string) StrategyAdjustment {
	state := d.Detect(symbol)
	return adjustmentForRegime(state.Regime)
}

func adjustmentForRegime(regime MarketRegime) StrategyAdjustment {
	switch regime {
	case RegimeCalm:
		return StrategyAdjustment{
			Regime:            regime,
			SizingMultiplier:  1.2,
			MinEdgeMultiplier: 0.8,
			MaxPositionMult:   1.2,
			PassiveEnabled:    true,
			AggressivenessLvl: 4,
		}
	case RegimeTrending:
		return StrategyAdjustment{
			Regime:            regime,
			SizingMultiplier:  1.0,
			MinEdgeMultiplier: 1.0,
			MaxPositionMult:   1.0,
			PassiveEnabled:    true,
			AggressivenessLvl: 3,
		}
	case RegimeVolatile:
		return StrategyAdjustment{
			Regime:            regime,
			SizingMultiplier:  0.5,
			MinEdgeMultiplier: 1.5,
			MaxPositionMult:   0.5,
			PassiveEnabled:    false,
			AggressivenessLvl: 2,
		}
	case RegimeCascade:
		return StrategyAdjustment{
			Regime:            regime,
			SizingMultiplier:  0.25,
			MinEdgeMultiplier: 2.0,
			MaxPositionMult:   0.3,
			PassiveEnabled:    false,
			AggressivenessLvl: 1,
		}
	default:
		return StrategyAdjustment{
			Regime:            RegimeCalm,
			SizingMultiplier:  1.0,
			MinEdgeMultiplier: 1.0,
			MaxPositionMult:   1.0,
			PassiveEnabled:    true,
			AggressivenessLvl: 3,
		}
	}
}

func (d *Detector) classify(vol, trend, spreadRatio, liqScore float64, bl *baseline) MarketRegime {
	// Liquidation cascade: high vol + widening spreads + score
	if liqScore > 0.7 && spreadRatio > 2.0 {
		return RegimeCascade
	}

	// Volatile: vol >> baseline
	volRatio := 1.0
	if bl.avgVolBps > 0 {
		volRatio = vol / bl.avgVolBps
	}
	if volRatio > 3.0 || spreadRatio > 2.5 {
		return RegimeVolatile
	}

	// Trending: strong directional move
	if math.Abs(trend) > 0.6 && volRatio > 1.5 {
		return RegimeTrending
	}

	return RegimeCalm
}

func (d *Detector) recentVolatility(obs []Observation, windowMs int64) float64 {
	cutoff := time.Now().UnixMilli() - windowMs
	var returns []float64
	var prevMid float64

	for _, o := range obs {
		if o.TsMs < cutoff {
			prevMid = o.Mid
			continue
		}
		if prevMid > 0 && o.Mid > 0 {
			ret := (o.Mid - prevMid) / prevMid * 10000 // bps
			returns = append(returns, ret)
		}
		prevMid = o.Mid
	}

	if len(returns) < 2 {
		return 0
	}

	// Standard deviation of returns
	var sum, sumSq float64
	for _, r := range returns {
		sum += r
		sumSq += r * r
	}
	n := float64(len(returns))
	mean := sum / n
	variance := sumSq/n - mean*mean
	return math.Sqrt(math.Max(0, variance))
}

func (d *Detector) trendStrength(obs []Observation, windowMs int64) float64 {
	cutoff := time.Now().UnixMilli() - windowMs
	var xs, ys []float64

	for _, o := range obs {
		if o.TsMs < cutoff || o.Mid == 0 {
			continue
		}
		xs = append(xs, float64(o.TsMs-cutoff))
		ys = append(ys, o.Mid)
	}

	n := len(xs)
	if n < 5 {
		return 0
	}

	// Simple linear regression
	var sumX, sumY, sumXY, sumXX float64
	for i := 0; i < n; i++ {
		sumX += xs[i]
		sumY += ys[i]
		sumXY += xs[i] * ys[i]
		sumXX += xs[i] * xs[i]
	}

	nf := float64(n)
	denom := nf*sumXX - sumX*sumX
	if denom == 0 {
		return 0
	}

	slope := (nf*sumXY - sumX*sumY) / denom

	// Normalise slope to -1..+1 range (bps per second)
	avgMid := sumY / nf
	if avgMid == 0 {
		return 0
	}
	slopeBpsPerSec := slope / avgMid * 10000 * 1000 // per second
	return math.Max(-1, math.Min(1, slopeBpsPerSec/10))
}

func (d *Detector) liquidationScore(obs []Observation, vol, spreadRatio float64, bl *baseline) float64 {
	// Cascade indicators:
	// 1. Rapidly increasing volatility (vol >> baseline)
	// 2. Spread widening (spreads blow out)
	// 3. Volume spike (liquidation volume)

	volFactor := 0.0
	if bl.avgVolBps > 0 {
		volFactor = math.Min(1, vol/bl.avgVolBps/5)
	}

	spreadFactor := math.Min(1, (spreadRatio-1)/3) // fully triggered at 4x spread
	if spreadFactor < 0 {
		spreadFactor = 0
	}

	volumeFactor := 0.0
	if bl.avgVolume1m > 0 {
		recent := d.recentAvg(obs, 60000, func(o Observation) float64 { return o.Volume1m })
		volumeFactor = math.Min(1, recent/bl.avgVolume1m/3) // triggered at 3x volume
	}

	return (volFactor*0.4 + spreadFactor*0.35 + volumeFactor*0.25)
}

func (d *Detector) recentAvg(obs []Observation, windowMs int64, extract func(Observation) float64) float64 {
	cutoff := time.Now().UnixMilli() - windowMs
	var sum float64
	var count int
	for _, o := range obs {
		if o.TsMs >= cutoff {
			sum += extract(o)
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func (d *Detector) updateBaseline(symbol string) {
	obs := d.observations[symbol]
	if len(obs) < 50 {
		return
	}

	bl := d.baselines[symbol]
	if bl == nil {
		bl = &baseline{}
		d.baselines[symbol] = bl
	}

	// Use the older 80% of data as baseline (exclude recent 20%)
	cutIdx := len(obs) * 80 / 100
	baseObs := obs[:cutIdx]

	var volSum, spreadSum, volumeSum float64
	var prevMid float64
	var volCount int

	for _, o := range baseObs {
		spreadSum += o.SpreadBps
		volumeSum += o.Volume1m

		if prevMid > 0 && o.Mid > 0 {
			ret := math.Abs((o.Mid - prevMid) / prevMid * 10000)
			volSum += ret
			volCount++
		}
		prevMid = o.Mid
	}

	n := float64(len(baseObs))
	if n > 0 {
		bl.avgSpreadBps = spreadSum / n
		bl.avgVolume1m = volumeSum / n
		bl.sampleCount = len(baseObs)
		bl.lastUpdateMs = time.Now().UnixMilli()
	}
	if volCount > 0 {
		bl.avgVolBps = volSum / float64(volCount)
	}
}

func (d *Detector) trimOld(symbol string) {
	cutoff := time.Now().UnixMilli() - d.windowMs
	obs := d.observations[symbol]
	start := 0
	for start < len(obs) && obs[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		d.observations[symbol] = obs[start:]
	}
	if len(d.observations[symbol]) > d.maxObs {
		d.observations[symbol] = d.observations[symbol][len(d.observations[symbol])-d.maxObs:]
	}
}
