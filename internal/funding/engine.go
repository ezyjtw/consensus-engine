package funding

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

const (
	StrategyFundingCarry        = "FUNDING_CARRY"
	StrategyFundingDifferential = "FUNDING_DIFFERENTIAL"
	StrategyFundingCarryReverse = "FUNDING_CARRY_REVERSE"
)

// venueData holds the latest known rates and fees for one venue+symbol.
type venueData struct {
	fundingRate float64 // 8h rate (e.g. 0.00035)
	markPrice   float64
	feeBpsTaker float64
	updatedMs   int64
}

// Engine evaluates funding strategies and emits trade intents.
type Engine struct {
	policy     *Policy
	mu         sync.RWMutex
	// state[symbol][venue]
	state      map[string]map[string]*venueData
	// cooldown: intentKey → last emitted unix ms
	cooldown   map[string]int64
	Emitted    map[string]int
	Rejected   map[string]int
	forecaster *RegimeForecaster
	scheduler  *FundingScheduler
	volTracker *VolTracker
	// runtimeStages holds dynamic stage overrides set via the dashboard.
	// These take precedence over YAML-configured stages in SymbolOverrides.
	runtimeStages map[string]FundingStage
	// ExitSignalC is a non-blocking signal channel. UpdateQuote sends on it
	// when a funding rate sign change is detected, so the main loop can
	// immediately evaluate exits instead of waiting for the next eval tick.
	ExitSignalC chan struct{}
	// nowFunc overrides the time source for Evaluate/EvaluateExits. Used in
	// tests to provide deterministic timestamps. Nil means use time.Now().
	nowFunc func() time.Time
}

func NewEngine(p *Policy) *Engine {
	state := make(map[string]map[string]*venueData)
	for _, sym := range p.Symbols {
		state[sym] = make(map[string]*venueData)
	}
	return &Engine{
		policy:      p,
		state:       state,
		cooldown:    make(map[string]int64),
		Emitted:     make(map[string]int),
		Rejected:    make(map[string]int),
		forecaster:  NewRegimeForecaster(0.15),
		scheduler:   NewFundingScheduler(),
		volTracker:  NewVolTracker(500),
		ExitSignalC: make(chan struct{}, 1),
	}
}

// UpdateQuote ingests a normalised market quote and updates internal state.
func (e *Engine) UpdateQuote(q consensus.Quote) {
	sym := string(q.Symbol)
	ven := string(q.Venue)

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, ok := e.state[sym]; !ok {
		return // symbol not configured
	}
	d, ok := e.state[sym][ven]
	if !ok {
		d = &venueData{}
		e.state[sym][ven] = d
	}
	if q.FundingRate != 0 {
		oldRate := d.fundingRate
		d.fundingRate = q.FundingRate
		// Update the regime forecaster whenever a new funding rate arrives.
		e.forecaster.Update(ven, sym, q.FundingRate, q.TsMs)
		// Detect funding rate sign change → signal immediate exit evaluation.
		if oldRate != 0 && signFlipped(oldRate, q.FundingRate) {
			select {
			case e.ExitSignalC <- struct{}{}:
			default: // already signalled, don't block
			}
		}
	}
	if q.Mark > 0 {
		d.markPrice = q.Mark
		// Feed the vol tracker with mark price observations.
		e.volTracker.Record(ven, sym, q.Mark, q.TsMs)
	}
	if q.FeeBpsTaker > 0 {
		d.feeBpsTaker = q.FeeBpsTaker
	}
	d.updatedMs = q.TsMs
}

// SetRuntimeStages updates the dynamic stage overrides from Redis.
// Called before each evaluation cycle to pick up dashboard changes.
// Passing nil clears all runtime overrides, falling back to YAML config.
func (e *Engine) SetRuntimeStages(stages map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if stages == nil {
		e.runtimeStages = nil
		return
	}
	m := make(map[string]FundingStage, len(stages))
	for sym, s := range stages {
		m[sym] = FundingStage(s)
	}
	e.runtimeStages = m
}

// effectiveStage returns the runtime stage if set, else the YAML-configured stage.
func (e *Engine) effectiveStage(symbol string) FundingStage {
	if e.runtimeStages != nil {
		if s, ok := e.runtimeStages[symbol]; ok {
			return s
		}
	}
	return e.policy.stage(symbol)
}

// Regime returns the current regime snapshot for a venue+symbol pair.
// Returns nil if no funding rate data has been seen yet for that pair.
func (e *Engine) Regime(venue, symbol string) *Regime {
	return e.forecaster.Get(venue, symbol)
}

// now returns the current time, or a test-injected time if nowFunc is set.
func (e *Engine) now() time.Time {
	if e.nowFunc != nil {
		return e.nowFunc()
	}
	return time.Now()
}

// Evaluate checks all configured strategies and returns any qualifying intents.
func (e *Engine) Evaluate(tenantID string) []arb.TradeIntent {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := e.now()
	nowMs := now.UnixMilli()
	var intents []arb.TradeIntent

	// Scheduling: skip new entries in the last 30 minutes before a funding reset.
	// Positions opened near reset only collect a partial period and face rate-flip risk.
	nearReset := e.scheduler.IsNearReset(now, 30*time.Minute)
	optimalEntry := e.scheduler.OptimalEntry(now)

	for _, sym := range e.policy.Symbols {
		venueMap := e.state[sym]
		symStage := e.effectiveStage(sym)

		// ── FUNDING_CARRY (one venue: long spot, short perp) ──────────────
		for ven, d := range venueMap {
			if d.markPrice == 0 || d.fundingRate == 0 {
				continue
			}

			// Skip if the regime is VOLATILE — funding direction is too uncertain.
			if r := e.forecaster.Get(ven, sym); r != nil && r.Label == "VOLATILE" {
				e.Rejected["carry_volatile_regime"]++
				log.Printf("funding: CARRY skipped sym=%s venue=%s regime=VOLATILE stddev=%.6f ewa=%.6f",
					sym, ven, r.StdDev, r.EWA)
				continue
			}

			// Skip new entries near funding reset — partial period risk.
			if nearReset {
				e.Rejected["carry_near_reset"]++
				continue
			}

			// Annualised gross yield (%) from 8h funding: rate × 3 × 365 × 100
			annualGrossPct := d.fundingRate * 3 * 365 * 100
			// Round-trip cost: 4 legs of taker fee (entry: spot buy + perp short,
			// exit: spot sell + perp buy) plus slippage on all 4 legs.
			// Deducted as a one-off % cost from the annualised yield.
			roundTripFeePct := d.feeBpsTaker * 4 / 10000 * 100
			roundTripSlipPct := e.policy.maxSlippage(sym) * 4 / 10000 * 100
			annualNetPct := annualGrossPct - roundTripFeePct - roundTripSlipPct

			threshold := e.minYield(sym, annualNetPct)
			if annualNetPct < threshold {
				e.Rejected["carry_below_threshold"]++
				continue
			}

			key := fmt.Sprintf("carry:%s:%s", sym, ven)
			if e.onCooldown(key, nowMs) {
				e.Rejected["carry_cooldown"]++
				continue
			}

			// OBSERVE stage: log the signal but emit no intent.
			if symStage == StageObserve {
				e.Emitted["carry_observe"]++
				log.Printf("funding: CARRY [OBSERVE] sym=%s venue=%s net_annual=%.2f%% (no intent emitted)",
					sym, ven, annualNetPct)
				continue
			}

			// Volatility gating: reduce position size when realized vol is elevated.
			symNotional := e.policy.maxNotional(sym)
			notional := symNotional
			notional = e.applyVolGate(ven, sym, notional)
			notional = e.applyStageNotional(sym, notional)

			// Bonus: increase notional slightly in optimal entry window (just after reset).
			if optimalEntry && notional == symNotional {
				log.Printf("funding: CARRY optimal entry window for %s on %s", sym, ven)
			}

			symSlippage := e.policy.maxSlippage(sym)
			intent := e.buildCarryIntent(tenantID, sym, ven, d, notional, annualNetPct, symSlippage, nowMs)
			e.applyStageConstraints(sym, &intent)
			intents = append(intents, intent)
			e.cooldown[key] = nowMs
			e.Emitted[StrategyFundingCarry]++
			log.Printf("funding: CARRY intent sym=%s venue=%s stage=%s net_annual=%.2f%% notional=%.0f",
				sym, ven, symStage, annualNetPct, notional)
		}

		// ── FUNDING_CARRY_REVERSE (one venue: short spot, long perp when rate < 0)
		for ven, d := range venueMap {
			if d.markPrice == 0 || d.fundingRate == 0 {
				continue
			}
			// Only trigger when funding rate is negative (shorts paying longs).
			if d.fundingRate >= 0 {
				continue
			}

			// Skip if the regime is VOLATILE.
			if r := e.forecaster.Get(ven, sym); r != nil && r.Label == "VOLATILE" {
				e.Rejected["reverse_carry_volatile_regime"]++
				continue
			}

			// Skip near funding reset.
			if nearReset {
				e.Rejected["reverse_carry_near_reset"]++
				continue
			}

			// Annualised gross yield from negative funding: |rate| × 3 × 365 × 100
			annualGrossPct := -d.fundingRate * 3 * 365 * 100
			roundTripFeePct := d.feeBpsTaker * 4 / 10000 * 100
			roundTripSlipPct := e.policy.maxSlippage(sym) * 4 / 10000 * 100
			annualNetPct := annualGrossPct - roundTripFeePct - roundTripSlipPct

			threshold := e.minYield(sym, annualNetPct)
			if annualNetPct < threshold {
				e.Rejected["reverse_carry_below_threshold"]++
				continue
			}

			key := fmt.Sprintf("reverse_carry:%s:%s", sym, ven)
			if e.onCooldown(key, nowMs) {
				e.Rejected["reverse_carry_cooldown"]++
				continue
			}

			// OBSERVE stage: log the signal but emit no intent.
			if symStage == StageObserve {
				e.Emitted["reverse_carry_observe"]++
				log.Printf("funding: REVERSE CARRY [OBSERVE] sym=%s venue=%s net_annual=%.2f%% (no intent emitted)",
					sym, ven, annualNetPct)
				continue
			}

			notional := e.policy.maxNotional(sym)
			notional = e.applyVolGate(ven, sym, notional)
			notional = e.applyStageNotional(sym, notional)

			symSlippage := e.policy.maxSlippage(sym)
			intent := e.buildReverseCarryIntent(tenantID, sym, ven, d, notional, annualNetPct, symSlippage, nowMs)
			e.applyStageConstraints(sym, &intent)
			intents = append(intents, intent)
			e.cooldown[key] = nowMs
			e.Emitted[StrategyFundingCarryReverse]++
			log.Printf("funding: REVERSE CARRY intent sym=%s venue=%s stage=%s net_annual=%.2f%% notional=%.0f rate=%.6f",
				sym, ven, symStage, annualNetPct, notional, d.fundingRate)
		}

		// ── FUNDING_DIFFERENTIAL (cross-venue: long low-rate, short high-rate) ─
		venues := e.policy.Venues
		for i := 0; i < len(venues); i++ {
			for j := i + 1; j < len(venues); j++ {
				vA, vB := venues[i], venues[j]
				dA, okA := venueMap[vA]
				dB, okB := venueMap[vB]
				if !okA || !okB {
					continue
				}
				if dA.markPrice == 0 || dB.markPrice == 0 {
					continue
				}

				// Skip if either venue's regime is VOLATILE — spread direction unreliable.
				rA := e.forecaster.Get(vA, sym)
				rB := e.forecaster.Get(vB, sym)
				if (rA != nil && rA.Label == "VOLATILE") || (rB != nil && rB.Label == "VOLATILE") {
					e.Rejected["diff_volatile_regime"]++
					log.Printf("funding: DIFF skipped sym=%s venues=%s/%s regime=VOLATILE",
						sym, vA, vB)
					continue
				}

				rateA, rateB := dA.fundingRate, dB.fundingRate
				// Ensure vB has the higher rate (we short vB, long vA).
				if rateA > rateB {
					vA, vB = vB, vA
					dA, dB = dB, dA
					rateA, rateB = rateB, rateA
				}

				diffBps := (rateB - rateA) * 10000 // in bps
				if diffBps < e.policy.MinDifferentialBps8h {
					continue
				}

				annualGrossPct := (rateB - rateA) * 3 * 365 * 100
				// Round-trip: 4 legs across 2 venues + slippage on all 4.
				feePct := (dA.feeBpsTaker + dB.feeBpsTaker) * 4 / 10000 * 100
				slipPct := e.policy.maxSlippage(sym) * 4 / 10000 * 100
				annualNetPct := annualGrossPct - feePct - slipPct
				threshold := e.minYield(sym, annualNetPct)
				if annualNetPct < threshold {
					e.Rejected["diff_below_threshold"]++
					continue
				}

				// Skip new differential entries near funding reset.
				if nearReset {
					e.Rejected["diff_near_reset"]++
					continue
				}

				key := fmt.Sprintf("diff:%s:%s:%s", sym, vA, vB)
				if e.onCooldown(key, nowMs) {
					e.Rejected["diff_cooldown"]++
					continue
				}

				// OBSERVE stage: log the signal but emit no intent.
				if symStage == StageObserve {
					e.Emitted["diff_observe"]++
					log.Printf("funding: DIFF [OBSERVE] sym=%s long=%s short=%s diff=%.2fbps net_annual=%.2f%% (no intent emitted)",
						sym, vA, vB, diffBps, annualNetPct)
					continue
				}

				// Volatility gating: use the higher vol of the two venues.
				notional := e.policy.maxNotional(sym)
				notional = e.applyVolGate(vA, sym, notional)
				notional = e.applyVolGate(vB, sym, notional)
				notional = e.applyStageNotional(sym, notional)

				symSlippage := e.policy.maxSlippage(sym)
				midPrice := (dA.markPrice + dB.markPrice) / 2
				intent := e.buildDiffIntent(tenantID, sym, vA, vB, dA, dB,
					notional, annualNetPct, diffBps, midPrice, symSlippage, nowMs)
				e.applyStageConstraints(sym, &intent)
				intents = append(intents, intent)
				e.cooldown[key] = nowMs
				e.Emitted[StrategyFundingDifferential]++
				log.Printf("funding: DIFF intent sym=%s long=%s short=%s stage=%s diff=%.2fbps net_annual=%.2f%% notional=%.0f",
					sym, vA, vB, symStage, diffBps, annualNetPct, notional)
			}
		}
	}
	return intents
}

func (e *Engine) minYield(symbol string, annualNetPct float64) float64 {
	// HIGH quality: use HIGH threshold; MED/LOW: use MED threshold.
	// Per-symbol overrides take precedence over global defaults.
	if t, ok := e.policy.minYieldForSymbol(symbol, "HIGH"); ok {
		if annualNetPct >= t {
			return t
		}
	}
	if t, ok := e.policy.minYieldForSymbol(symbol, "MED"); ok {
		return t
	}
	return 8.0
}

// applyStageNotional applies the CONSERVATIVE size scaling factor to notional.
// For non-CONSERVATIVE stages, returns notional unchanged.
func (e *Engine) applyStageNotional(sym string, notional float64) float64 {
	if e.effectiveStage(sym) == StageConservative {
		return notional * e.policy.sizeScale(sym)
	}
	return notional
}

// applyStageConstraints sets ForcePaperMode on intents from PAPER-stage symbols.
func (e *Engine) applyStageConstraints(sym string, intent *arb.TradeIntent) {
	if e.effectiveStage(sym) == StagePaper {
		intent.Constraints.ForcePaperMode = true
	}
}

// applyVolGate reduces notional when realized vol exceeds the configured threshold.
func (e *Engine) applyVolGate(venue, symbol string, notional float64) float64 {
	if e.volTracker == nil || e.policy.VolatilityGate.VolThresholdPct <= 0 {
		return notional
	}
	vol := e.volTracker.RealizedVol(venue, symbol)
	if vol <= e.policy.VolatilityGate.VolThresholdPct {
		return notional
	}
	factor := e.policy.VolatilityGate.SizeReductionFactor
	if factor <= 0 || factor >= 1 {
		factor = 0.5
	}
	reduced := notional * factor
	log.Printf("funding: vol gate sym=%s venue=%s vol=%.1f%% > %.1f%% — reducing notional %.0f → %.0f",
		symbol, venue, vol, e.policy.VolatilityGate.VolThresholdPct, notional, reduced)
	return reduced
}

// signFlipped returns true if a and b have opposite signs (one positive, one negative).
func signFlipped(a, b float64) bool {
	return (a > 0 && b < 0) || (a < 0 && b > 0)
}

func (e *Engine) onCooldown(key string, nowMs int64) bool {
	last, ok := e.cooldown[key]
	if !ok {
		return false
	}
	return nowMs-last < e.policy.CooldownS*1000
}

func (e *Engine) buildCarryIntent(tenantID, sym, venue string, d *venueData,
	notional, annualNetPct, slippageBps float64, nowMs int64) arb.TradeIntent {

	feeUSD := notional * d.feeBpsTaker * 2 / 10000
	return arb.TradeIntent{
		TenantID:  tenantID,
		IntentID:  newUUID(),
		Strategy:  StrategyFundingCarry,
		Symbol:    sym,
		TsMs:      nowMs,
		ExpiresMs: nowMs + e.policy.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Venue:          venue,
				Action:         "BUY",
				Market:         "SPOT",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: slippageBps,
				PriceLimit:     d.markPrice * 1.005,
			},
			{
				Venue:          venue,
				Action:         "SELL",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: slippageBps,
				PriceLimit:     d.markPrice * 0.995,
			},
		},
		Expected: arb.ExpectedMetrics{
			FundingRate8hBps:  d.fundingRate * 10000,
			AnnualYieldPctNet: annualNetPct,
			FeesUSDEst:        feeUSD,
			ProfitUSDNet:      notional * annualNetPct / 100 / 365 * 8 / 24,
		},
		Constraints: arb.IntentConstraints{
			MinQuality:      "MED",
			RequireVenueOK:  true,
			MaxAgeMs:        e.policy.IntentTTLMs,
			HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
			CooldownKey:     fmt.Sprintf("carry:%s:%s", sym, venue),
		},
	}
}

func (e *Engine) buildDiffIntent(tenantID, sym, longVenue, shortVenue string,
	dLong, dShort *venueData, notional, annualNetPct, diffBps, midPrice, slippageBps float64, nowMs int64) arb.TradeIntent {

	feeUSD := notional * (dLong.feeBpsTaker + dShort.feeBpsTaker) * 2 / 10000
	return arb.TradeIntent{
		TenantID:  tenantID,
		IntentID:  newUUID(),
		Strategy:  StrategyFundingDifferential,
		Symbol:    sym,
		TsMs:      nowMs,
		ExpiresMs: nowMs + e.policy.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Venue:          longVenue,
				Action:         "BUY",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: slippageBps,
				PriceLimit:     midPrice * 1.005,
			},
			{
				Venue:          shortVenue,
				Action:         "SELL",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: slippageBps,
				PriceLimit:     midPrice * 0.995,
			},
		},
		Expected: arb.ExpectedMetrics{
			FundingRate8hBps:  diffBps,
			AnnualYieldPctNet: annualNetPct,
			FeesUSDEst:        feeUSD,
			ProfitUSDNet:      notional * annualNetPct / 100 / 365 * 8 / 24,
		},
		Constraints: arb.IntentConstraints{
			MinQuality:      "MED",
			RequireVenueOK:  true,
			MaxAgeMs:        e.policy.IntentTTLMs,
			HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
			CooldownKey:     fmt.Sprintf("diff:%s:%s:%s", sym, longVenue, shortVenue),
		},
	}
}

func (e *Engine) buildReverseCarryIntent(tenantID, sym, venue string, d *venueData,
	notional, annualNetPct, slippageBps float64, nowMs int64) arb.TradeIntent {

	feeUSD := notional * d.feeBpsTaker * 2 / 10000
	return arb.TradeIntent{
		TenantID:  tenantID,
		IntentID:  newUUID(),
		Strategy:  StrategyFundingCarryReverse,
		Symbol:    sym,
		TsMs:      nowMs,
		ExpiresMs: nowMs + e.policy.IntentTTLMs,
		Legs: []arb.TradeLeg{
			{
				Venue:          venue,
				Action:         "SELL",
				Market:         "SPOT",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: slippageBps,
				PriceLimit:     d.markPrice * 0.995,
			},
			{
				Venue:          venue,
				Action:         "BUY",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: slippageBps,
				PriceLimit:     d.markPrice * 1.005,
			},
		},
		Expected: arb.ExpectedMetrics{
			FundingRate8hBps:  d.fundingRate * 10000,
			AnnualYieldPctNet: annualNetPct,
			FeesUSDEst:        feeUSD,
			ProfitUSDNet:      notional * annualNetPct / 100 / 365 * 8 / 24,
		},
		Constraints: arb.IntentConstraints{
			MinQuality:      "MED",
			RequireVenueOK:  true,
			MaxAgeMs:        e.policy.IntentTTLMs,
			HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
			CooldownKey:     fmt.Sprintf("reverse_carry:%s:%s", sym, venue),
		},
	}
}
