package arb

import (
	"crypto/rand"
	"fmt"
	"sort"

	"github.com/yourorg/consensus-engine/internal/consensus"
)

const epsilon = 1e-9

// Engine detects cross-venue arbitrage opportunities from ConsensusUpdate events.
type Engine struct {
	policy   *Policy
	cooldown *Cooldown

	// Observability counters — read by the service loop for periodic logging.
	Rejected map[RejectionReason]int64
	Emitted  map[string]int64 // keyed by symbol
}

func NewEngine(p *Policy) *Engine {
	return &Engine{
		policy:   p,
		cooldown: NewCooldown(p.CooldownMs),
		Rejected: make(map[RejectionReason]int64),
		Emitted:  make(map[string]int64),
	}
}

// Process evaluates a single ConsensusUpdate and returns at most two TradeIntents:
// the best opportunity found, plus a second using entirely disjoint venues if one exists.
// Returns nil when no qualifying opportunity is found.
func (e *Engine) Process(update ConsensusUpdate) []TradeIntent {
	quality := update.Consensus.Quality

	// Gate 1: quality must meet configured minimum.
	if qualityRank(quality) < qualityRank(e.policy.MinConsensusQuality) {
		e.Rejected[RejectLowQuality]++
		return nil
	}

	// Gate 2: TTL of 0 means this quality level is disabled.
	ttlMs := e.policy.intentTTL(quality)
	if ttlMs == 0 {
		e.Rejected[RejectLowQuality]++
		return nil
	}

	// Gate 3: need at least two eligible venues to form a pair.
	eligible := e.filterVenues(update)
	if len(eligible) < 2 {
		e.Rejected[RejectVenueFiltered]++
		return nil
	}

	venueMap := make(map[string]VenueMetrics, len(eligible))
	for _, vm := range eligible {
		venueMap[string(vm.Venue)] = vm
	}

	symbol := string(update.Symbol)
	pairs := e.policy.EnabledPairs[symbol]
	if len(pairs) == 0 {
		e.Rejected[RejectNoPairs]++
		return nil
	}

	latencyBps := e.policy.latencyBuffer(quality)
	minEdge := e.policy.minEdge(quality)
	consMid := update.Consensus.Mid
	if consMid < epsilon {
		return nil
	}

	var candidates []TradeIntent

	for _, size := range e.policy.SizeLadderUSD {
		for _, pair := range pairs {
			if len(pair) != 2 {
				continue
			}
			// Each configured pair is checked in both directions so the YAML only
			// needs to list each pair once.
			for _, ordered := range [2][2]string{
				{pair[0], pair[1]},
				{pair[1], pair[0]},
			} {
				buyName, sellName := ordered[0], ordered[1]
				buyVM, buyOK := venueMap[buyName]
				sellVM, sellOK := venueMap[sellName]
				if !buyOK || !sellOK {
					continue
				}

				cooldownKey := fmt.Sprintf("%s|%s|%s", symbol, buyName, sellName)
				if e.cooldown.IsOnCooldown(cooldownKey, update.TsMs) {
					e.Rejected[RejectCooldown]++
					continue
				}

				// Apply latency buffer to both legs: buying costs more, selling yields less.
				buyCost := buyVM.EffectiveBuy * (1 + latencyBps/10000)
				sellValue := sellVM.EffectiveSell * (1 - latencyBps/10000)

				grossEdgeBps := (sellValue - buyCost) / consMid * 10000
				netEdgeBps := grossEdgeBps // latency buffer IS the safety margin per spec

				if netEdgeBps < minEdge {
					e.Rejected[RejectInsufficientEdge]++
					continue
				}

				profitUSD := size * netEdgeBps / 10000

				intent := TradeIntent{
					TenantID:  update.TenantID,
					IntentID:  newUUID(),
					Strategy:  "CROSS_VENUE_ARB",
					Symbol:    symbol,
					TsMs:      update.TsMs,
					ExpiresMs: update.TsMs + ttlMs,
					Legs: []TradeLeg{
						{
							Venue:          buyName,
							Action:         "BUY",
							Type:           "MARKET_OR_IOC",
							NotionalUSD:    size,
							MaxSlippageBps: e.policy.MaxSlippageBps,
							// Conservative upper bound: the router must not fill above this.
							PriceLimit: buyVM.EffectiveBuy * (1 + e.policy.MaxSlippageBps/10000),
						},
						{
							Venue:          sellName,
							Action:         "SELL",
							Type:           "MARKET_OR_IOC",
							NotionalUSD:    size,
							MaxSlippageBps: e.policy.MaxSlippageBps,
							// Conservative lower bound: the router must not fill below this.
							PriceLimit: sellVM.EffectiveSell * (1 - e.policy.MaxSlippageBps/10000),
						},
					},
					Expected: ExpectedMetrics{
						EdgeBpsGross:   grossEdgeBps,
						EdgeBpsNet:     netEdgeBps,
						ProfitUSDNet:   profitUSD,
						FeesUSDEst:     estimateFeesUSD(size, buyVM, sellVM),
						SlippageUSDEst: size * latencyBps * 2 / 10000,
					},
					Constraints: IntentConstraints{
						MinQuality:      e.policy.MinConsensusQuality,
						RequireVenueOK:  !e.policy.AllowWarnVenues,
						MaxAgeMs:        ttlMs,
						HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
						CooldownKey:     cooldownKey,
					},
					Debug: IntentDebug{
						ConsensusBandLow:  update.Consensus.BandLow,
						ConsensusBandHigh: update.Consensus.BandHigh,
						BuyOn:             buyName,
						SellOn:            sellName,
						BuyExec:           buyVM.EffectiveBuy,
						SellExec:          sellVM.EffectiveSell,
					},
				}
				candidates = append(candidates, intent)
			}
		}
	}

	best := pickBest(candidates)
	for _, intent := range best {
		e.cooldown.Mark(intent.Constraints.CooldownKey, update.TsMs)
		e.Emitted[symbol]++
	}
	return best
}

// filterVenues returns only the venues that meet the configured health criteria.
func (e *Engine) filterVenues(update ConsensusUpdate) []VenueMetrics {
	var out []VenueMetrics
	for _, vm := range update.Venues {
		if vm.Status == consensus.StateBlacklisted {
			continue
		}
		if !e.policy.AllowWarnVenues && vm.Status == consensus.StateWarn {
			continue
		}
		if e.policy.IgnoreFlaggedVenues && hasFlag(vm.Flags, "OUTLIER") {
			continue
		}
		out = append(out, vm)
	}
	return out
}

// pickBest selects the highest-profit intent as primary, then optionally a
// second intent whose venues are entirely disjoint from the first.
func pickBest(candidates []TradeIntent) []TradeIntent {
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Expected.ProfitUSDNet > candidates[j].Expected.ProfitUSDNet
	})
	result := []TradeIntent{candidates[0]}
	used := map[string]bool{
		candidates[0].Legs[0].Venue: true,
		candidates[0].Legs[1].Venue: true,
	}
	for _, c := range candidates[1:] {
		if !used[c.Legs[0].Venue] && !used[c.Legs[1].Venue] {
			result = append(result, c)
			break
		}
	}
	return result
}

// estimateFeesUSD approximates the fee cost from the difference between
// raw executable prices and fee-inclusive effective prices.
// EffectiveBuy  = BuyExec  * (1 + feeBps/10000)  → fee ≈ EffectiveBuy  - BuyExec
// EffectiveSell = SellExec * (1 - feeBps/10000)  → fee ≈ SellExec - EffectiveSell
func estimateFeesUSD(notional float64, buy, sell VenueMetrics) float64 {
	var fee float64
	if buy.EffectiveBuy > epsilon {
		fee += (buy.EffectiveBuy - buy.BuyExec) / buy.EffectiveBuy * notional
	}
	if sell.SellExec > epsilon {
		fee += (sell.SellExec - sell.EffectiveSell) / sell.SellExec * notional
	}
	return fee
}

func hasFlag(flags []string, target string) bool {
	for _, f := range flags {
		if f == target {
			return true
		}
	}
	return false
}

// newUUID generates a random RFC 4122 v4 UUID without external dependencies.
func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
