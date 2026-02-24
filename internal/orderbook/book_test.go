package orderbook

import (
	"testing"
	"time"
)

func TestAggregatorSyntheticBook(t *testing.T) {
	agg := NewAggregator()
	now := time.Now().UnixMilli()

	agg.Update(VenueBook{
		Venue:  "binance",
		Symbol: "BTC-PERP",
		TsMs:   now,
		Bids: []Level{
			{Price: 100000, Qty: 1.0},
			{Price: 99990, Qty: 2.0},
		},
		Asks: []Level{
			{Price: 100010, Qty: 1.0},
			{Price: 100020, Qty: 2.0},
		},
	})
	agg.Update(VenueBook{
		Venue:  "okx",
		Symbol: "BTC-PERP",
		TsMs:   now,
		Bids: []Level{
			{Price: 99995, Qty: 1.5},
			{Price: 99985, Qty: 3.0},
		},
		Asks: []Level{
			{Price: 100015, Qty: 1.5},
			{Price: 100025, Qty: 3.0},
		},
	})

	sb := agg.SyntheticFor("BTC-PERP")
	if sb == nil {
		t.Fatal("expected synthetic book")
	}
	if len(sb.Bids) != 4 {
		t.Errorf("expected 4 merged bid levels, got %d", len(sb.Bids))
	}
	if len(sb.Asks) != 4 {
		t.Errorf("expected 4 merged ask levels, got %d", len(sb.Asks))
	}
	// Best bid should be 100000 (binance)
	if sb.Bids[0].Price != 100000 {
		t.Errorf("expected best bid 100000, got %.2f", sb.Bids[0].Price)
	}
	// Best ask should be 100010 (binance)
	if sb.Asks[0].Price != 100010 {
		t.Errorf("expected best ask 100010, got %.2f", sb.Asks[0].Price)
	}
	if len(sb.VenueDepth) != 2 {
		t.Errorf("expected 2 venue depths, got %d", len(sb.VenueDepth))
	}
}

func TestEstimateSlippage(t *testing.T) {
	agg := NewAggregator()
	now := time.Now().UnixMilli()

	agg.Update(VenueBook{
		Venue:  "binance",
		Symbol: "BTC-PERP",
		TsMs:   now,
		Bids: []Level{
			{Price: 100000, Qty: 1.0},  // $100k
			{Price: 99900, Qty: 2.0},   // $199.8k
		},
		Asks: []Level{
			{Price: 100100, Qty: 0.5},  // $50k
			{Price: 100200, Qty: 1.0},  // $100.2k
		},
	})

	// Small buy order should have minimal slippage
	slip, fill := agg.EstimateSlippage("BTC-PERP", "binance", 10000, true)
	if fill < 90 {
		t.Errorf("expected high fill for small order, got %.1f%%", fill)
	}
	if slip > 5 {
		t.Errorf("expected low slippage for small order, got %.2f bps", slip)
	}

	// Large buy order should have higher slippage
	slip2, _ := agg.EstimateSlippage("BTC-PERP", "binance", 100000, true)
	if slip2 <= slip {
		t.Errorf("expected higher slippage for larger order: %.2f vs %.2f", slip2, slip)
	}
}

func TestBestExecutionVenue(t *testing.T) {
	agg := NewAggregator()
	now := time.Now().UnixMilli()

	// Binance: deep book
	agg.Update(VenueBook{
		Venue:  "binance",
		Symbol: "BTC-PERP",
		TsMs:   now,
		Asks: []Level{
			{Price: 100010, Qty: 5.0}, // $500k
			{Price: 100020, Qty: 5.0},
		},
		Bids: []Level{{Price: 100000, Qty: 5.0}},
	})

	// OKX: thin book
	agg.Update(VenueBook{
		Venue:  "okx",
		Symbol: "BTC-PERP",
		TsMs:   now,
		Asks: []Level{
			{Price: 100010, Qty: 0.1}, // $10k
			{Price: 100200, Qty: 0.5},
		},
		Bids: []Level{{Price: 100000, Qty: 0.1}},
	})

	venue, _ := agg.BestExecutionVenue("BTC-PERP", 50000, true)
	if venue != "binance" {
		t.Errorf("expected binance as best venue, got %s", venue)
	}
}

func TestFlowDetector(t *testing.T) {
	fd := NewFlowDetector(10000) // 10 second window

	now := time.Now().UnixMilli()

	// Record aggressive buying
	for i := 0; i < 20; i++ {
		fd.Record(FlowSample{
			TsMs:              now - int64(i*100),
			Venue:             "binance",
			Symbol:            "BTC-PERP",
			AggressiveBuyQty:  3.0,
			AggressiveSellQty: 1.0,
			BidDepthDelta:     -100,
			AskDepthDelta:     -300,
		})
	}

	p := fd.Pressure("BTC-PERP")
	if p.Score <= 0 {
		t.Errorf("expected positive pressure (buy side), got %.4f", p.Score)
	}
	if p.AggressiveBuyPct < 50 {
		t.Errorf("expected >50%% aggressive buy, got %.1f%%", p.AggressiveBuyPct)
	}
	if p.Confidence <= 0 {
		t.Error("expected positive confidence")
	}
}

func TestLeaderDetector(t *testing.T) {
	ld := NewLeaderDetector(2.0, 500, 60000)

	now := time.Now().UnixMilli()

	// Initialise prices
	ld.RecordPrice("binance", "BTC-PERP", 100000, now-2000)
	ld.RecordPrice("okx", "BTC-PERP", 100000, now-2000)
	ld.RecordPrice("bybit", "BTC-PERP", 100000, now-2000)

	// Binance moves first (5 bps up)
	ld.RecordPrice("binance", "BTC-PERP", 100050, now-1000)
	// OKX follows 200ms later
	ld.RecordPrice("okx", "BTC-PERP", 100050, now-800)
	// Bybit follows 300ms later
	ld.RecordPrice("bybit", "BTC-PERP", 100050, now-700)

	// Another round: binance leads again
	ld.RecordPrice("binance", "BTC-PERP", 100100, now-500)
	ld.RecordPrice("okx", "BTC-PERP", 100100, now-300)

	stats := ld.Stats("BTC-PERP")
	if len(stats) == 0 {
		t.Fatal("expected leader stats")
	}

	// Binance should be top leader
	if stats[0].Venue != "binance" {
		t.Errorf("expected binance as top leader, got %s", stats[0].Venue)
	}
	if stats[0].LeadPct <= 0 {
		t.Error("expected positive lead percentage for binance")
	}
}
