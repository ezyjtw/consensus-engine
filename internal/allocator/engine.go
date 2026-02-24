package allocator

import (
	"fmt"
	"log"
	"sync"

	"github.com/ezyjtw/consensus-engine/internal/arb"
)

// Outcome records the allocator's decision on an intent.
type Outcome struct {
	Intent   arb.TradeIntent
	Approved bool
	Reason   string
}

// Engine validates intents against position caps and quality gates, and emits
// size-adjusted approved intents.
type Engine struct {
	policy *Policy
	mu     sync.Mutex
	// notional deployed per strategy (in-memory; reset on restart)
	strategyDeployed map[string]float64
	// notional deployed per venue
	venueDeployed map[string]float64
	Approved      int
	Rejected      map[string]int
	// Optional: OI-gated position sizing and dynamic allocation.
	OITracker  *OITracker
	DynAlloc   *DynamicAllocator

	// Capital tracking — paper mode starting balance.
	initialCapital float64
	cumulativePnL  float64
	totalFees      float64
	fillCount      int
	peakEquity     float64 // high-water mark for drawdown calculation
}

func NewEngine(p *Policy) *Engine {
	e := &Engine{
		policy:           p,
		strategyDeployed: make(map[string]float64),
		venueDeployed:    make(map[string]float64),
		Rejected:         make(map[string]int),
		initialCapital:   p.InitialCapitalUSD,
	}
	if e.initialCapital > 0 {
		e.peakEquity = e.initialCapital
	}
	return e
}

// Evaluate decides whether to approve an intent and returns the (possibly
// size-adjusted) intent along with the decision.
func (e *Engine) Evaluate(intent arb.TradeIntent, systemMode, consensusQuality string) Outcome {
	e.mu.Lock()
	defer e.mu.Unlock()

	// Gate 1: system must be RUNNING.
	if systemMode != "RUNNING" && systemMode != "" {
		return e.reject(intent, fmt.Sprintf("system_mode:%s", systemMode))
	}

	// Gate 2: consensus quality gate per strategy type.
	minQ := e.minQualityForStrategy(intent.Strategy)
	if qualityRank(consensusQuality) < qualityRank(minQ) {
		return e.reject(intent, "consensus_quality_below_minimum")
	}

	// Gate 3: intent not expired.
	// (Handled by execution router; allocator trusts intent has TTL.)

	// Gate 4: per-strategy notional cap.
	intentNotional := totalNotional(intent)
	stratCap, ok := e.policy.PerStrategyMaxUSD[intent.Strategy]
	if !ok {
		stratCap = 0 // unknown strategy → block
	}
	if stratCap == 0 {
		return e.reject(intent, "strategy_not_configured")
	}

	// Dynamic allocation: adjust cap based on strategy performance.
	if e.DynAlloc != nil {
		dynCap := e.DynAlloc.AdjustedCap(intent.Strategy)
		if dynCap > 0 && dynCap < stratCap {
			stratCap = dynCap
		}
	}

	// OI-gated sizing: scale down intent notional based on market liquidity.
	oiMultiplier := 1.0
	if e.OITracker != nil && len(intent.Legs) > 0 {
		venue := intent.Legs[0].Venue
		oiMultiplier = e.OITracker.LiquidityMultiplier(venue, intent.Symbol, e.policy.BaselineOI)
	}
	// Gate 4b: per-position concentration limit.
	if e.policy.MaxSingleIntentPct > 0 {
		maxIntentUSD := stratCap * e.policy.MaxSingleIntentPct / 100
		if intentNotional > maxIntentUSD {
			return e.reject(intent, "single_intent_concentration_exceeded")
		}
	}

	currentStrat := e.strategyDeployed[intent.Strategy]
	available := stratCap - currentStrat
	if available <= 0 {
		return e.reject(intent, "strategy_cap_exceeded")
	}

	// Gate 5: per-venue notional cap.
	for _, leg := range intent.Legs {
		venueCap, ok := e.policy.PerVenueMaxUSD[leg.Venue]
		if !ok {
			venueCap = 0
		}
		if venueCap == 0 {
			return e.reject(intent, fmt.Sprintf("venue_not_configured:%s", leg.Venue))
		}
		if e.venueDeployed[leg.Venue]+leg.NotionalUSD > venueCap {
			return e.reject(intent, fmt.Sprintf("venue_cap_exceeded:%s", leg.Venue))
		}
	}

	// Gate 5b: capital pool gate — when paper capital is configured, ensure
	// the total deployed notional plus this intent does not exceed equity.
	if e.initialCapital > 0 {
		equity := e.initialCapital + e.cumulativePnL
		totalDeployed := e.totalDeployed()
		availCap := equity - totalDeployed
		if availCap <= 0 {
			return e.reject(intent, "capital_exhausted")
		}
		if intentNotional > availCap {
			// Will be scaled down below.
			log.Printf("allocator: capital gate: equity=$%.0f deployed=$%.0f avail=$%.0f intent=$%.0f",
				equity, totalDeployed, availCap, intentNotional)
		}
	}

	// Scale down notional to fit within the tightest available cap.
	scaleFactor := 1.0
	if intentNotional > available {
		scaleFactor = available / intentNotional
	}
	for _, leg := range intent.Legs {
		venueCap := e.policy.PerVenueMaxUSD[leg.Venue]
		avail := venueCap - e.venueDeployed[leg.Venue]
		if leg.NotionalUSD*scaleFactor > avail {
			sf := avail / leg.NotionalUSD
			if sf < scaleFactor {
				scaleFactor = sf
			}
		}
	}

	// Apply capital pool constraint to scale factor.
	if e.initialCapital > 0 {
		equity := e.initialCapital + e.cumulativePnL
		availCap := equity - e.totalDeployed()
		if intentNotional*scaleFactor > availCap {
			sf := availCap / intentNotional
			if sf < scaleFactor {
				scaleFactor = sf
			}
		}
	}

	// Apply OI liquidity multiplier to scale factor.
	scaleFactor *= oiMultiplier

	if scaleFactor < 0.1 {
		return e.reject(intent, "insufficient_available_notional")
	}

	// Gate 6: fractional Kelly position sizing (optional).
	// When KellyFraction > 0, the intent is additionally scaled by the Kelly fraction.
	// This prevents over-betting on any single signal and is the recommended setting
	// for institutional deployments (0.25 = quarter-Kelly).
	//
	// For arb strategies the Kelly fraction acts as a simple risk-adjusted size limit:
	//   kelly_size = kelly_fraction × available_notional
	// A future improvement can weight by edge_bps / historical_edge_variance.
	if e.policy.KellyFraction > 0 && e.policy.KellyFraction < 1.0 {
		scaleFactor *= e.policy.KellyFraction
		log.Printf("allocator: kelly fraction=%.2f applied to strategy=%s symbol=%s",
			e.policy.KellyFraction, intent.Strategy, intent.Symbol)
	}

	if scaleFactor < 0.05 {
		return e.reject(intent, "kelly_scaled_notional_too_small")
	}

	// Apply scale factor to all legs.
	adjusted := intent
	for i := range adjusted.Legs {
		adjusted.Legs[i].NotionalUSD *= scaleFactor
	}
	adjusted.Expected.ProfitUSDNet *= scaleFactor
	adjusted.Expected.FeesUSDEst *= scaleFactor

	// Update deployed tracking.
	e.strategyDeployed[intent.Strategy] += totalNotional(adjusted)
	for _, leg := range adjusted.Legs {
		e.venueDeployed[leg.Venue] += leg.NotionalUSD
	}
	e.Approved++
	log.Printf("allocator: APPROVED strategy=%s symbol=%s notional=$%.0f scale=%.2f",
		intent.Strategy, intent.Symbol, totalNotional(adjusted), scaleFactor)
	return Outcome{Intent: adjusted, Approved: true}
}

// ReleaseNotional removes deployed notional when a position is closed.
// Called by the ledger / execution router via a fill event.
func (e *Engine) ReleaseNotional(strategy, venue string, notional float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.strategyDeployed[strategy] -= notional
	if e.strategyDeployed[strategy] < 0 {
		e.strategyDeployed[strategy] = 0
	}
	e.venueDeployed[venue] -= notional
	if e.venueDeployed[venue] < 0 {
		e.venueDeployed[venue] = 0
	}
}

func (e *Engine) reject(intent arb.TradeIntent, reason string) Outcome {
	e.Rejected[reason]++
	log.Printf("allocator: REJECTED strategy=%s symbol=%s reason=%s",
		intent.Strategy, intent.Symbol, reason)
	return Outcome{Intent: intent, Approved: false, Reason: reason}
}

func (e *Engine) minQualityForStrategy(strategy string) string {
	switch strategy {
	case "CROSS_VENUE_ARB", "BASIS_CONVERGENCE", "DEX_CEX_ARB", "L2_BRIDGE_ARB":
		return e.policy.MinQualityForArb
	case "FUNDING_CARRY", "FUNDING_DIFFERENTIAL", "FUNDING_CARRY_REVERSE",
		"FUNDING_CARRY_EXIT", "FUNDING_DIFFERENTIAL_EXIT", "FUNDING_CARRY_REVERSE_EXIT":
		return e.policy.MinQualityForFunding
	case "SPREAD_CAPTURE", "LIQUIDATION_CONTRA", "CASCADE_CONTRA", "CORRELATION_BREAK":
		return e.policy.MinQualityForLiquidity
	default:
		return "HIGH"
	}
}

func totalNotional(intent arb.TradeIntent) float64 {
	var n float64
	for _, l := range intent.Legs {
		n += l.NotionalUSD
	}
	return n
}

func qualityRank(q string) int {
	switch q {
	case "HIGH":
		return 2
	case "MED":
		return 1
	default:
		return 0
	}
}

// ── Capital tracking ─────────────────────────────────────────────────────

// totalDeployed returns the sum of all currently deployed notional.
func (e *Engine) totalDeployed() float64 {
	var sum float64
	for _, v := range e.strategyDeployed {
		sum += v
	}
	return sum
}

// RecordPnL updates the cumulative P&L and high-water mark.
// Called when fills are received. Must be called under e.mu.
func (e *Engine) RecordPnL(pnl, fees float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cumulativePnL += pnl
	e.totalFees += fees
	e.fillCount++
	equity := e.initialCapital + e.cumulativePnL
	if equity > e.peakEquity {
		e.peakEquity = equity
	}
}

// EquitySnapshot returns the current capital state.
type EquitySnapshot struct {
	InitialCapitalUSD float64 `json:"initial_capital_usd"`
	CumulativePnLUSD  float64 `json:"cumulative_pnl_usd"`
	CurrentEquityUSD  float64 `json:"current_equity_usd"`
	DeployedUSD       float64 `json:"deployed_usd"`
	AvailableUSD      float64 `json:"available_usd"`
	TotalFeesUSD      float64 `json:"total_fees_usd"`
	FillCount         int     `json:"fill_count"`
	ReturnPct         float64 `json:"return_pct"`
	DrawdownPct       float64 `json:"drawdown_pct"`
	PeakEquityUSD     float64 `json:"peak_equity_usd"`
}

// Equity returns a snapshot of the current capital state.
func (e *Engine) Equity() EquitySnapshot {
	e.mu.Lock()
	defer e.mu.Unlock()

	equity := e.initialCapital + e.cumulativePnL
	deployed := e.totalDeployed()
	available := equity - deployed
	if available < 0 {
		available = 0
	}

	returnPct := 0.0
	if e.initialCapital > 0 {
		returnPct = (e.cumulativePnL / e.initialCapital) * 100
	}

	drawdownPct := 0.0
	if e.peakEquity > 0 {
		drawdownPct = ((e.peakEquity - equity) / e.peakEquity) * 100
		if drawdownPct < 0 {
			drawdownPct = 0
		}
	}

	return EquitySnapshot{
		InitialCapitalUSD: e.initialCapital,
		CumulativePnLUSD:  e.cumulativePnL,
		CurrentEquityUSD:  equity,
		DeployedUSD:       deployed,
		AvailableUSD:      available,
		TotalFeesUSD:      e.totalFees,
		FillCount:         e.fillCount,
		ReturnPct:         returnPct,
		DrawdownPct:       drawdownPct,
		PeakEquityUSD:     e.peakEquity,
	}
}
