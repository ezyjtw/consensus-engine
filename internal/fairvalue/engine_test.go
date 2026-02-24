package fairvalue

import (
	"testing"
	"time"
)

func TestFairValueBasic(t *testing.T) {
	cfg := DefaultConfig()
	e := NewEngine(cfg)
	now := time.Now().UnixMilli()

	inputs := []VenueInput{
		{Venue: "binance", Mid: 100000, BestBid: 99995, BestAsk: 100005, DepthUSD: 500000, LatencyUs: 50000, LeadPct: 60, Reliability: 0.9, TsMs: now},
		{Venue: "okx", Mid: 100010, BestBid: 100005, BestAsk: 100015, DepthUSD: 300000, LatencyUs: 80000, LeadPct: 20, Reliability: 0.8, TsMs: now},
		{Venue: "bybit", Mid: 100005, BestBid: 100000, BestAsk: 100010, DepthUSD: 200000, LatencyUs: 100000, LeadPct: 15, Reliability: 0.7, TsMs: now},
	}

	fv := e.Compute("BTC-PERP", inputs)
	if fv == nil {
		t.Fatal("expected fair value, got nil")
	}

	// Fair value should be close to 100005 (weighted toward binance)
	if fv.Mid < 99990 || fv.Mid > 100020 {
		t.Errorf("fair value mid out of range: %.2f", fv.Mid)
	}

	// Binance should have highest weight (best leader + latency + depth)
	var binanceW float64
	for _, vw := range fv.VenueWeights {
		if vw.Venue == "binance" {
			binanceW = vw.Weight
		}
	}
	if binanceW <= 0 {
		t.Error("binance should have positive weight")
	}
	for _, vw := range fv.VenueWeights {
		if vw.Venue != "binance" && vw.Weight > binanceW {
			t.Errorf("%s weight %.4f exceeds binance %.4f", vw.Venue, vw.Weight, binanceW)
		}
	}

	if fv.Confidence <= 0 {
		t.Error("expected positive confidence")
	}
}

func TestFairValueStaleFiltering(t *testing.T) {
	cfg := DefaultConfig()
	cfg.StaleMs = 1000
	cfg.MinVenues = 2
	e := NewEngine(cfg)
	now := time.Now().UnixMilli()

	inputs := []VenueInput{
		{Venue: "binance", Mid: 100000, BestBid: 99995, BestAsk: 100005, TsMs: now},
		{Venue: "okx", Mid: 100010, BestBid: 100005, BestAsk: 100015, TsMs: now - 5000}, // stale
	}

	fv := e.Compute("BTC-PERP", inputs)
	// Only 1 fresh venue, below min of 2
	if fv != nil {
		t.Error("expected nil when insufficient fresh venues")
	}
}

func TestFairValueEWASmoothing(t *testing.T) {
	cfg := DefaultConfig()
	cfg.EWADecay = 0.5 // fast decay for testing
	e := NewEngine(cfg)
	now := time.Now().UnixMilli()

	inputs := []VenueInput{
		{Venue: "binance", Mid: 100000, BestBid: 99995, BestAsk: 100005, DepthUSD: 500000, LeadPct: 50, Reliability: 0.9, TsMs: now},
		{Venue: "okx", Mid: 100000, BestBid: 99995, BestAsk: 100005, DepthUSD: 500000, LeadPct: 50, Reliability: 0.9, TsMs: now},
	}

	// First compute
	fv1 := e.Compute("BTC-PERP", inputs)
	if fv1 == nil {
		t.Fatal("expected fair value")
	}

	// Change leadership dramatically
	inputs[0].LeadPct = 90
	inputs[1].LeadPct = 10

	// Second compute — EWA should smooth the weights, not jump instantly
	fv2 := e.Compute("BTC-PERP", inputs)
	if fv2 == nil {
		t.Fatal("expected fair value")
	}

	// Binance weight should increase but not jump to maximum
	var bw1, bw2 float64
	for _, vw := range fv1.VenueWeights {
		if vw.Venue == "binance" {
			bw1 = vw.Weight
		}
	}
	for _, vw := range fv2.VenueWeights {
		if vw.Venue == "binance" {
			bw2 = vw.Weight
		}
	}
	if bw2 <= bw1 {
		t.Errorf("binance weight should increase: %.4f → %.4f", bw1, bw2)
	}
}
