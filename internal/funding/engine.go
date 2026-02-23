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
}

func NewEngine(p *Policy) *Engine {
	state := make(map[string]map[string]*venueData)
	for _, sym := range p.Symbols {
		state[sym] = make(map[string]*venueData)
	}
	return &Engine{
		policy:     p,
		state:      state,
		cooldown:   make(map[string]int64),
		Emitted:    make(map[string]int),
		Rejected:   make(map[string]int),
		forecaster: NewRegimeForecaster(0.15),
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
		d.fundingRate = q.FundingRate
		// Update the regime forecaster whenever a new funding rate arrives.
		e.forecaster.Update(ven, sym, q.FundingRate, q.TsMs)
	}
	if q.Mark > 0 {
		d.markPrice = q.Mark
	}
	if q.FeeBpsTaker > 0 {
		d.feeBpsTaker = q.FeeBpsTaker
	}
	d.updatedMs = q.TsMs
}

// Regime returns the current regime snapshot for a venue+symbol pair.
// Returns nil if no funding rate data has been seen yet for that pair.
func (e *Engine) Regime(venue, symbol string) *Regime {
	return e.forecaster.Get(venue, symbol)
}

// Evaluate checks all configured strategies and returns any qualifying intents.
func (e *Engine) Evaluate(tenantID string) []arb.TradeIntent {
	e.mu.RLock()
	defer e.mu.RUnlock()

	now := time.Now().UnixMilli()
	var intents []arb.TradeIntent

	for _, sym := range e.policy.Symbols {
		venueMap := e.state[sym]

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

			// Annualised gross yield (%) from 8h funding: rate × 3 × 365 × 100
			annualGrossPct := d.fundingRate * 3 * 365 * 100
			// Cost: 2 entry legs × taker fee (spot buy + perp short).
			// Expressed as annual % (conservative: assume hold 30 days = 90 periods).
			entryFeePct := d.feeBpsTaker * 2 / 10000 * 100
			annualNetPct := annualGrossPct - entryFeePct

			threshold := e.minYield(annualNetPct)
			if annualNetPct < threshold {
				e.Rejected["carry_below_threshold"]++
				continue
			}

			key := fmt.Sprintf("carry:%s:%s", sym, ven)
			if e.onCooldown(key, now) {
				e.Rejected["carry_cooldown"]++
				continue
			}

			notional := e.policy.MaxNotionalUSD
			intent := e.buildCarryIntent(tenantID, sym, ven, d, notional, annualNetPct, now)
			intents = append(intents, intent)
			e.cooldown[key] = now
			e.Emitted[StrategyFundingCarry]++
			log.Printf("funding: CARRY intent sym=%s venue=%s net_annual=%.2f%%",
				sym, ven, annualNetPct)
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
				feePct := (dA.feeBpsTaker+dB.feeBpsTaker) * 2 / 10000 * 100
				annualNetPct := annualGrossPct - feePct
				threshold := e.minYield(annualNetPct)
				if annualNetPct < threshold {
					e.Rejected["diff_below_threshold"]++
					continue
				}

				key := fmt.Sprintf("diff:%s:%s:%s", sym, vA, vB)
				if e.onCooldown(key, now) {
					e.Rejected["diff_cooldown"]++
					continue
				}

				notional := e.policy.MaxNotionalUSD
				midPrice := (dA.markPrice + dB.markPrice) / 2
				intent := e.buildDiffIntent(tenantID, sym, vA, vB, dA, dB,
					notional, annualNetPct, diffBps, midPrice, now)
				intents = append(intents, intent)
				e.cooldown[key] = now
				e.Emitted[StrategyFundingDifferential]++
				log.Printf("funding: DIFF intent sym=%s long=%s short=%s diff=%.2fbps net_annual=%.2f%%",
					sym, vA, vB, diffBps, annualNetPct)
			}
		}
	}
	return intents
}

func (e *Engine) minYield(annualNetPct float64) float64 {
	// HIGH quality: use HIGH threshold; MED/LOW: use MED threshold.
	// (Funding strategies don't consume consensus quality directly in V1.)
	if t, ok := e.policy.MinAnnualYieldPct["HIGH"]; ok {
		if annualNetPct >= t {
			return t
		}
	}
	if t, ok := e.policy.MinAnnualYieldPct["MED"]; ok {
		return t
	}
	return 8.0
}

func (e *Engine) onCooldown(key string, nowMs int64) bool {
	last, ok := e.cooldown[key]
	if !ok {
		return false
	}
	return nowMs-last < e.policy.CooldownS*1000
}

func (e *Engine) buildCarryIntent(tenantID, sym, venue string, d *venueData,
	notional, annualNetPct float64, nowMs int64) arb.TradeIntent {

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
				MaxSlippageBps: e.policy.MaxSlippageBps,
				PriceLimit:     d.markPrice * 1.005,
			},
			{
				Venue:          venue,
				Action:         "SELL",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: e.policy.MaxSlippageBps,
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
	dLong, dShort *venueData, notional, annualNetPct, diffBps, midPrice float64, nowMs int64) arb.TradeIntent {

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
				MaxSlippageBps: e.policy.MaxSlippageBps,
				PriceLimit:     midPrice * 1.005,
			},
			{
				Venue:          shortVenue,
				Action:         "SELL",
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    notional,
				MaxSlippageBps: e.policy.MaxSlippageBps,
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
