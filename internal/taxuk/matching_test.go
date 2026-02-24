package taxuk

import (
	"math"
	"testing"
	"time"
)

func d(year, month, day int) time.Time {
	return time.Date(year, time.Month(month), day, 12, 0, 0, 0, time.UTC)
}

func makeTrade(id string, ts time.Time, action string, qty, notionalGBP, feesGBP float64) TradeRecord {
	return TradeRecord{
		ID:          id,
		Timestamp:   ts,
		Symbol:      "BTC",
		Action:      action,
		Market:      "SPOT",
		Quantity:    qty,
		NotionalGBP: notionalGBP,
		FeesGBP:     feesGBP,
	}
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 0.01 {
		t.Errorf("%s: got %.4f, want %.4f", name, got, want)
	}
}

// spec §10 test 1: same-day matching takes priority.
func TestSameDayMatching(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("buy1", d(2025, 3, 1), "BUY", 10, 1000, 10),  // 10 BTC for 1000 GBP
		makeTrade("sell1", d(2025, 3, 1), "SELL", 5, 600, 5),    // sell 5 BTC for 600 GBP same day
		makeTrade("buy2", d(2025, 3, 10), "BUY", 10, 1200, 10),  // later buy, should not match
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	if disp.MatchType != "same-day" {
		t.Errorf("match type: got %q, want %q", disp.MatchType, "same-day")
	}
	assertClose(t, "quantity", disp.Quantity, 5)
	// Cost = 5 * ((1000+10)/10) = 5 * 101 = 505
	assertClose(t, "cost", disp.CostGBP, 505)
	// Proceeds = 600 - 5 = 595
	assertClose(t, "proceeds", disp.ProceedsGBP, 595)
	// Gain = 595 - 505 = 90
	assertClose(t, "gain", disp.GainGBP, 90)

	// Remaining: 5 from buy1 + 10 from buy2 = 15 in pool
	assertClose(t, "pool qty", pool.TotalQuantity, 15)
}

// spec §10 test 2: 30-day bed-and-breakfasting rule.
func TestBedAndBreakfasting(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("buy1", d(2025, 1, 1), "BUY", 10, 1000, 0),   // old buy
		makeTrade("sell1", d(2025, 3, 1), "SELL", 5, 750, 0),    // sell
		makeTrade("buy2", d(2025, 3, 15), "BUY", 5, 600, 0),    // rebuy within 30 days
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	if disp.MatchType != "30-day" {
		t.Errorf("match type: got %q, want %q", disp.MatchType, "30-day")
	}
	// Cost basis is the 30-day rebuy: 5 * (600/5) = 600
	assertClose(t, "cost", disp.CostGBP, 600)
	// Proceeds = 750
	assertClose(t, "proceeds", disp.ProceedsGBP, 750)
	// Gain = 750 - 600 = 150
	assertClose(t, "gain", disp.GainGBP, 150)

	// buy1 (10 units) should be in the pool
	assertClose(t, "pool qty", pool.TotalQuantity, 10)
}

// spec §10 test 3: Section 104 pool fallback.
func TestSection104Pool(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("buy1", d(2025, 1, 1), "BUY", 10, 1000, 0),
		makeTrade("buy2", d(2025, 2, 1), "BUY", 10, 2000, 0),
		makeTrade("sell1", d(2025, 6, 1), "SELL", 15, 3000, 0), // no same-day or 30-day match
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	if disp.MatchType != "section-104" {
		t.Errorf("match type: got %q, want %q", disp.MatchType, "section-104")
	}
	// Pool: 10 units @ 100 + 10 units @ 200 = 20 units, cost 3000
	// Weighted avg = 150/unit → cost for 15 = 2250
	assertClose(t, "cost", disp.CostGBP, 2250)
	assertClose(t, "proceeds", disp.ProceedsGBP, 3000)
	assertClose(t, "gain", disp.GainGBP, 750)

	// Pool should have 5 units remaining, cost = 5 * 150 = 750
	assertClose(t, "pool qty", pool.TotalQuantity, 5)
	assertClose(t, "pool cost", pool.TotalCostGBP, 750)
}

// spec §10 test 4: mixed matching — same-day, 30-day, and pool combined.
func TestMixedMatching(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("old", d(2025, 1, 1), "BUY", 20, 2000, 0),       // pool fodder: 100/unit
		makeTrade("sell1", d(2025, 6, 15), "SELL", 10, 1500, 0),    // dispose 10
		makeTrade("sameday", d(2025, 6, 15), "BUY", 3, 450, 0),    // same-day: 150/unit
		makeTrade("bnb", d(2025, 6, 20), "BUY", 4, 640, 0),        // 30-day: 160/unit
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	// 3 same-day @ 150 = 450
	// 4 30-day @ 160 = 640
	// 3 from pool @ 100 = 300
	// Total cost = 1390
	assertClose(t, "quantity", disp.Quantity, 10)
	assertClose(t, "cost", disp.CostGBP, 1390)
	assertClose(t, "proceeds", disp.ProceedsGBP, 1500)
	assertClose(t, "gain", disp.GainGBP, 110)
	if disp.MatchType != "same-day" {
		t.Errorf("primary match type: got %q, want %q", disp.MatchType, "same-day")
	}

	// Pool: 20 - 3 (used by pool match) = 17 remaining
	assertClose(t, "pool qty", pool.TotalQuantity, 17)
}

// spec §10 test 5: pre-existing pool carries forward.
func TestPreExistingPool(t *testing.T) {
	pool := &Section104Pool{
		Symbol:        "ETH",
		TotalQuantity: 50,
		TotalCostGBP:  5000, // 100 GBP/unit
	}

	trades := []TradeRecord{
		makeTrade("sell1", d(2025, 6, 1), "SELL", 20, 3000, 0),
	}
	trades[0].Symbol = "ETH"

	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	// Cost from pool: 20 * 100 = 2000
	assertClose(t, "cost", disp.CostGBP, 2000)
	assertClose(t, "proceeds", disp.ProceedsGBP, 3000)
	assertClose(t, "gain", disp.GainGBP, 1000)

	// Pool: 30 units, 3000 GBP
	assertClose(t, "pool qty", pool.TotalQuantity, 30)
	assertClose(t, "pool cost", pool.TotalCostGBP, 3000)
}

// spec §10 test 6: no trades produces no disposals.
func TestEmptyTrades(t *testing.T) {
	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(nil, pool)
	if len(disposals) != 0 {
		t.Errorf("expected 0 disposals, got %d", len(disposals))
	}
}

// spec §10 test 7: buys only — all go to pool, no disposals.
func TestBuysOnly(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("buy1", d(2025, 1, 1), "BUY", 10, 1000, 0),
		makeTrade("buy2", d(2025, 2, 1), "BUY", 5, 750, 0),
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 0 {
		t.Errorf("expected 0 disposals, got %d", len(disposals))
	}
	assertClose(t, "pool qty", pool.TotalQuantity, 15)
	assertClose(t, "pool cost", pool.TotalCostGBP, 1750)
}

// spec §10 test 8: fees are included in acquisition cost.
func TestFeesIncludedInCost(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("buy1", d(2025, 1, 1), "BUY", 10, 1000, 50),  // cost = (1000+50)/10 = 105/unit
		makeTrade("sell1", d(2025, 6, 1), "SELL", 10, 1500, 25), // proceeds = 1500-25 = 1475
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	// Cost: 10 * 105 = 1050
	assertClose(t, "cost", disp.CostGBP, 1050)
	// Proceeds: 1475
	assertClose(t, "proceeds", disp.ProceedsGBP, 1475)
	// Gain: 425
	assertClose(t, "gain", disp.GainGBP, 425)
}

// spec §10 test 9: 30-day rule does not match buys before the disposal date.
func TestBnBDoesNotLookBackward(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("buy1", d(2025, 1, 1), "BUY", 10, 1000, 0),    // before disposal
		makeTrade("sell1", d(2025, 3, 1), "SELL", 5, 750, 0),     // disposal
		// No rebuy within 30 days after sell1
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	// Should match from pool (buy1 went to pool), not 30-day
	disp := disposals[0]
	if disp.MatchType != "section-104" {
		t.Errorf("match type: got %q, want %q", disp.MatchType, "section-104")
	}
	// Pool cost per unit = 100, so cost = 500
	assertClose(t, "cost", disp.CostGBP, 500)
}

// spec §10 test 10: 30-day boundary is exactly 30 days inclusive.
func TestBnBBoundary(t *testing.T) {
	trades := []TradeRecord{
		makeTrade("sell1", d(2025, 3, 1), "SELL", 5, 750, 0),
		makeTrade("buy30", d(2025, 3, 31), "BUY", 5, 600, 0), // exactly 30 days later
		makeTrade("buy31", d(2025, 4, 1), "BUY", 5, 500, 0),  // 31 days — too late
	}

	pool := &Section104Pool{Symbol: "BTC"}
	disposals := MatchSpotTrades(trades, pool)

	if len(disposals) != 1 {
		t.Fatalf("expected 1 disposal, got %d", len(disposals))
	}

	disp := disposals[0]
	if disp.MatchType != "30-day" {
		t.Errorf("match type: got %q, want %q", disp.MatchType, "30-day")
	}
	// Matched with buy30: cost = 600
	assertClose(t, "cost", disp.CostGBP, 600)

	// buy31 should be in the pool
	assertClose(t, "pool qty", pool.TotalQuantity, 5)
}
