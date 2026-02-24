package orderbook

import (
	"math"
	"sync"
	"time"
)

// FlowSample represents a single observation of order flow activity.
type FlowSample struct {
	TsMs             int64   `json:"ts_ms"`
	Venue            string  `json:"venue"`
	Symbol           string  `json:"symbol"`
	AggressiveBuyQty float64 `json:"aggressive_buy_qty"`  // market buy volume
	AggressiveSellQty float64 `json:"aggressive_sell_qty"` // market sell volume
	BidDepthDelta    float64 `json:"bid_depth_delta"`     // change in bid-side depth
	AskDepthDelta    float64 `json:"ask_depth_delta"`     // change in ask-side depth
}

// LiquidityPressure is the output of the flow detector.
type LiquidityPressure struct {
	Symbol           string  `json:"symbol"`
	Score            float64 `json:"score"`              // -1.0 (sell pressure) to +1.0 (buy pressure)
	AggressiveBuyPct float64 `json:"aggressive_buy_pct"` // % of volume that's aggressive buy
	DepletionRate    float64 `json:"depletion_rate"`     // how fast book is thinning
	ReplenishRate    float64 `json:"replenish_rate"`     // how fast book is refilling
	Confidence       float64 `json:"confidence"`         // 0..1 how reliable the signal is
	TsMs             int64   `json:"ts_ms"`
}

// FlowDetector tracks aggressive order flow and book dynamics to produce
// a liquidity pressure score predicting short-term price direction.
type FlowDetector struct {
	mu       sync.RWMutex
	samples  map[string][]FlowSample // symbol → recent samples
	windowMs int64                   // lookback window
	maxSamples int                   // cap per symbol
}

// NewFlowDetector creates a flow detector with the given lookback window.
func NewFlowDetector(windowMs int64) *FlowDetector {
	return &FlowDetector{
		samples:    make(map[string][]FlowSample),
		windowMs:   windowMs,
		maxSamples: 5000,
	}
}

// Record adds a flow sample.
func (fd *FlowDetector) Record(s FlowSample) {
	fd.mu.Lock()
	defer fd.mu.Unlock()

	fd.samples[s.Symbol] = append(fd.samples[s.Symbol], s)

	// Trim old + enforce cap
	cutoff := time.Now().UnixMilli() - fd.windowMs
	samples := fd.samples[s.Symbol]
	start := 0
	for start < len(samples) && samples[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		fd.samples[s.Symbol] = samples[start:]
	}
	if len(fd.samples[s.Symbol]) > fd.maxSamples {
		fd.samples[s.Symbol] = fd.samples[s.Symbol][len(fd.samples[s.Symbol])-fd.maxSamples:]
	}
}

// Pressure computes the current liquidity pressure for a symbol.
func (fd *FlowDetector) Pressure(symbol string) LiquidityPressure {
	fd.mu.RLock()
	defer fd.mu.RUnlock()

	samples := fd.samples[symbol]
	now := time.Now().UnixMilli()
	cutoff := now - fd.windowMs

	if len(samples) == 0 {
		return LiquidityPressure{Symbol: symbol, TsMs: now}
	}

	var totalBuyQty, totalSellQty float64
	var bidDepletions, askDepletions float64
	var bidReplenish, askReplenish float64
	count := 0

	for _, s := range samples {
		if s.TsMs < cutoff {
			continue
		}
		count++
		totalBuyQty += s.AggressiveBuyQty
		totalSellQty += s.AggressiveSellQty

		if s.BidDepthDelta < 0 {
			bidDepletions += math.Abs(s.BidDepthDelta)
		} else {
			bidReplenish += s.BidDepthDelta
		}
		if s.AskDepthDelta < 0 {
			askDepletions += math.Abs(s.AskDepthDelta)
		} else {
			askReplenish += s.AskDepthDelta
		}
	}

	totalVol := totalBuyQty + totalSellQty
	if totalVol == 0 || count == 0 {
		return LiquidityPressure{Symbol: symbol, TsMs: now}
	}

	// Aggressive flow imbalance: +1 = all buy, -1 = all sell
	flowImbalance := (totalBuyQty - totalSellQty) / totalVol

	// Book depletion imbalance: positive = ask side thinning (bullish)
	depTotal := bidDepletions + askDepletions
	depImbalance := 0.0
	if depTotal > 0 {
		depImbalance = (askDepletions - bidDepletions) / depTotal
	}

	// Combined score (60% flow, 40% depth dynamics)
	score := 0.6*flowImbalance + 0.4*depImbalance
	score = math.Max(-1, math.Min(1, score))

	aggressiveBuyPct := totalBuyQty / totalVol * 100

	// Depletion rate: how fast the total book is thinning (normalised)
	windowSec := float64(fd.windowMs) / 1000
	depletionRate := depTotal / windowSec
	replenishRate := (bidReplenish + askReplenish) / windowSec

	// Confidence based on sample count and volume
	confidence := math.Min(1, float64(count)/20) * math.Min(1, totalVol/10)

	return LiquidityPressure{
		Symbol:           symbol,
		Score:            score,
		AggressiveBuyPct: aggressiveBuyPct,
		DepletionRate:    depletionRate,
		ReplenishRate:    replenishRate,
		Confidence:       confidence,
		TsMs:             now,
	}
}
