package transfer

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/l2"
)

const StrategyL2BridgeArb = "L2_BRIDGE_ARB"

// BridgeArbConfig configures the L2 bridge arbitrage strategy.
type BridgeArbConfig struct {
	Enabled        bool    `yaml:"enabled"`
	MinEdgeBps     float64 `yaml:"min_edge_bps"`     // minimum edge after gas costs
	MaxNotionalUSD float64 `yaml:"max_notional_usd"`
	IntentTTLMs    int64   `yaml:"intent_ttl_ms"`
	CooldownMs     int64   `yaml:"cooldown_ms"`
}

// BridgeArbTracker monitors token prices across L2 networks and detects
// arbitrage opportunities exploiting price discrepancies between chains.
type BridgeArbTracker struct {
	mu       sync.Mutex
	bridge   *l2.Bridge
	prices   map[string]chainPrice // "network:symbol" → price
	cooldown map[string]int64
}

type chainPrice struct {
	mid  float64
	tsMs int64
}

// NewBridgeArbTracker creates an L2 bridge arb tracker.
func NewBridgeArbTracker(bridge *l2.Bridge) *BridgeArbTracker {
	return &BridgeArbTracker{
		bridge:   bridge,
		prices:   make(map[string]chainPrice),
		cooldown: make(map[string]int64),
	}
}

// UpdatePrice records a token price on a specific network.
func (bt *BridgeArbTracker) UpdatePrice(network, symbol string, mid float64, tsMs int64) {
	bt.mu.Lock()
	defer bt.mu.Unlock()
	key := network + ":" + symbol
	bt.prices[key] = chainPrice{mid: mid, tsMs: tsMs}
}

// Evaluate checks for cross-chain price discrepancies and returns bridge arb intents.
func (bt *BridgeArbTracker) Evaluate(ctx context.Context, cfg BridgeArbConfig, tenantID string) []arb.TradeIntent {
	if !cfg.Enabled || bt.bridge == nil || !bt.bridge.Enabled() {
		return nil
	}

	bt.mu.Lock()
	defer bt.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []arb.TradeIntent

	networks := []string{string(l2.NetworkArbitrum), string(l2.NetworkOptimism), string(l2.NetworkBase)}

	// Compare all symbols across all network pairs.
	symbols := bt.uniqueSymbols()
	for _, symbol := range symbols {
		for i := 0; i < len(networks); i++ {
			for j := i + 1; j < len(networks); j++ {
				nA, nB := networks[i], networks[j]
				pA, okA := bt.prices[nA+":"+symbol]
				pB, okB := bt.prices[nB+":"+symbol]
				if !okA || !okB {
					continue
				}
				// Skip stale prices (> 30s).
				if now-pA.tsMs > 30_000 || now-pB.tsMs > 30_000 {
					continue
				}
				if pA.mid <= 0 || pB.mid <= 0 {
					continue
				}

				// Check both directions.
				edgeAtoB := (pB.mid - pA.mid) / pA.mid * 10000 // buy on A, sell on B
				edgeBtoA := (pA.mid - pB.mid) / pB.mid * 10000 // buy on B, sell on A

				var buyNet, sellNet string
				var edgeBps float64
				if edgeAtoB > edgeBtoA && edgeAtoB > cfg.MinEdgeBps {
					buyNet, sellNet = nA, nB
					edgeBps = edgeAtoB
				} else if edgeBtoA > cfg.MinEdgeBps {
					buyNet, sellNet = nB, nA
					edgeBps = edgeBtoA
				} else {
					continue
				}

				cooldownKey := fmt.Sprintf("l2:%s:%s:%s", symbol, buyNet, sellNet)
				if last, ok := bt.cooldown[cooldownKey]; ok && now-last < cfg.CooldownMs {
					continue
				}

				// Estimate bridge cost.
				estimate, err := bt.bridge.Estimate(ctx, l2.BridgeRequest{
					Network:     l2.Network(buyNet),
					TokenSymbol: symbol,
					AmountWei:   "1000000000000000000", // 1 token for estimation
				})
				if err != nil {
					continue
				}

				// Subtract bridge cost from edge.
				bridgeCostBps := estimate.TotalCostUSD / cfg.MaxNotionalUSD * 10000
				netEdgeBps := edgeBps - bridgeCostBps
				if netEdgeBps < cfg.MinEdgeBps {
					continue
				}

				log.Printf("l2-bridge-arb: %s buy=%s sell=%s edge=%.1fbps net=%.1fbps bridge_cost=$%.2f",
					symbol, buyNet, sellNet, edgeBps, netEdgeBps, estimate.TotalCostUSD)

				intent := arb.TradeIntent{
					TenantID:  tenantID,
					IntentID:  newBridgeUUID(),
					Strategy:  StrategyL2BridgeArb,
					Symbol:    symbol,
					TsMs:      now,
					ExpiresMs: now + cfg.IntentTTLMs,
					Legs: []arb.TradeLeg{
						{
							Venue:       "l2:" + buyNet,
							Action:      "BUY",
							Market:      "SPOT",
							Type:        "DEX_SWAP",
							NotionalUSD: cfg.MaxNotionalUSD,
						},
						{
							Venue:       "l2:" + sellNet,
							Action:      "SELL",
							Market:      "SPOT",
							Type:        "DEX_SWAP",
							NotionalUSD: cfg.MaxNotionalUSD,
						},
					},
					Expected: arb.ExpectedMetrics{
						EdgeBpsGross: edgeBps,
						EdgeBpsNet:   netEdgeBps,
						ProfitUSDNet: cfg.MaxNotionalUSD * netEdgeBps / 10000,
						FeesUSDEst:   estimate.TotalCostUSD,
					},
					Constraints: arb.IntentConstraints{
						MinQuality:  "MED",
						MaxAgeMs:    cfg.IntentTTLMs,
						CooldownKey: cooldownKey,
					},
				}
				intents = append(intents, intent)
				bt.cooldown[cooldownKey] = now
			}
		}
	}
	return intents
}

func (bt *BridgeArbTracker) uniqueSymbols() []string {
	seen := make(map[string]bool)
	for key := range bt.prices {
		for i, c := range key {
			if c == ':' {
				seen[key[i+1:]] = true
				break
			}
		}
	}
	result := make([]string, 0, len(seen))
	for s := range seen {
		result = append(result, s)
	}
	return result
}

func newBridgeUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
