package arb

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

const StrategyCrossAssetArb = "CROSS_ASSET_ARB"

// CrossAssetConfig controls cross-asset arbitrage detection.
type CrossAssetConfig struct {
	Enabled        bool                `yaml:"enabled"`
	Pairs          []CrossAssetPair    `yaml:"pairs"`
	MinEdgeBps     float64             `yaml:"min_edge_bps"`
	NotionalUSD    float64             `yaml:"notional_usd"`
	MaxSlippageBps float64             `yaml:"max_slippage_bps"`
	IntentTTLMs    int64               `yaml:"intent_ttl_ms"`
	CooldownMs     int64               `yaml:"cooldown_ms"`
}

// CrossAssetPair defines a pair of related assets to monitor for arbitrage.
type CrossAssetPair struct {
	Name       string  `yaml:"name"`         // e.g. "BTC_PERP_SPOT"
	LongAsset  string  `yaml:"long_asset"`   // e.g. "BTC-PERP"
	ShortAsset string  `yaml:"short_asset"`  // e.g. "BTC-SPOT"
	LongVenue  string  `yaml:"long_venue"`
	ShortVenue string  `yaml:"short_venue"`
	LongMarket string  `yaml:"long_market"`  // "PERP" or "SPOT"
	ShortMarket string `yaml:"short_market"`
	FairSpreadBps float64 `yaml:"fair_spread_bps"` // expected spread between the two
}

// CrossAssetDetector monitors price relationships between related assets
// (perp vs spot, perp vs ETF proxy, different expiries) and detects
// when the spread diverges from fair value.
type CrossAssetDetector struct {
	mu       sync.Mutex
	prices   map[string]*assetPrice // "asset:venue" → latest price
}

type assetPrice struct {
	mid    float64
	bid    float64
	ask    float64
	tsMs   int64
}

// NewCrossAssetDetector creates a cross-asset arbitrage detector.
func NewCrossAssetDetector() *CrossAssetDetector {
	return &CrossAssetDetector{
		prices: make(map[string]*assetPrice),
	}
}

// UpdatePrice records the latest price for an asset on a venue.
func (ca *CrossAssetDetector) UpdatePrice(asset, venue string, mid, bid, ask float64, tsMs int64) {
	ca.mu.Lock()
	defer ca.mu.Unlock()
	key := asset + ":" + venue
	ca.prices[key] = &assetPrice{mid: mid, bid: bid, ask: ask, tsMs: tsMs}
}

// Evaluate checks all cross-asset pairs for arbitrage opportunities.
func (ca *CrossAssetDetector) Evaluate(cfg CrossAssetConfig, tenantID string, cooldown *Cooldown) []TradeIntent {
	if !cfg.Enabled {
		return nil
	}

	ca.mu.Lock()
	defer ca.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for _, pair := range cfg.Pairs {
		longKey := pair.LongAsset + ":" + pair.LongVenue
		shortKey := pair.ShortAsset + ":" + pair.ShortVenue

		longPrice := ca.prices[longKey]
		shortPrice := ca.prices[shortKey]

		if longPrice == nil || shortPrice == nil {
			continue
		}

		// Skip stale data
		if now-longPrice.tsMs > 5000 || now-shortPrice.tsMs > 5000 {
			continue
		}

		// Compute spread between the two assets
		if shortPrice.mid == 0 {
			continue
		}
		spreadBps := (longPrice.mid - shortPrice.mid) / shortPrice.mid * 10000

		// Check if spread diverges from fair value
		divergenceBps := spreadBps - pair.FairSpreadBps
		absDivergence := math.Abs(divergenceBps)

		if absDivergence < cfg.MinEdgeBps {
			continue
		}

		cooldownKey := fmt.Sprintf("crossasset:%s", pair.Name)
		if cooldown != nil && cooldown.IsOnCooldown(cooldownKey, now) {
			continue
		}

		log.Printf("cross-asset: %s spread=%.1fbps fair=%.1fbps divergence=%.1fbps",
			pair.Name, spreadBps, pair.FairSpreadBps, divergenceBps)

		// Convergence trade: if spread is above fair → short the spread
		// (sell the rich asset, buy the cheap asset)
		var buyAsset, sellAsset, buyVenue, sellVenue, buyMarket, sellMarket string
		var buyPrice, sellPrice float64

		if divergenceBps > 0 {
			// Spread too wide: sell long asset, buy short asset
			sellAsset = pair.LongAsset
			sellVenue = pair.LongVenue
			sellMarket = pair.LongMarket
			sellPrice = longPrice.bid

			buyAsset = pair.ShortAsset
			buyVenue = pair.ShortVenue
			buyMarket = pair.ShortMarket
			buyPrice = shortPrice.ask
		} else {
			// Spread too narrow: buy long asset, sell short asset
			buyAsset = pair.LongAsset
			buyVenue = pair.LongVenue
			buyMarket = pair.LongMarket
			buyPrice = longPrice.ask

			sellAsset = pair.ShortAsset
			sellVenue = pair.ShortVenue
			sellMarket = pair.ShortMarket
			sellPrice = shortPrice.bid
		}

		intent := TradeIntent{
			TenantID:  tenantID,
			IntentID:  newUUID(),
			Strategy:  StrategyCrossAssetArb,
			Symbol:    pair.Name,
			TsMs:      now,
			ExpiresMs: now + cfg.IntentTTLMs,
			Legs: []TradeLeg{
				{
					Venue:          buyVenue,
					Action:         "BUY",
					Market:         buyMarket,
					NotionalUSD:    cfg.NotionalUSD,
					MaxSlippageBps: cfg.MaxSlippageBps,
					PriceLimit:     buyPrice * (1 + cfg.MaxSlippageBps/10000),
				},
				{
					Venue:          sellVenue,
					Action:         "SELL",
					Market:         sellMarket,
					NotionalUSD:    cfg.NotionalUSD,
					MaxSlippageBps: cfg.MaxSlippageBps,
					PriceLimit:     sellPrice * (1 - cfg.MaxSlippageBps/10000),
				},
			},
			Expected: ExpectedMetrics{
				EdgeBpsGross: absDivergence,
				EdgeBpsNet:   absDivergence - 8, // assume ~8 bps in fees
				ProfitUSDNet: cfg.NotionalUSD * (absDivergence - 8) / 10000,
			},
			Constraints: IntentConstraints{
				MinQuality:  "MED",
				MaxAgeMs:    cfg.IntentTTLMs,
				CooldownKey: cooldownKey,
			},
			Debug: IntentDebug{
				BuyOn:    buyVenue,
				SellOn:   sellVenue,
				BuyExec:  buyPrice,
				SellExec: sellPrice,
			},
		}
		_ = buyAsset
		_ = sellAsset

		intents = append(intents, intent)
		if cooldown != nil {
			cooldown.Mark(cooldownKey, now)
		}
	}

	return intents
}
