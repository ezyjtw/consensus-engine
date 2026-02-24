// Package fairvalue provides a latency-adjusted fair value engine that computes
// dynamic venue weights based on leadership probability, depth, latency, and
// reliability — replacing naive mid-price averaging.
package fairvalue

import (
	"math"
	"sync"
	"time"
)

// VenueInput is the real-time data used to compute fair value.
type VenueInput struct {
	Venue       string  `json:"venue"`
	Mid         float64 `json:"mid"`
	BestBid     float64 `json:"best_bid"`
	BestAsk     float64 `json:"best_ask"`
	DepthUSD    float64 `json:"depth_usd"`    // total depth in USD
	LatencyUs   int64   `json:"latency_us"`   // feed latency
	LeadPct     float64 `json:"lead_pct"`     // % of moves this venue leads
	Reliability float64 `json:"reliability"`  // 0..1 consistency
	TsMs        int64   `json:"ts_ms"`
}

// FairValue is the output of the engine.
type FairValue struct {
	Symbol         string             `json:"symbol"`
	Mid            float64            `json:"mid"`
	BuyExec        float64            `json:"buy_exec"`   // fair executable buy
	SellExec       float64            `json:"sell_exec"`  // fair executable sell
	Confidence     float64            `json:"confidence"` // 0..1
	VenueWeights   []VenueWeight      `json:"venue_weights"`
	TsMs           int64              `json:"ts_ms"`
}

// VenueWeight describes the contribution of a venue to fair value.
type VenueWeight struct {
	Venue  string  `json:"venue"`
	Weight float64 `json:"weight"` // normalised 0..1
	Mid    float64 `json:"mid"`
}

// Config controls the fair value engine behaviour.
type Config struct {
	StaleMs           int64              `yaml:"stale_ms"`            // max quote age
	MinVenues         int                `yaml:"min_venues"`          // minimum for valid FV
	DepthRefUSD       float64            `yaml:"depth_ref_usd"`       // reference depth for normalisation
	LatencyRefUs      int64              `yaml:"latency_ref_us"`      // reference latency (microseconds)
	LeadershipAlpha   float64            `yaml:"leadership_alpha"`    // weight of leadership in scoring
	DepthAlpha        float64            `yaml:"depth_alpha"`         // weight of depth
	LatencyAlpha      float64            `yaml:"latency_alpha"`       // weight of latency
	ReliabilityAlpha  float64            `yaml:"reliability_alpha"`   // weight of reliability
	EWADecay          float64            `yaml:"ewa_decay"`           // exponential weight decay (0..1)
	BaseWeights       map[string]float64 `yaml:"base_weights"`        // static venue weights
}

// DefaultConfig returns a sensible default configuration.
func DefaultConfig() Config {
	return Config{
		StaleMs:          2000,
		MinVenues:        2,
		DepthRefUSD:      500000,
		LatencyRefUs:     100000, // 100ms
		LeadershipAlpha:  0.35,
		DepthAlpha:       0.25,
		LatencyAlpha:     0.20,
		ReliabilityAlpha: 0.20,
		EWADecay:         0.95,
		BaseWeights: map[string]float64{
			"binance": 1.0,
			"okx":     0.9,
			"bybit":   0.85,
			"deribit": 0.80,
		},
	}
}

// Engine computes latency-adjusted fair value with dynamic weights.
type Engine struct {
	mu     sync.RWMutex
	cfg    Config
	ewa    map[string]map[string]float64 // symbol → venue → EWA weight
}

// NewEngine creates a fair value engine.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg: cfg,
		ewa: make(map[string]map[string]float64),
	}
}

// Compute produces a fair value estimate for a symbol from venue inputs.
func (e *Engine) Compute(symbol string, inputs []VenueInput) *FairValue {
	now := time.Now().UnixMilli()

	// Filter stale inputs
	var fresh []VenueInput
	for _, inp := range inputs {
		if now-inp.TsMs <= e.cfg.StaleMs {
			fresh = append(fresh, inp)
		}
	}

	if len(fresh) < e.cfg.MinVenues {
		return nil
	}

	// Compute raw scores per venue
	rawScores := make(map[string]float64)
	var totalRaw float64
	for _, inp := range fresh {
		score := e.venueScore(inp)
		rawScores[inp.Venue] = score
		totalRaw += score
	}

	if totalRaw == 0 {
		return nil
	}

	// Normalise to weights
	weights := make(map[string]float64, len(rawScores))
	for v, s := range rawScores {
		weights[v] = s / totalRaw
	}

	// Apply EWA smoothing
	e.mu.Lock()
	if e.ewa[symbol] == nil {
		e.ewa[symbol] = make(map[string]float64)
	}
	decay := e.cfg.EWADecay
	for v, w := range weights {
		prev, exists := e.ewa[symbol][v]
		if exists {
			e.ewa[symbol][v] = decay*prev + (1-decay)*w
		} else {
			e.ewa[symbol][v] = w
		}
		weights[v] = e.ewa[symbol][v]
	}
	e.mu.Unlock()

	// Re-normalise after EWA
	totalW := 0.0
	for _, w := range weights {
		totalW += w
	}
	for v := range weights {
		weights[v] /= totalW
	}

	// Compute weighted fair value
	var fvMid, fvBuy, fvSell float64
	var venueWeights []VenueWeight

	for _, inp := range fresh {
		w := weights[inp.Venue]
		fvMid += w * inp.Mid
		fvBuy += w * inp.BestAsk
		fvSell += w * inp.BestBid
		venueWeights = append(venueWeights, VenueWeight{
			Venue:  inp.Venue,
			Weight: w,
			Mid:    inp.Mid,
		})
	}

	// Confidence: based on venue count, agreement, and depth
	confidence := e.computeConfidence(fresh, fvMid)

	return &FairValue{
		Symbol:       symbol,
		Mid:          fvMid,
		BuyExec:      fvBuy,
		SellExec:     fvSell,
		Confidence:   confidence,
		VenueWeights: venueWeights,
		TsMs:         now,
	}
}

// venueScore computes the raw quality score for a venue.
func (e *Engine) venueScore(inp VenueInput) float64 {
	cfg := e.cfg

	// Base weight
	base := cfg.BaseWeights[inp.Venue]
	if base == 0 {
		base = 0.1
	}

	// Leadership factor: higher lead% = more weight
	leaderFactor := inp.LeadPct / 100
	if leaderFactor < 0.01 {
		leaderFactor = 0.01
	}

	// Depth factor: normalised to reference depth
	depthFactor := math.Min(2.0, inp.DepthUSD/cfg.DepthRefUSD)
	if depthFactor < 0.01 {
		depthFactor = 0.01
	}

	// Latency factor: lower latency = higher weight (inverse)
	latencyFactor := 1.0
	if inp.LatencyUs > 0 {
		latencyFactor = float64(cfg.LatencyRefUs) / float64(inp.LatencyUs)
		latencyFactor = math.Min(2.0, math.Max(0.1, latencyFactor))
	}

	// Reliability factor
	relFactor := math.Max(0.1, inp.Reliability)

	// Weighted combination
	score := base * (cfg.LeadershipAlpha*leaderFactor +
		cfg.DepthAlpha*depthFactor +
		cfg.LatencyAlpha*latencyFactor +
		cfg.ReliabilityAlpha*relFactor)

	return math.Max(0.001, score)
}

// computeConfidence estimates how reliable the fair value is.
func (e *Engine) computeConfidence(inputs []VenueInput, fvMid float64) float64 {
	n := len(inputs)
	if n == 0 || fvMid == 0 {
		return 0
	}

	// Factor 1: venue count (more = better)
	venueFactor := math.Min(1.0, float64(n)/4)

	// Factor 2: agreement (low deviation = better)
	var sumDevSq float64
	for _, inp := range inputs {
		dev := (inp.Mid - fvMid) / fvMid * 10000
		sumDevSq += dev * dev
	}
	rmsDev := math.Sqrt(sumDevSq / float64(n))
	agreementFactor := math.Max(0, 1-rmsDev/50) // 50 bps → 0 confidence

	// Factor 3: total depth
	var totalDepth float64
	for _, inp := range inputs {
		totalDepth += inp.DepthUSD
	}
	depthFactor := math.Min(1.0, totalDepth/1000000)

	return venueFactor * 0.4 + agreementFactor * 0.4 + depthFactor * 0.2
}
