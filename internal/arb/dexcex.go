package arb

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/dex"
)

const StrategyDEXCEXArb = "DEX_CEX_ARB"

// DEXCEXConfig configures the DEX-CEX arbitrage strategy.
type DEXCEXConfig struct {
	Enabled        bool    `yaml:"enabled"`
	MinEdgeBps     float64 `yaml:"min_edge_bps"`
	MaxNotionalUSD float64 `yaml:"max_notional_usd"`
	MaxSlippageBps float64 `yaml:"max_slippage_bps"`
	IntentTTLMs    int64   `yaml:"intent_ttl_ms"`
	CooldownMs     int64   `yaml:"cooldown_ms"`
	// Token addresses for DEX quotes (chain-specific).
	TokenAddresses map[string]string `yaml:"token_addresses"` // symbol → token address
	StableAddress  string            `yaml:"stable_address"`  // USDT/USDC address
	ChainID        int               `yaml:"chain_id"`
}

// DEXCEXTracker monitors CEX prices and compares against DEX quotes
// to find cross-venue arbitrage between decentralised and centralised exchanges.
type DEXCEXTracker struct {
	mu       sync.Mutex
	router   *dex.Router
	prices   map[string]cexPrice // "venue:symbol" → latest CEX price
}

type cexPrice struct {
	bid  float64
	ask  float64
	mid  float64
	tsMs int64
}

// NewDEXCEXTracker creates a DEX-CEX arb tracker.
func NewDEXCEXTracker(router *dex.Router) *DEXCEXTracker {
	return &DEXCEXTracker{
		router: router,
		prices: make(map[string]cexPrice),
	}
}

// RecordQuote updates the latest CEX price for a venue+symbol.
func (dt *DEXCEXTracker) RecordQuote(q consensus.Quote) {
	if q.BestBid <= 0 || q.BestAsk <= 0 {
		return
	}
	dt.mu.Lock()
	defer dt.mu.Unlock()
	key := string(q.Venue) + ":" + string(q.Symbol)
	dt.prices[key] = cexPrice{
		bid:  q.BestBid,
		ask:  q.BestAsk,
		mid:  (q.BestBid + q.BestAsk) / 2,
		tsMs: q.TsMs,
	}
}

// EvaluateDEXCEX compares DEX quotes against CEX prices to find arb opportunities.
// This is an async operation since DEX quotes require HTTP calls.
func (dt *DEXCEXTracker) EvaluateDEXCEX(ctx context.Context, cfg DEXCEXConfig, tenantID string, cooldown *Cooldown) []TradeIntent {
	if !cfg.Enabled || dt.router == nil || !dt.router.Enabled() {
		return nil
	}

	dt.mu.Lock()
	// Snapshot current CEX prices.
	snapshot := make(map[string]cexPrice, len(dt.prices))
	for k, v := range dt.prices {
		snapshot[k] = v
	}
	dt.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for symbol, tokenAddr := range cfg.TokenAddresses {
		// Find the best CEX bid and ask across venues for this symbol.
		var bestBid cexPrice
		var bestBidVenue string
		var bestAsk cexPrice
		var bestAskVenue string

		for key, p := range snapshot {
			v, s := splitKey(key)
			if s != symbol {
				continue
			}
			// Skip stale quotes (> 10s).
			if now-p.tsMs > 10_000 {
				continue
			}
			if p.bid > bestBid.bid {
				bestBid = p
				bestBidVenue = v
			}
			if bestAsk.ask == 0 || p.ask < bestAsk.ask {
				bestAsk = p
				bestAskVenue = v
			}
		}

		if bestBid.bid <= 0 || bestAsk.ask <= 0 {
			continue
		}

		// Get DEX quote: how much USDT would we get selling 1 token on DEX?
		// This is approximate — we use a standard notional amount.
		amountWei := notionalToWei(cfg.MaxNotionalUSD, bestBid.mid)
		if amountWei == "" {
			continue
		}

		dexQuote, err := dt.router.BestQuote(ctx, dex.QuoteRequest{
			ChainID:   cfg.ChainID,
			FromToken: tokenAddr,
			ToToken:   cfg.StableAddress,
			AmountWei: amountWei,
		})
		if err != nil {
			continue // DEX quote failed, skip
		}

		// Parse DEX output.
		dexOutputUSD := weiToUSD(dexQuote.ToAmount, 6) // USDT has 6 decimals
		if dexOutputUSD <= 0 {
			continue
		}
		dexImpliedPrice := dexOutputUSD / (cfg.MaxNotionalUSD / bestBid.mid)

		// Check: Can we buy on CEX and sell on DEX profitably?
		// Edge = (DEX sell price - CEX buy price) / CEX buy price * 10000
		buyOnCEXEdgeBps := (dexImpliedPrice - bestAsk.ask) / bestAsk.ask * 10000
		if buyOnCEXEdgeBps >= cfg.MinEdgeBps {
			cooldownKey := fmt.Sprintf("dexcex:buy:%s:%s", symbol, bestAskVenue)
			if cooldown == nil || !cooldown.IsOnCooldown(cooldownKey, now) {
				log.Printf("dex-cex: BUY CEX→SELL DEX sym=%s venue=%s edge=%.1fbps",
					symbol, bestAskVenue, buyOnCEXEdgeBps)
				intent := TradeIntent{
					TenantID:  tenantID,
					IntentID:  newUUID(),
					Strategy:  StrategyDEXCEXArb,
					Symbol:    symbol,
					TsMs:      now,
					ExpiresMs: now + cfg.IntentTTLMs,
					Legs: []TradeLeg{
						{
							Venue:          bestAskVenue,
							Action:         "BUY",
							Market:         "SPOT",
							Type:           "MARKET_OR_IOC",
							NotionalUSD:    cfg.MaxNotionalUSD,
							MaxSlippageBps: cfg.MaxSlippageBps,
							PriceLimit:     bestAsk.ask * 1.003,
						},
						{
							Venue:          "dex:" + string(dexQuote.Provider),
							Action:         "SELL",
							Market:         "SPOT",
							Type:           "DEX_SWAP",
							NotionalUSD:    cfg.MaxNotionalUSD,
							MaxSlippageBps: cfg.MaxSlippageBps,
						},
					},
					Expected: ExpectedMetrics{
						EdgeBpsGross: buyOnCEXEdgeBps,
						EdgeBpsNet:   buyOnCEXEdgeBps - 10, // ~10 bps gas + DEX fees
						ProfitUSDNet: cfg.MaxNotionalUSD * (buyOnCEXEdgeBps - 10) / 10000,
					},
					Constraints: IntentConstraints{
						MinQuality:  "MED",
						MaxAgeMs:    cfg.IntentTTLMs,
						CooldownKey: cooldownKey,
					},
				}
				intents = append(intents, intent)
				if cooldown != nil {
					cooldown.Mark(cooldownKey, now)
				}
			}
		}

		// Check reverse: buy on DEX, sell on CEX.
		sellOnCEXEdgeBps := (bestBid.bid - dexImpliedPrice) / dexImpliedPrice * 10000
		if sellOnCEXEdgeBps >= cfg.MinEdgeBps {
			cooldownKey := fmt.Sprintf("dexcex:sell:%s:%s", symbol, bestBidVenue)
			if cooldown == nil || !cooldown.IsOnCooldown(cooldownKey, now) {
				log.Printf("dex-cex: BUY DEX→SELL CEX sym=%s venue=%s edge=%.1fbps",
					symbol, bestBidVenue, sellOnCEXEdgeBps)
				intent := TradeIntent{
					TenantID:  tenantID,
					IntentID:  newUUID(),
					Strategy:  StrategyDEXCEXArb,
					Symbol:    symbol,
					TsMs:      now,
					ExpiresMs: now + cfg.IntentTTLMs,
					Legs: []TradeLeg{
						{
							Venue:          "dex:" + string(dexQuote.Provider),
							Action:         "BUY",
							Market:         "SPOT",
							Type:           "DEX_SWAP",
							NotionalUSD:    cfg.MaxNotionalUSD,
							MaxSlippageBps: cfg.MaxSlippageBps,
						},
						{
							Venue:          bestBidVenue,
							Action:         "SELL",
							Market:         "SPOT",
							Type:           "MARKET_OR_IOC",
							NotionalUSD:    cfg.MaxNotionalUSD,
							MaxSlippageBps: cfg.MaxSlippageBps,
							PriceLimit:     bestBid.bid * 0.997,
						},
					},
					Expected: ExpectedMetrics{
						EdgeBpsGross: sellOnCEXEdgeBps,
						EdgeBpsNet:   sellOnCEXEdgeBps - 10,
						ProfitUSDNet: cfg.MaxNotionalUSD * (sellOnCEXEdgeBps - 10) / 10000,
					},
					Constraints: IntentConstraints{
						MinQuality:  "MED",
						MaxAgeMs:    cfg.IntentTTLMs,
						CooldownKey: cooldownKey,
					},
				}
				intents = append(intents, intent)
				if cooldown != nil {
					cooldown.Mark(cooldownKey, now)
				}
			}
		}
	}
	return intents
}

// notionalToWei converts a USD notional at a given price to a wei string (18 decimals).
func notionalToWei(notionalUSD, price float64) string {
	if price <= 0 {
		return ""
	}
	tokenAmount := notionalUSD / price
	// Convert to wei (18 decimals).
	weiFloat := tokenAmount * 1e18
	weiBig := new(big.Int)
	weiBig.SetString(fmt.Sprintf("%.0f", weiFloat), 10)
	return weiBig.String()
}

// weiToUSD converts a wei string to USD (given decimals, e.g. 6 for USDT).
func weiToUSD(weiStr string, decimals int) float64 {
	weiBig := new(big.Int)
	weiBig.SetString(weiStr, 10)
	if weiBig.Sign() <= 0 {
		return 0
	}
	divisor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)
	result := new(big.Float).Quo(
		new(big.Float).SetInt(weiBig),
		new(big.Float).SetInt(divisor),
	)
	f, _ := result.Float64()
	return f
}
