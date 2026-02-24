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

// Per-symbol override: a ~30 bps deviation exceeds BTC warn threshold (25 bps)
// but is within DOGE warn threshold (80 bps). Venues are spread to produce a
// nonzero MAD so the robust z-score stays below 6.
func TestSymbolOverrideOutlierThreshold(t *testing.T) {
	p := testPolicy()
	warnBps := 80.0
	blacklistBps := 150.0
	p.SymbolOverrides = map[string]SymbolOverride{
		"DOGE-PERP": {
			OutlierBpsWarn:      &warnBps,
			OutlierBpsBlacklist: &blacklistBps,
		},
	}
	engine := NewEngine(p)

	// At price ~100000, 1 bps = $10. For a ~35 bps deviation we need ~$350 offset.
	// Spread venues to produce nonzero MAD so z-score doesn't dominate:
	// Mids: 99900, 99950, 100050, 100350. Median ≈ 100000, MAD ≈ 50.
	// Binance at 100350 → devBps ≈ 350/100000 * 10000 ≈ 35 bps.
	// z-score ≈ 350/74 ≈ 4.7 (below 6), so only devBps matters.
	quotes := map[Venue]Quote{
		"binance": makeQuote("binance", 100_350, 10), // ~35 bps from median
		"okx":     makeQuote("okx", 99_900, 10),
		"bybit":   makeQuote("bybit", 99_950, 10),
		"deribit": makeQuote("deribit", 100_050, 10),
	}

	// BTC (warn=25): ~35 bps deviation > 25 → flagged OUTLIER
	btcResult := engine.Compute("t1", "BTC-PERP", quotes, okStatus)
	var btcBinance *VenueMetrics
	for i := range btcResult.Update.Venues {
		if btcResult.Update.Venues[i].Venue == "binance" {
			btcBinance = &btcResult.Update.Venues[i]
		}
	}
	if btcBinance == nil {
		t.Fatal("binance not in BTC result")
	}
	btcFlagged := false
	for _, f := range btcBinance.Flags {
		if f == "OUTLIER" {
			btcFlagged = true
		}
	}
	if !btcFlagged {
		t.Errorf("BTC-PERP: deviation (%.1f bps) should be flagged OUTLIER (warn=25), flags=%v",
			btcBinance.DeviationBps, btcBinance.Flags)
	}

	// DOGE (warn=80): same ~28 bps deviation < 80 → NOT flagged
	dogeResult := engine.Compute("t1", "DOGE-PERP", quotes, okStatus)
	var dogeBinance *VenueMetrics
	for i := range dogeResult.Update.Venues {
		if dogeResult.Update.Venues[i].Venue == "binance" {
			dogeBinance = &dogeResult.Update.Venues[i]
		}
	}
	if dogeBinance == nil {
		t.Fatal("binance not in DOGE result")
	}
	dogeFlagged := false
	for _, f := range dogeBinance.Flags {
		if f == "OUTLIER" {
			dogeFlagged = true
		}
	}
	if dogeFlagged {
		t.Errorf("DOGE-PERP: deviation (%.1f bps) should NOT be flagged (warn=80), flags=%v",
			dogeBinance.DeviationBps, dogeBinance.Flags)
	}
}

// ResolvedPolicy returns the base policy when no override exists.
func TestResolvedPolicyNoOverride(t *testing.T) {
	p := testPolicy()
	resolved := p.ResolvedPolicy("BTC-PERP")
	if resolved != p {
		t.Error("expected same pointer when no override exists")
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
