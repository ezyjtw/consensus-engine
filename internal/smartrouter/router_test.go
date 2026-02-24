package smartrouter

import (
	"testing"
	"time"
)

func TestRouterSelectsBestVenue(t *testing.T) {
	cfg := DefaultRouterConfig()
	r := NewRouter(cfg)

	depths := map[string]VenueDepth{
		"binance": {Venue: "binance", BidDepthUSD: 500000, AskDepthUSD: 500000, BestBid: 99995, BestAsk: 100005, SpreadBps: 1.0, LatencyUs: 50000},
		"okx":     {Venue: "okx", BidDepthUSD: 100000, AskDepthUSD: 100000, BestBid: 99990, BestAsk: 100010, SpreadBps: 2.0, LatencyUs: 150000},
	}

	legs := []LegRequest{
		{Action: "BUY", SizeUSD: 10000, CandidateVenues: []string{"binance", "okx"}, MaxSlippageBps: 10},
	}

	decision := r.Route("BTC-PERP", legs, depths)
	if decision == nil {
		t.Fatal("expected routing decision")
	}
	if len(decision.Legs) != 1 {
		t.Fatalf("expected 1 leg, got %d", len(decision.Legs))
	}
	// Binance should win (deeper book, lower latency, tighter spread)
	if decision.Legs[0].Venue != "binance" {
		t.Errorf("expected binance, got %s", decision.Legs[0].Venue)
	}
}

func TestRouterHedgeFirst(t *testing.T) {
	cfg := DefaultRouterConfig()
	r := NewRouter(cfg)

	depths := map[string]VenueDepth{
		"binance": {Venue: "binance", BidDepthUSD: 500000, AskDepthUSD: 500000, BestBid: 99995, BestAsk: 100005, SpreadBps: 1.0},
		"okx":     {Venue: "okx", BidDepthUSD: 50000, AskDepthUSD: 50000, BestBid: 99990, BestAsk: 100010, SpreadBps: 2.0},
	}

	legs := []LegRequest{
		{Action: "BUY", SizeUSD: 10000, CandidateVenues: []string{"binance"}, MaxSlippageBps: 10},
		{Action: "SELL", SizeUSD: 10000, CandidateVenues: []string{"okx"}, MaxSlippageBps: 10},
	}

	decision := r.Route("BTC-PERP", legs, depths)
	if decision == nil {
		t.Fatal("expected routing decision")
	}

	// Strategy should be HEDGE_FIRST (OKX is much thinner)
	if decision.Strategy != StrategyHedgeFirst {
		t.Errorf("expected HEDGE_FIRST, got %s", decision.Strategy)
	}
}

func TestRecordFillUpdatesStats(t *testing.T) {
	cfg := DefaultRouterConfig()
	r := NewRouter(cfg)

	now := time.Now().UnixMilli()
	for i := 0; i < 10; i++ {
		r.RecordFill(FillRecord{
			Venue:       "binance",
			TsMs:        now - int64(i*1000),
			SlippageBps: 2.0,
			LatencyMs:   30,
			Filled:      true,
		})
	}

	stats := r.venueStats("binance")
	if stats.FillCount != 10 {
		t.Errorf("expected 10 fills, got %d", stats.FillCount)
	}
	if stats.AvgSlippageBps != 2.0 {
		t.Errorf("expected avg slippage 2.0, got %.2f", stats.AvgSlippageBps)
	}
}

func TestPassiveEngine(t *testing.T) {
	cfg := DefaultPassiveConfig()
	pe := NewPassiveEngine(cfg)

	depths := map[string]VenueDepth{
		"binance": {Venue: "binance", BidDepthUSD: 200000, AskDepthUSD: 200000, BestBid: 99990, BestAsk: 100010, SpreadBps: 2.0},
		"okx":     {Venue: "okx", BidDepthUSD: 200000, AskDepthUSD: 200000, BestBid: 99985, BestAsk: 100015, SpreadBps: 3.0},
	}

	// Only OKX should qualify (spread >= 3.0)
	opps := pe.FindOpportunities("BTC-PERP", depths, 0)
	found := false
	for _, opp := range opps {
		if opp.Venue == "okx" {
			found = true
			if opp.EdgeBps <= 0 {
				t.Error("expected positive edge for passive order")
			}
		}
	}
	if !found {
		t.Error("expected passive opportunity on OKX with wide spread")
	}
}
