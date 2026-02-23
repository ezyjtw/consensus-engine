package arb_test

import (
	"testing"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

const baseTS = int64(1700000000000)

// testPolicy returns a minimal valid policy for unit tests.
func testPolicy() *arb.Policy {
	return &arb.Policy{
		Symbols:             []string{"BTC-PERP"},
		MinConsensusQuality: "MED",
		MinEdgeBpsNet: map[string]float64{
			"HIGH": 6.0,
			"MED":  10.0,
			"LOW":  9999.0,
		},
		IntentTTLMs: map[string]int64{
			"HIGH": 400,
			"MED":  250,
			"LOW":  0,
		},
		LatencyBufferBps: map[string]float64{
			"HIGH": 2.0,
			"MED":  4.0,
			"LOW":  10.0,
		},
		SizeLadderUSD:       []float64{10000},
		MaxSlippageBps:      8.0,
		CooldownMs:          300,
		AllowWarnVenues:     false,
		IgnoreFlaggedVenues: true,
		EnabledPairs: map[string][][]string{
			"BTC-PERP": {{"binance", "deribit"}},
		},
		Redis: arb.ArbRedisPolicy{Addr: "localhost:6379"},
	}
}

// makeVenue builds a VenueMetrics fixture. BuyExec/SellExec are set to simulate
// a small fee difference so estimateFeesUSD is non-zero and non-NaN.
func makeVenue(name string, effBuy, effSell float64, status consensus.VenueState) consensus.VenueMetrics {
	return consensus.VenueMetrics{
		Venue:         consensus.Venue(name),
		Status:        status,
		Trust:         0.5,
		Mid:           (effBuy + effSell) / 2,
		BuyExec:       effBuy * 0.9996,  // raw price before ~4bps taker fee
		SellExec:      effSell * 1.0004, // raw price before ~4bps taker fee
		EffectiveBuy:  effBuy,
		EffectiveSell: effSell,
		DeviationBps:  0,
	}
}

// makeUpdate builds a ConsensusUpdate fixture.
func makeUpdate(quality string, venues []consensus.VenueMetrics) arb.ConsensusUpdate {
	return arb.ConsensusUpdate{
		TenantID: "default",
		Symbol:   "BTC-PERP",
		TsMs:     baseTS,
		Consensus: consensus.Consensus{
			Mid:      100000.0,
			BandLow:  99990.0,
			BandHigh: 100010.0,
			Quality:  quality,
		},
		Venues: venues,
	}
}

// ---- Tests ----

// 1. LOW quality → no intents regardless of price gap.
func TestRejectLowQuality(t *testing.T) {
	e := arb.NewEngine(testPolicy())
	u := makeUpdate("LOW", []consensus.VenueMetrics{
		makeVenue("binance", 99880.0, 99870.0, consensus.StateOK),
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
	})
	intents := e.Process(u)
	if len(intents) != 0 {
		t.Errorf("expected 0 intents for LOW quality, got %d", len(intents))
	}
	if e.Rejected[arb.RejectLowQuality] == 0 {
		t.Error("expected low_quality rejection counter to be incremented")
	}
}

// 2. Clear net edge above threshold → intent emitted with correct legs.
//
// Math (MED quality, latency_buffer=4bps):
//
//	buy binance EffectiveBuy=99880 → buyCost  = 99880 * 1.0004 = 99919.952
//	sell deribit EffectiveSell=100110 → sellValue = 100110 * 0.9996 = 100069.956
//	grossEdge = (100069.956 - 99919.952) / 100000 * 10000 ≈ 15.0 bps > 10 ✓
func TestDetectNetEdge(t *testing.T) {
	e := arb.NewEngine(testPolicy())
	u := makeUpdate("MED", []consensus.VenueMetrics{
		makeVenue("binance", 99880.0, 99870.0, consensus.StateOK),
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
	})
	intents := e.Process(u)
	if len(intents) == 0 {
		t.Fatal("expected at least one intent, got 0")
	}
	intent := intents[0]
	if intent.Expected.EdgeBpsNet <= 10 {
		t.Errorf("expected net edge > 10 bps, got %.4f", intent.Expected.EdgeBpsNet)
	}
	if intent.Strategy != "CROSS_VENUE_ARB" {
		t.Errorf("unexpected strategy %q", intent.Strategy)
	}
	if len(intent.Legs) != 2 {
		t.Fatalf("expected 2 legs, got %d", len(intent.Legs))
	}
	if intent.Legs[0].Action != "BUY" {
		t.Errorf("leg[0] action: want BUY, got %s", intent.Legs[0].Action)
	}
	if intent.Legs[1].Action != "SELL" {
		t.Errorf("leg[1] action: want SELL, got %s", intent.Legs[1].Action)
	}
	// Price limits must be set on both legs.
	if intent.Legs[0].PriceLimit <= 0 {
		t.Error("buy leg PriceLimit must be > 0")
	}
	if intent.Legs[1].PriceLimit <= 0 {
		t.Error("sell leg PriceLimit must be > 0")
	}
}

// 3. Gap exists but below min_edge after latency buffers → no intent.
//
// Math (MED quality, latency_buffer=4bps):
//
//	buy binance EffectiveBuy=99960 → buyCost   = 99960 * 1.0004 = 99999.984
//	sell deribit EffectiveSell=100000 → sellValue = 100000 * 0.9996 = 99960.0
//	grossEdge = (99960 - 99999.984) / 100000 * 10000 ≈ -4 bps < 10 → rejected ✓
func TestEdgeBelowThreshold(t *testing.T) {
	e := arb.NewEngine(testPolicy())
	u := makeUpdate("MED", []consensus.VenueMetrics{
		makeVenue("binance", 99960.0, 99950.0, consensus.StateOK),
		makeVenue("deribit", 100010.0, 100000.0, consensus.StateOK),
	})
	intents := e.Process(u)
	if len(intents) != 0 {
		t.Errorf("expected 0 intents (edge below threshold), got %d", len(intents))
	}
	if e.Rejected[arb.RejectInsufficientEdge] == 0 {
		t.Error("expected insufficient_edge rejection counter to be incremented")
	}
}

// 4. Two updates within cooldown → only the first emits; after TTL the third emits.
func TestCooldownBlocksSpam(t *testing.T) {
	e := arb.NewEngine(testPolicy())
	venues := []consensus.VenueMetrics{
		makeVenue("binance", 99880.0, 99870.0, consensus.StateOK),
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
	}

	u1 := makeUpdate("MED", venues)
	u1.TsMs = baseTS
	first := e.Process(u1)
	if len(first) == 0 {
		t.Fatal("expected first intent to be emitted")
	}

	// 100ms later — still inside 300ms cooldown.
	u2 := makeUpdate("MED", venues)
	u2.TsMs = baseTS + 100
	second := e.Process(u2)
	if len(second) != 0 {
		t.Errorf("expected 0 intents during cooldown, got %d", len(second))
	}

	// 400ms later — cooldown has expired.
	u3 := makeUpdate("MED", venues)
	u3.TsMs = baseTS + 400
	third := e.Process(u3)
	if len(third) == 0 {
		t.Error("expected intent after cooldown expiry, got 0")
	}
}

// 5. WARN-status venue is excluded when allow_warn_venues=false,
// leaving only one eligible venue → no intent.
func TestVenueStatusFilter(t *testing.T) {
	p := testPolicy()
	p.AllowWarnVenues = false
	e := arb.NewEngine(p)
	u := makeUpdate("HIGH", []consensus.VenueMetrics{
		makeVenue("binance", 99880.0, 99870.0, consensus.StateWarn), // excluded
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
	})
	intents := e.Process(u)
	if len(intents) != 0 {
		t.Errorf("expected 0 intents (WARN venue excluded), got %d", len(intents))
	}
}

// 6. Intent expiry is set to TsMs + TTL matching the quality level.
func TestExpiryMatchesQualityTTL(t *testing.T) {
	e := arb.NewEngine(testPolicy())
	venues := []consensus.VenueMetrics{
		makeVenue("binance", 99880.0, 99870.0, consensus.StateOK),
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
	}

	// MED quality → TTL = 250ms
	uMED := makeUpdate("MED", venues)
	uMED.TsMs = baseTS
	intentsMED := e.Process(uMED)
	if len(intentsMED) == 0 {
		t.Fatal("expected MED intent")
	}
	if want := baseTS + 250; intentsMED[0].ExpiresMs != want {
		t.Errorf("MED: ExpiresMs = %d, want %d", intentsMED[0].ExpiresMs, want)
	}

	// HIGH quality → TTL = 400ms; use fresh engine to avoid cooldown.
	e2 := arb.NewEngine(testPolicy())
	uHIGH := makeUpdate("HIGH", venues)
	uHIGH.TsMs = baseTS + 5000 // well outside any cooldown
	intentsHIGH := e2.Process(uHIGH)
	if len(intentsHIGH) == 0 {
		t.Fatal("expected HIGH intent")
	}
	if want := baseTS + 5000 + 400; intentsHIGH[0].ExpiresMs != want {
		t.Errorf("HIGH: ExpiresMs = %d, want %d", intentsHIGH[0].ExpiresMs, want)
	}
}

// 7. OUTLIER-flagged venue is excluded when ignore_flagged_venues=true.
func TestOutlierFlagExclusion(t *testing.T) {
	p := testPolicy()
	p.IgnoreFlaggedVenues = true
	e := arb.NewEngine(p)
	// binance is flagged OUTLIER — should be excluded.
	flaggedBinance := makeVenue("binance", 99880.0, 99870.0, consensus.StateOK)
	flaggedBinance.Flags = []string{"OUTLIER"}
	u := makeUpdate("HIGH", []consensus.VenueMetrics{
		flaggedBinance,
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
	})
	intents := e.Process(u)
	if len(intents) != 0 {
		t.Errorf("expected 0 intents (OUTLIER venue excluded), got %d", len(intents))
	}
}

// 8. Two disjoint opportunities on different pair sets → both emitted.
func TestDisjointPairsEmitTwo(t *testing.T) {
	p := testPolicy()
	// Add a second pair that uses okx+bybit (disjoint from binance+deribit).
	p.EnabledPairs["BTC-PERP"] = [][]string{
		{"binance", "deribit"},
		{"okx", "bybit"},
	}
	e := arb.NewEngine(p)
	u := makeUpdate("HIGH", []consensus.VenueMetrics{
		makeVenue("binance", 99880.0, 99870.0, consensus.StateOK),
		makeVenue("deribit", 100120.0, 100110.0, consensus.StateOK),
		makeVenue("okx", 99860.0, 99850.0, consensus.StateOK),
		makeVenue("bybit", 100140.0, 100130.0, consensus.StateOK),
	})
	intents := e.Process(u)
	if len(intents) != 2 {
		t.Errorf("expected 2 disjoint intents, got %d", len(intents))
	}
}
