package arb

import (
	"fmt"
	"math"
	"sync"
	"time"
)

const StrategyLiquidityMirror = "LIQUIDITY_MIRROR"

// MirrorConfig controls liquidity mirroring behaviour.
type MirrorConfig struct {
	Enabled          bool    `yaml:"enabled"`
	MinFlowSizeUSD   float64 `yaml:"min_flow_size_usd"`     // minimum whale flow to mirror
	MaxDelayMs       int64   `yaml:"max_delay_ms"`           // max delay after detecting flow
	NotionalUSD      float64 `yaml:"notional_usd"`           // our position size
	MaxSlippageBps   float64 `yaml:"max_slippage_bps"`
	IntentTTLMs      int64   `yaml:"intent_ttl_ms"`
	CooldownMs       int64   `yaml:"cooldown_ms"`
	MinEdgeBps       float64 `yaml:"min_edge_bps"`           // minimum expected edge
	MaxMirrorPct     float64 `yaml:"max_mirror_pct"`         // max % of detected flow to mirror
}

// WhaleFlow represents a detected large trade or order book event.
type WhaleFlow struct {
	Venue        string  `json:"venue"`
	Symbol       string  `json:"symbol"`
	Direction    string  `json:"direction"` // BUY or SELL
	SizeUSD      float64 `json:"size_usd"`
	PriceImpact  float64 `json:"price_impact_bps"` // observed impact in bps
	TsMs         int64   `json:"ts_ms"`
}

// LiquidityMirror detects and mirrors large institutional/whale order flow
// by tracking aggressive volume spikes and predictable patterns.
type LiquidityMirror struct {
	mu       sync.Mutex
	flows    map[string][]WhaleFlow // symbol → detected whale flows
	patterns map[string]*flowPattern // symbol:venue → detected patterns
	windowMs int64
	maxFlows int
}

type flowPattern struct {
	avgIntervalMs int64   // average time between repeated flows
	avgSizeUSD    float64 // average flow size
	direction     string  // dominant direction
	count         int     // number of similar flows detected
	lastSeenMs    int64
}

// NewLiquidityMirror creates a liquidity mirroring engine.
func NewLiquidityMirror(windowMs int64) *LiquidityMirror {
	return &LiquidityMirror{
		flows:    make(map[string][]WhaleFlow),
		patterns: make(map[string]*flowPattern),
		windowMs: windowMs,
		maxFlows: 1000,
	}
}

// RecordFlow records a detected large trade.
func (lm *LiquidityMirror) RecordFlow(flow WhaleFlow) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	lm.flows[flow.Symbol] = append(lm.flows[flow.Symbol], flow)
	lm.trimFlows(flow.Symbol)
	lm.updatePatterns(flow)
}

// Evaluate checks for mirroring opportunities.
func (lm *LiquidityMirror) Evaluate(cfg MirrorConfig, tenantID string, cooldown *Cooldown) []TradeIntent {
	if !cfg.Enabled {
		return nil
	}

	lm.mu.Lock()
	defer lm.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for symbol, flows := range lm.flows {
		// Look at recent flows within the delay window
		for _, flow := range flows {
			if now-flow.TsMs > cfg.MaxDelayMs {
				continue // too old
			}
			if flow.SizeUSD < cfg.MinFlowSizeUSD {
				continue
			}

			cooldownKey := fmt.Sprintf("mirror:%s:%s:%s", symbol, flow.Venue, flow.Direction)
			if cooldown != nil && cooldown.IsOnCooldown(cooldownKey, now) {
				continue
			}

			// Check for repeating pattern
			patternKey := symbol + ":" + flow.Venue
			pattern := lm.patterns[patternKey]
			edgeBps := flow.PriceImpact * 0.3 // expect 30% of impact as followthrough

			if pattern != nil && pattern.count >= 3 {
				// Strong pattern: higher confidence, higher edge
				edgeBps = flow.PriceImpact * 0.5
			}

			if edgeBps < cfg.MinEdgeBps {
				continue
			}

			// Size: mirror a portion of the flow, capped
			mirrorSize := math.Min(cfg.NotionalUSD, flow.SizeUSD*cfg.MaxMirrorPct/100)
			if mirrorSize < 100 {
				continue
			}

			intent := TradeIntent{
				TenantID:  tenantID,
				IntentID:  newUUID(),
				Strategy:  StrategyLiquidityMirror,
				Symbol:    symbol,
				TsMs:      now,
				ExpiresMs: now + cfg.IntentTTLMs,
				Legs: []TradeLeg{{
					Venue:          flow.Venue,
					Action:         flow.Direction,
					Market:         "PERP",
					NotionalUSD:    mirrorSize,
					MaxSlippageBps: cfg.MaxSlippageBps,
				}},
				Expected: ExpectedMetrics{
					EdgeBpsGross: edgeBps,
					EdgeBpsNet:   edgeBps - 4, // assume ~4 bps in fees
					ProfitUSDNet: mirrorSize * (edgeBps - 4) / 10000,
				},
				Constraints: IntentConstraints{
					MinQuality:  "MED",
					MaxAgeMs:    cfg.IntentTTLMs,
					CooldownKey: cooldownKey,
				},
				Debug: IntentDebug{
					BuyOn:  flow.Venue,
					SellOn: flow.Venue,
				},
			}

			intents = append(intents, intent)
			if cooldown != nil {
				cooldown.Mark(cooldownKey, now)
			}
		}
	}

	return intents
}

// DetectedPatterns returns all detected flow patterns.
func (lm *LiquidityMirror) DetectedPatterns() map[string]*flowPattern {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	result := make(map[string]*flowPattern, len(lm.patterns))
	for k, v := range lm.patterns {
		cp := *v
		result[k] = &cp
	}
	return result
}

func (lm *LiquidityMirror) updatePatterns(flow WhaleFlow) {
	key := flow.Symbol + ":" + flow.Venue

	pat, ok := lm.patterns[key]
	if !ok {
		lm.patterns[key] = &flowPattern{
			direction:  flow.Direction,
			avgSizeUSD: flow.SizeUSD,
			count:      1,
			lastSeenMs: flow.TsMs,
		}
		return
	}

	if flow.Direction != pat.direction {
		// Direction changed — reset pattern
		pat.direction = flow.Direction
		pat.count = 1
		pat.avgSizeUSD = flow.SizeUSD
		pat.lastSeenMs = flow.TsMs
		return
	}

	// Update rolling averages
	interval := flow.TsMs - pat.lastSeenMs
	if pat.count > 0 && pat.avgIntervalMs > 0 {
		pat.avgIntervalMs = (pat.avgIntervalMs*int64(pat.count) + interval) / int64(pat.count+1)
	} else {
		pat.avgIntervalMs = interval
	}

	pat.avgSizeUSD = (pat.avgSizeUSD*float64(pat.count) + flow.SizeUSD) / float64(pat.count+1)
	pat.count++
	pat.lastSeenMs = flow.TsMs
}

func (lm *LiquidityMirror) trimFlows(symbol string) {
	cutoff := time.Now().UnixMilli() - lm.windowMs
	flows := lm.flows[symbol]
	start := 0
	for start < len(flows) && flows[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		lm.flows[symbol] = flows[start:]
	}
	if len(lm.flows[symbol]) > lm.maxFlows {
		lm.flows[symbol] = lm.flows[symbol][len(lm.flows[symbol])-lm.maxFlows:]
	}
}
