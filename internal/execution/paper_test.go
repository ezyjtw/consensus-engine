package execution

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

func testConfig() *Config {
	return &Config{
		TradingMode:    "PAPER",
		SimSlippageBps: 4.0,
		SimLatencyMs:   50,
		AdverseSelBps:  10.0,
		TenantID:       "test",
	}
}

func makeTestIntent(symbol string, expiresMs int64) arb.TradeIntent {
	return arb.TradeIntent{
		IntentID:  "test-intent-1",
		Strategy:  "CROSS_VENUE_ARB",
		Symbol:    symbol,
		TsMs:      time.Now().UnixMilli(),
		ExpiresMs: expiresMs,
		Legs: []arb.Leg{
			{
				Venue:          "binance",
				Action:         "BUY",
				NotionalUSD:    10000,
				MaxSlippageBps: 8.0,
				Market:         "PERP",
			},
			{
				Venue:          "deribit",
				Action:         "SELL",
				NotionalUSD:    10000,
				MaxSlippageBps: 8.0,
				Market:         "PERP",
			},
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsNet: 15.0,
		},
	}
}

// Test that expired intents are rejected.
func TestExpiredIntentRejected(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: 100000},
	})
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("BTC-PERP", time.Now().UnixMilli()-1000) // expired 1s ago
	events, fill := exec.Execute(context.Background(), intent)
	if len(events) != 0 {
		t.Errorf("expected 0 events for expired intent, got %d", len(events))
	}
	if fill == nil {
		t.Fatal("expected fill result even for expired intent")
	}
	if !fill.IntentExpired {
		t.Error("fill should be marked as expired")
	}
}

// Test that missing price returns nil.
func TestMissingPriceReturnsNil(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("ETH-PERP", time.Now().UnixMilli()+60000)
	events, fill := exec.Execute(context.Background(), intent)
	if len(events) != 0 {
		t.Errorf("expected 0 events with no price, got %d", len(events))
	}
	if fill != nil {
		t.Error("expected nil fill when price is missing")
	}
}

// Test successful two-leg fill produces correct events.
func TestTwoLegFillSuccess(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	mid := 100000.0
	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: mid, BandLow: 99990, BandHigh: 100010},
	})
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("BTC-PERP", time.Now().UnixMilli()+60000)
	events, fill := exec.Execute(context.Background(), intent)

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Verify event properties.
	buyEvent := events[0]
	sellEvent := events[1]
	if buyEvent.Action != "BUY" {
		t.Errorf("event[0] action should be BUY, got %s", buyEvent.Action)
	}
	if sellEvent.Action != "SELL" {
		t.Errorf("event[1] action should be SELL, got %s", sellEvent.Action)
	}
	if buyEvent.EventType != "ORDER_FILLED" {
		t.Errorf("event type should be ORDER_FILLED, got %s", buyEvent.EventType)
	}

	// BUY fills above mid (slippage), SELL fills below mid.
	expectedBuy := mid * (1 + cfg.SimSlippageBps/10000)
	expectedSell := mid * (1 - cfg.SimSlippageBps/10000)
	if math.Abs(buyEvent.FilledPrice-expectedBuy) > 0.01 {
		t.Errorf("buy fill price should be ~%.2f, got %.2f", expectedBuy, buyEvent.FilledPrice)
	}
	if math.Abs(sellEvent.FilledPrice-expectedSell) > 0.01 {
		t.Errorf("sell fill price should be ~%.2f, got %.2f", expectedSell, sellEvent.FilledPrice)
	}

	// Verify fill summary.
	if fill == nil {
		t.Fatal("expected non-nil fill")
	}
	if fill.IntentExpired {
		t.Error("fill should not be expired")
	}
	if fill.Strategy != "CROSS_VENUE_ARB" {
		t.Errorf("strategy should be CROSS_VENUE_ARB, got %s", fill.Strategy)
	}
	if fill.TenantID != "test" {
		t.Errorf("tenant should be 'test', got %s", fill.TenantID)
	}
	if fill.Mode != "PAPER" {
		t.Errorf("mode should be PAPER, got %s", fill.Mode)
	}
	if fill.LatencyMs != 50 {
		t.Errorf("latency should be 50, got %d", fill.LatencyMs)
	}
}

// Test that fees are calculated as 4bps of notional.
func TestFeeCalculation(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: 100000},
	})
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("BTC-PERP", time.Now().UnixMilli()+60000)
	events, fill := exec.Execute(context.Background(), intent)
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	// Each leg: 10000 * 4/10000 = 4.0 USD fees.
	expectedFeePerLeg := 10000.0 * 4 / 10000
	for i, ev := range events {
		if math.Abs(ev.FeesUSDActual-expectedFeePerLeg) > 0.01 {
			t.Errorf("event[%d] fees should be ~%.2f, got %.2f", i, expectedFeePerLeg, ev.FeesUSDActual)
		}
	}
	if math.Abs(fill.FeesAssumedUSD-expectedFeePerLeg*2) > 0.01 {
		t.Errorf("total fees should be ~%.2f, got %.2f", expectedFeePerLeg*2, fill.FeesAssumedUSD)
	}
}

// Test PnL is negative when selling below buying (with slippage both ways).
func TestPnLNegativeWithSymmetricSlippage(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: 100000},
	})
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("BTC-PERP", time.Now().UnixMilli()+60000)
	_, fill := exec.Execute(context.Background(), intent)
	if fill == nil {
		t.Fatal("expected non-nil fill")
	}
	// With symmetric slippage, buy > mid > sell, so PnL is negative (before edge).
	// The intent has equal notionals on both sides, so (sell-buy) spread is negative.
	if fill.NetPnLUSD >= 0 {
		t.Errorf("PnL should be negative with symmetric slippage and no spread, got %.4f", fill.NetPnLUSD)
	}
}

// Test price limit enforcement on buy leg.
func TestPriceLimitBuyEnforced(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: 100000},
	})
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("BTC-PERP", time.Now().UnixMilli()+60000)
	// Set buy price limit below the slippage-adjusted fill price.
	intent.Legs[0].PriceLimit = 100000.0 // exactly at mid, below mid*(1+4bps)

	events, _ := exec.Execute(context.Background(), intent)
	if len(events) < 1 {
		t.Fatal("expected at least 1 event")
	}
	if events[0].FilledPrice > 100000.0+0.01 {
		t.Errorf("buy fill should be capped at price limit 100000, got %.2f", events[0].FilledPrice)
	}
}

// Test position tracking after fills.
func TestPositionTracking(t *testing.T) {
	cfg := testConfig()
	cache := NewPriceCache()
	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: 50000},
	})
	exec := NewPaperExecutor(cfg, cache)

	intent := makeTestIntent("BTC-PERP", time.Now().UnixMilli()+60000)
	exec.Execute(context.Background(), intent)

	positions := exec.PositionJSON()
	if len(positions) == 0 {
		t.Fatal("expected positions after fill")
	}
	// Should have entries for both legs.
	if _, ok := positions["binance:BTC-PERP:PERP"]; !ok {
		t.Error("missing binance position entry")
	}
	if _, ok := positions["deribit:BTC-PERP:PERP"]; !ok {
		t.Error("missing deribit position entry")
	}
}

// Test PriceCache operations.
func TestPriceCacheMidLookup(t *testing.T) {
	cache := NewPriceCache()

	// No price yet.
	if _, ok := cache.Mid("BTC-PERP"); ok {
		t.Error("should return false for missing symbol")
	}

	cache.Update(consensus.ConsensusUpdate{
		Symbol:    "BTC-PERP",
		Consensus: consensus.Consensus{Mid: 65432.10, BandLow: 65400, BandHigh: 65460},
	})

	mid, ok := cache.Mid("BTC-PERP")
	if !ok {
		t.Fatal("should find BTC-PERP after update")
	}
	if mid != 65432.10 {
		t.Errorf("mid should be 65432.10, got %.2f", mid)
	}
}

// Test MarshalFill produces valid JSON.
func TestMarshalFill(t *testing.T) {
	fill := &SimulatedFill{
		IntentID: "test-1",
		Strategy: "ARB",
		Symbol:   "BTC-PERP",
	}
	data, err := MarshalFill(fill)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if len(data) == 0 {
		t.Error("marshalled data should not be empty")
	}
}
