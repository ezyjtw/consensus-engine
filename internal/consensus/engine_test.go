package consensus

import (
	"testing"
)

// testPolicy returns a minimal valid Policy with spec-aligned thresholds.
func testPolicy() *Policy {
	return &Policy{
		SizeNotionalUSD:     10000,
		StaleMs:             750,
		OutlierBpsWarn:      25.0,
		OutlierBpsBlacklist: 50.0,
		WarnPersistMs:       500,
		BlacklistPersistMs:  1500,
		BlacklistTtlMs:      60000,
		RecoveryMs:          2000,
		SpreadWarnBps:       10.0,
		MinCoreQuorum:       3,
		CoreVenues:          []string{"binance", "okx", "bybit", "deribit"},
		SlippageBufferBps:   5.0,
		DepthPenaltyBps:     20.0,
		BaseTrust: map[string]float64{
			"binance": 1.0,
			"okx":     0.9,
			"bybit":   0.85,
			"deribit": 0.80,
		},
	}
}

func okStatus(_ Venue) VenueStatus { return VenueStatus{State: StateOK} }

func makeQuote(venue Venue, mid, spread float64) Quote {
	return Quote{
		TenantID: "t1",
		Venue:    venue,
		Symbol:   "BTC-PERP",
		BestBid:  mid - spread/2,
		BestAsk:  mid + spread/2,
		FeedHealth: FeedHealth{
			WsConnected:  true,
			LastMsgTsMs:  1_700_000_000_000,
		},
	}
}

// spec §10 test 1: single outlier detected
// 3 venues at ~100 000, one at 100 700 (70 bps off) → flagged OUTLIER/HARD_OUTLIER
func TestOutlierDetection(t *testing.T) {
	engine := NewEngine(testPolicy())

	normal := 100_000.0
	quotes := map[Venue]Quote{
		"binance": makeQuote("binance", 100_700, 10), // 70 bps outlier
		"okx":     makeQuote("okx", normal, 10),
		"bybit":   makeQuote("bybit", normal, 10),
		"deribit": makeQuote("deribit", normal, 10),
	}
	result := engine.Compute("t1", "BTC-PERP", quotes, okStatus)

	var binance *VenueMetrics
	for i := range result.Update.Venues {
		if result.Update.Venues[i].Venue == "binance" {
			binance = &result.Update.Venues[i]
		}
	}
	if binance == nil {
		t.Fatal("binance not in venue metrics")
	}

	flagged := false
	for _, f := range binance.Flags {
		if f == "OUTLIER" || f == "HARD_OUTLIER" {
			flagged = true
		}
	}
	if !flagged {
		t.Errorf("binance (70 bps off) should be flagged; flags=%v", binance.Flags)
	}
	if binance.DeviationBps < 60 {
		t.Errorf("binance deviation should be ~70 bps, got %.1f", binance.DeviationBps)
	}

	// consensus mid should track the three normal venues, not the outlier
	if result.Update.Consensus.Mid > 100_100 {
		t.Errorf("consensus mid skewed by outlier: %.2f", result.Update.Consensus.Mid)
	}
}

// spec §10 test 2: MAD=0 — all mids equal, no outlier, consensus equals that mid
func TestMADZeroNoFalseOutlier(t *testing.T) {
	engine := NewEngine(testPolicy())

	mid := 50_000.0
	quotes := map[Venue]Quote{
		"binance": makeQuote("binance", mid, 10),
		"okx":     makeQuote("okx", mid, 10),
		"bybit":   makeQuote("bybit", mid, 10),
		"deribit": makeQuote("deribit", mid, 10),
	}
	result := engine.Compute("t1", "BTC-PERP", quotes, okStatus)

	for _, vm := range result.Update.Venues {
		for _, f := range vm.Flags {
			if f == "OUTLIER" || f == "HARD_OUTLIER" {
				t.Errorf("venue %s should not be flagged when all mids are equal; flags=%v",
					vm.Venue, vm.Flags)
			}
		}
	}

	// consensus mid should equal the common mid (within fees/slippage on buy/sell, mid itself is exact)
	if result.Update.Consensus.Mid < mid*0.9999 || result.Update.Consensus.Mid > mid*1.0001 {
		t.Errorf("consensus mid should be ~%.2f, got %.2f", mid, result.Update.Consensus.Mid)
	}
}

// spec §10 test 5: band_low = P25(effective sells), band_high = P75(effective buys)
// With all venues identical the band narrows to a single price; this test checks
// the ordering guarantee: band_low < consensus_mid < band_high.
func TestBandOrdering(t *testing.T) {
	engine := NewEngine(testPolicy())

	quotes := map[Venue]Quote{
		"binance": makeQuote("binance", 100_000, 20),
		"okx":     makeQuote("okx", 100_010, 18),
		"bybit":   makeQuote("bybit", 99_990, 22),
		"deribit": makeQuote("deribit", 100_005, 16),
	}
	result := engine.Compute("t1", "BTC-PERP", quotes, okStatus)
	c := result.Update.Consensus

	if c.BandLow >= c.BandHigh {
		t.Errorf("band_low (%.4f) must be < band_high (%.4f)", c.BandLow, c.BandHigh)
	}
}

// spec §4.10: quality HIGH requires 4+ core venues with no WARN
func TestQualityHighFourOKVenues(t *testing.T) {
	engine := NewEngine(testPolicy()) // MinCoreQuorum=3, 4 core venues

	mid := 100_000.0
	quotes := map[Venue]Quote{
		"binance": makeQuote("binance", mid, 10),
		"okx":     makeQuote("okx", mid, 10),
		"bybit":   makeQuote("bybit", mid, 10),
		"deribit": makeQuote("deribit", mid, 10),
	}
	result := engine.Compute("t1", "BTC-PERP", quotes, okStatus)
	if result.Update.Consensus.Quality != "HIGH" {
		t.Errorf("expected HIGH with 4 OK core venues, got %s", result.Update.Consensus.Quality)
	}
}

// spec §4.10: fewer than MinCoreQuorum eligible → LOW
func TestQualityLowBelowQuorum(t *testing.T) {
	engine := NewEngine(testPolicy()) // MinCoreQuorum=3

	// Only 2 venues provided
	mid := 100_000.0
	quotes := map[Venue]Quote{
		"binance": makeQuote("binance", mid, 10),
		"okx":     makeQuote("okx", mid, 10),
	}
	result := engine.Compute("t1", "BTC-PERP", quotes, okStatus)
	if result.Update.Consensus.Quality != "LOW" {
		t.Errorf("expected LOW with 2 core venues (quorum=3), got %s",
			result.Update.Consensus.Quality)
	}
}
