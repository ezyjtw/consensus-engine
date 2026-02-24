// Package prediction provides an opportunity prediction engine that forecasts
// spread widening, venue dislocation, and arbitrage opportunities before they
// fully materialise — allowing pre-positioning and faster execution.
package prediction

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Signal represents a predicted opportunity.
type Signal struct {
	Symbol         string       `json:"symbol"`
	Type           SignalType   `json:"type"`
	Direction      string       `json:"direction"` // BUY, SELL, or NEUTRAL
	Strength       float64      `json:"strength"`  // 0..1
	Confidence     float64      `json:"confidence"` // 0..1
	ExpectedEdgeBps float64     `json:"expected_edge_bps"`
	LeadVenue      string       `json:"lead_venue"`
	TargetVenues   []string     `json:"target_venues"`
	DecayMs        int64        `json:"decay_ms"` // signal validity window
	TsMs           int64        `json:"ts_ms"`
}

// SignalType classifies the predicted opportunity.
type SignalType string

const (
	SignalLeaderMove   SignalType = "LEADER_MOVE"    // leader venue moved, followers haven't yet
	SignalSpreadWiden  SignalType = "SPREAD_WIDEN"    // spread predicted to widen (arb opportunity coming)
	SignalFlowImbal    SignalType = "FLOW_IMBALANCE"  // aggressive flow will push price
	SignalDepthDrain   SignalType = "DEPTH_DRAIN"     // book thinning, spread likely to blow out
	SignalConvergence  SignalType = "CONVERGENCE"     // prices currently diverged, expected to converge
)

// PredictionInput provides the real-time data needed for prediction.
type PredictionInput struct {
	Symbol          string             `json:"symbol"`
	VenueMids       map[string]float64 `json:"venue_mids"`       // venue → current mid
	VenueSpreads    map[string]float64 `json:"venue_spreads"`    // venue → spread in bps
	VenueDepths     map[string]float64 `json:"venue_depths"`     // venue → depth in USD
	FlowScore       float64            `json:"flow_score"`       // -1..+1 from flow detector
	LeaderVenue     string             `json:"leader_venue"`     // current leader
	LeaderMoveBps   float64            `json:"leader_move_bps"`  // last leader move size
	VolatilityBps   float64            `json:"volatility_bps"`   // current vol
	TsMs            int64              `json:"ts_ms"`
}

// Engine predicts opportunities from real-time market intelligence.
type Engine struct {
	mu          sync.RWMutex
	history     map[string][]PredictionInput // symbol → recent inputs
	signals     map[string][]Signal          // symbol → active signals
	windowMs    int64
	maxHistory  int
}

// NewEngine creates a prediction engine.
func NewEngine(windowMs int64) *Engine {
	return &Engine{
		history:    make(map[string][]PredictionInput),
		signals:    make(map[string][]Signal),
		windowMs:   windowMs,
		maxHistory: 2000,
	}
}

// Predict produces opportunity signals from the latest market state.
func (e *Engine) Predict(input PredictionInput) []Signal {
	e.mu.Lock()
	e.history[input.Symbol] = append(e.history[input.Symbol], input)
	e.trimHistory(input.Symbol)
	e.mu.Unlock()

	var signals []Signal

	// 1. Leader move detection
	if s := e.detectLeaderMove(input); s != nil {
		signals = append(signals, *s)
	}

	// 2. Spread widening prediction
	if s := e.detectSpreadWiden(input); s != nil {
		signals = append(signals, *s)
	}

	// 3. Flow imbalance
	if s := e.detectFlowImbalance(input); s != nil {
		signals = append(signals, *s)
	}

	// 4. Depth drain
	if s := e.detectDepthDrain(input); s != nil {
		signals = append(signals, *s)
	}

	// 5. Price convergence
	if s := e.detectConvergence(input); s != nil {
		signals = append(signals, *s)
	}

	// Store active signals
	e.mu.Lock()
	e.signals[input.Symbol] = signals
	e.mu.Unlock()

	return signals
}

// ActiveSignals returns current active signals for a symbol.
func (e *Engine) ActiveSignals(symbol string) []Signal {
	e.mu.RLock()
	defer e.mu.RUnlock()

	now := time.Now().UnixMilli()
	var active []Signal
	for _, s := range e.signals[symbol] {
		if now-s.TsMs < s.DecayMs {
			active = append(active, s)
		}
	}
	return active
}

func (e *Engine) detectLeaderMove(input PredictionInput) *Signal {
	if input.LeaderVenue == "" || math.Abs(input.LeaderMoveBps) < 2.0 {
		return nil
	}

	leaderMid := input.VenueMids[input.LeaderVenue]
	if leaderMid == 0 {
		return nil
	}

	// Find venues that haven't followed yet
	var targets []string
	var totalGap float64
	for venue, mid := range input.VenueMids {
		if venue == input.LeaderVenue || mid == 0 {
			continue
		}
		gapBps := (leaderMid - mid) / mid * 10000
		if math.Abs(gapBps) > 1.5 { // significant gap
			targets = append(targets, venue)
			totalGap += math.Abs(gapBps)
		}
	}

	if len(targets) == 0 {
		return nil
	}

	avgGap := totalGap / float64(len(targets))
	strength := math.Min(1, avgGap/10)

	direction := "BUY"
	if input.LeaderMoveBps < 0 {
		direction = "SELL"
	}

	return &Signal{
		Symbol:          input.Symbol,
		Type:            SignalLeaderMove,
		Direction:       direction,
		Strength:        strength,
		Confidence:      math.Min(0.9, strength*0.8),
		ExpectedEdgeBps: avgGap * 0.5, // expect to capture 50% of gap
		LeadVenue:       input.LeaderVenue,
		TargetVenues:    targets,
		DecayMs:         2000,
		TsMs:            input.TsMs,
	}
}

func (e *Engine) detectSpreadWiden(input PredictionInput) *Signal {
	e.mu.RLock()
	history := e.history[input.Symbol]
	e.mu.RUnlock()

	if len(history) < 10 {
		return nil
	}

	// Compute average spread over last 30 seconds
	cutoff := input.TsMs - 30000
	var baselineSpread float64
	var count int
	for _, h := range history {
		if h.TsMs < cutoff {
			for _, s := range h.VenueSpreads {
				baselineSpread += s
				count++
			}
		}
	}
	if count == 0 {
		return nil
	}
	baselineSpread /= float64(count)

	// Current average spread
	var currentSpread float64
	for _, s := range input.VenueSpreads {
		currentSpread += s
	}
	if len(input.VenueSpreads) > 0 {
		currentSpread /= float64(len(input.VenueSpreads))
	}

	ratio := currentSpread / baselineSpread
	if ratio < 1.5 {
		return nil
	}

	strength := math.Min(1, (ratio-1)/3)
	edgeBps := (currentSpread - baselineSpread) / 2

	return &Signal{
		Symbol:          input.Symbol,
		Type:            SignalSpreadWiden,
		Direction:       "NEUTRAL",
		Strength:        strength,
		Confidence:      math.Min(0.8, strength*0.7),
		ExpectedEdgeBps: edgeBps,
		DecayMs:         5000,
		TsMs:            input.TsMs,
	}
}

func (e *Engine) detectFlowImbalance(input PredictionInput) *Signal {
	if math.Abs(input.FlowScore) < 0.4 {
		return nil
	}

	strength := math.Abs(input.FlowScore)
	direction := "BUY"
	if input.FlowScore < 0 {
		direction = "SELL"
	}

	return &Signal{
		Symbol:          input.Symbol,
		Type:            SignalFlowImbal,
		Direction:       direction,
		Strength:        strength,
		Confidence:      strength * 0.6,
		ExpectedEdgeBps: strength * 5,
		DecayMs:         3000,
		TsMs:            input.TsMs,
	}
}

func (e *Engine) detectDepthDrain(input PredictionInput) *Signal {
	e.mu.RLock()
	history := e.history[input.Symbol]
	e.mu.RUnlock()

	if len(history) < 10 {
		return nil
	}

	// Check if depth has been decreasing
	cutoff := input.TsMs - 30000
	var prevDepths, currDepths []float64

	for _, h := range history {
		var total float64
		for _, d := range h.VenueDepths {
			total += d
		}
		if h.TsMs < cutoff {
			prevDepths = append(prevDepths, total)
		} else {
			currDepths = append(currDepths, total)
		}
	}

	if len(prevDepths) == 0 || len(currDepths) == 0 {
		return nil
	}

	avgPrev := avg(prevDepths)
	avgCurr := avg(currDepths)

	if avgPrev == 0 {
		return nil
	}

	drainPct := (avgPrev - avgCurr) / avgPrev * 100
	if drainPct < 30 { // depth dropped by less than 30%
		return nil
	}

	strength := math.Min(1, drainPct/80)

	return &Signal{
		Symbol:          input.Symbol,
		Type:            SignalDepthDrain,
		Direction:       "NEUTRAL",
		Strength:        strength,
		Confidence:      math.Min(0.7, strength*0.6),
		ExpectedEdgeBps: strength * 8,
		DecayMs:         5000,
		TsMs:            input.TsMs,
	}
}

func (e *Engine) detectConvergence(input PredictionInput) *Signal {
	if len(input.VenueMids) < 2 {
		return nil
	}

	// Find max divergence between venues
	var mids []float64
	var venues []string
	for v, m := range input.VenueMids {
		if m > 0 {
			mids = append(mids, m)
			venues = append(venues, v)
		}
	}

	if len(mids) < 2 {
		return nil
	}

	sort.Float64s(mids)
	maxDivBps := (mids[len(mids)-1] - mids[0]) / mids[0] * 10000

	if maxDivBps < 3.0 { // minimum divergence
		return nil
	}

	// Find the diverged venues
	medMid := mids[len(mids)/2]
	var targets []string
	for v, m := range input.VenueMids {
		devBps := math.Abs(m-medMid) / medMid * 10000
		if devBps > 2.0 {
			targets = append(targets, v)
		}
	}

	strength := math.Min(1, maxDivBps/15)

	return &Signal{
		Symbol:          input.Symbol,
		Type:            SignalConvergence,
		Direction:       "NEUTRAL",
		Strength:        strength,
		Confidence:      math.Min(0.85, strength*0.8),
		ExpectedEdgeBps: maxDivBps * 0.4, // expect 40% capture
		TargetVenues:    targets,
		DecayMs:         3000,
		TsMs:            input.TsMs,
	}
}

func (e *Engine) trimHistory(symbol string) {
	cutoff := time.Now().UnixMilli() - e.windowMs
	hist := e.history[symbol]
	start := 0
	for start < len(hist) && hist[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		e.history[symbol] = hist[start:]
	}
	if len(e.history[symbol]) > e.maxHistory {
		e.history[symbol] = e.history[symbol][len(e.history[symbol])-e.maxHistory:]
	}
}

func avg(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}
