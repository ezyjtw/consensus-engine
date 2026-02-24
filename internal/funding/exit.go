package funding

import (
	"fmt"
	"log"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
)

const (
	StrategyFundingCarryExit = "FUNDING_CARRY_EXIT"
	StrategyFundingDiffExit  = "FUNDING_DIFFERENTIAL_EXIT"
)

// ExitReason describes why an exit signal was generated.
type ExitReason string

const (
	ExitFundingInverted  ExitReason = "FUNDING_INVERTED"    // rate flipped sign
	ExitVolatilitySpike  ExitReason = "VOLATILITY_SPIKE"    // 24h vol > threshold
	ExitRegimeVolatile   ExitReason = "REGIME_VOLATILE"     // regime shifted to VOLATILE
	ExitNearReset        ExitReason = "NEAR_RESET_NEGATIVE" // negative rate near funding reset
)

// OpenPosition represents a tracked open funding position.
// Populated from execution events flowing back through Redis.
type OpenPosition struct {
	Strategy    string
	Symbol      string
	Venue       string    // for carry; empty for differential
	LongVenue   string    // for differential
	ShortVenue  string    // for differential
	NotionalUSD float64
	EntryRateBps float64  // funding rate at entry
	EntryTsMs   int64
}

// EvaluateExits checks all open positions and returns unwind intents for any
// that should be closed based on current market conditions.
func (e *Engine) EvaluateExits(tenantID string, positions []OpenPosition) []arb.TradeIntent {
	e.mu.RLock()
	defer e.mu.RUnlock()

	now := time.Now()
	nowMs := now.UnixMilli()
	var intents []arb.TradeIntent

	for _, pos := range positions {
		switch pos.Strategy {
		case StrategyFundingCarry:
			if intent := e.evaluateCarryExit(tenantID, pos, now, nowMs); intent != nil {
				intents = append(intents, *intent)
			}
		case StrategyFundingDifferential:
			if intent := e.evaluateDiffExit(tenantID, pos, now, nowMs); intent != nil {
				intents = append(intents, *intent)
			}
		}
	}
	return intents
}

func (e *Engine) evaluateCarryExit(tenantID string, pos OpenPosition, now time.Time, nowMs int64) *arb.TradeIntent {
	venueMap, ok := e.state[pos.Symbol]
	if !ok {
		return nil
	}
	d, ok := venueMap[pos.Venue]
	if !ok {
		return nil
	}

	var reason ExitReason

	// Check 1: Funding rate inverted (was collecting, now paying).
	if pos.EntryRateBps > 0 && d.fundingRate < 0 {
		reason = ExitFundingInverted
	}

	// Check 2: Regime went VOLATILE.
	if reason == "" {
		if r := e.forecaster.Get(pos.Venue, pos.Symbol); r != nil && r.Label == "VOLATILE" {
			reason = ExitRegimeVolatile
		}
	}

	// Check 3: Volatility spike above threshold.
	if reason == "" && e.volTracker != nil && e.policy.VolatilityGate.VolThresholdPct > 0 {
		vol := e.volTracker.RealizedVol(pos.Venue, pos.Symbol)
		if vol > e.policy.VolatilityGate.VolThresholdPct {
			reason = ExitVolatilitySpike
		}
	}

	// Check 4: Negative funding near reset — will pay instead of collect.
	if reason == "" && e.scheduler != nil && d.fundingRate < 0 {
		if e.scheduler.IsNearReset(now, 30*time.Minute) {
			reason = ExitNearReset
		}
	}

	if reason == "" {
		return nil
	}

	log.Printf("funding: EXIT signal sym=%s venue=%s reason=%s rate=%.6f",
		pos.Symbol, pos.Venue, reason, d.fundingRate)
	e.Emitted[StrategyFundingCarryExit]++

	// Unwind carry: sell spot + buy-to-close perp short.
	return &arb.TradeIntent{
		TenantID:  tenantID,
		IntentID:  newUUID(),
		Strategy:  StrategyFundingCarryExit,
		Symbol:    pos.Symbol,
		TsMs:      nowMs,
		ExpiresMs: nowMs + e.policy.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Venue:          pos.Venue,
				Action:         "SELL",
				Market:         "SPOT",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    pos.NotionalUSD,
				MaxSlippageBps: e.policy.MaxSlippageBps,
				PriceLimit:     d.markPrice * 0.995,
			},
			{
				Venue:          pos.Venue,
				Action:         "BUY",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    pos.NotionalUSD,
				MaxSlippageBps: e.policy.MaxSlippageBps,
				PriceLimit:     d.markPrice * 1.005,
			},
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsNet: 0, // exit, not edge-seeking
		},
		Constraints: arb.IntentConstraints{
			MinQuality:      "LOW",
			RequireVenueOK:  false, // exit even on degraded venues
			MaxAgeMs:        e.policy.IntentTTLMs,
			HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
			CooldownKey:     fmt.Sprintf("exit:carry:%s:%s", pos.Symbol, pos.Venue),
		},
	}
}

func (e *Engine) evaluateDiffExit(tenantID string, pos OpenPosition, now time.Time, nowMs int64) *arb.TradeIntent {
	venueMap, ok := e.state[pos.Symbol]
	if !ok {
		return nil
	}
	dLong, okL := venueMap[pos.LongVenue]
	dShort, okS := venueMap[pos.ShortVenue]
	if !okL || !okS {
		return nil
	}

	var reason ExitReason

	// Check 1: Differential inverted (the venue we're short now has lower rate).
	diffBps := (dShort.fundingRate - dLong.fundingRate) * 10000
	if diffBps < 0 {
		reason = ExitFundingInverted
	}

	// Check 2: Either venue went VOLATILE.
	if reason == "" {
		rL := e.forecaster.Get(pos.LongVenue, pos.Symbol)
		rS := e.forecaster.Get(pos.ShortVenue, pos.Symbol)
		if (rL != nil && rL.Label == "VOLATILE") || (rS != nil && rS.Label == "VOLATILE") {
			reason = ExitRegimeVolatile
		}
	}

	if reason == "" {
		return nil
	}

	log.Printf("funding: EXIT signal sym=%s long=%s short=%s reason=%s diff=%.2fbps",
		pos.Symbol, pos.LongVenue, pos.ShortVenue, reason, diffBps)
	e.Emitted[StrategyFundingDiffExit]++

	midPrice := (dLong.markPrice + dShort.markPrice) / 2

	// Unwind differential: sell long-venue perp, buy short-venue perp.
	return &arb.TradeIntent{
		TenantID:  tenantID,
		IntentID:  newUUID(),
		Strategy:  StrategyFundingDiffExit,
		Symbol:    pos.Symbol,
		TsMs:      nowMs,
		ExpiresMs: nowMs + e.policy.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Venue:          pos.LongVenue,
				Action:         "SELL",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    pos.NotionalUSD,
				MaxSlippageBps: e.policy.MaxSlippageBps,
				PriceLimit:     midPrice * 0.995,
			},
			{
				Venue:          pos.ShortVenue,
				Action:         "BUY",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    pos.NotionalUSD,
				MaxSlippageBps: e.policy.MaxSlippageBps,
				PriceLimit:     midPrice * 1.005,
			},
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsNet: 0,
		},
		Constraints: arb.IntentConstraints{
			MinQuality:      "LOW",
			RequireVenueOK:  false,
			MaxAgeMs:        e.policy.IntentTTLMs,
			HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
			CooldownKey:     fmt.Sprintf("exit:diff:%s:%s:%s", pos.Symbol, pos.LongVenue, pos.ShortVenue),
		},
	}
}
