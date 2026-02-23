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
}

func NewEngine(p *Policy) *Engine {
	return &Engine{
		policy:           p,
		strategyDeployed: make(map[string]float64),
		venueDeployed:    make(map[string]float64),
		Rejected:         make(map[string]int),
	}
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

	if scaleFactor < 0.1 {
		return e.reject(intent, "insufficient_available_notional")
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
	case "CROSS_VENUE_ARB":
		return e.policy.MinQualityForArb
	case "FUNDING_CARRY", "FUNDING_DIFFERENTIAL":
		return e.policy.MinQualityForFunding
	case "SPREAD_CAPTURE", "LIQUIDATION_CONTRA":
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
	return n / float64(len(intent.Legs)) // average leg notional
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
