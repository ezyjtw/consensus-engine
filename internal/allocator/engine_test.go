package allocator

import (
	"testing"

	"github.com/ezyjtw/consensus-engine/internal/arb"
)

func testPolicy() *Policy {
	return &Policy{
		InitialCapitalUSD: 100000,
		PerStrategyMaxUSD: map[string]float64{
			"CROSS_VENUE_ARB": 100000,
		},
		PerVenueMaxUSD: map[string]float64{
			"binance": 200000,
			"okx":     150000,
		},
		MinQualityForArb:     "MED",
		MinQualityForFunding: "MED",
	}
}

func makeIntent(strategy, symbol string, notional float64) arb.TradeIntent {
	return arb.TradeIntent{
		IntentID: "test-intent",
		Strategy: strategy,
		Symbol:   symbol,
		Legs: []arb.TradeLeg{
			{Venue: "binance", Action: "BUY", NotionalUSD: notional / 2},
			{Venue: "okx", Action: "SELL", NotionalUSD: notional / 2},
		},
		Expected:    arb.ExpectedMetrics{EdgeBpsNet: 10},
		Constraints: arb.IntentConstraints{MinQuality: "MED"},
	}
}

// Test that initial capital is set and equity snapshot works.
func TestEquitySnapshot_Initial(t *testing.T) {
	e := NewEngine(testPolicy())
	snap := e.Equity()

	if snap.InitialCapitalUSD != 100000 {
		t.Errorf("initial capital: got %.0f, want 100000", snap.InitialCapitalUSD)
	}
	if snap.CurrentEquityUSD != 100000 {
		t.Errorf("current equity: got %.0f, want 100000", snap.CurrentEquityUSD)
	}
	if snap.ReturnPct != 0 {
		t.Errorf("return pct: got %.2f, want 0", snap.ReturnPct)
	}
	if snap.DrawdownPct != 0 {
		t.Errorf("drawdown pct: got %.2f, want 0", snap.DrawdownPct)
	}
}

// Test that P&L updates equity correctly.
func TestRecordPnL(t *testing.T) {
	e := NewEngine(testPolicy())

	e.RecordPnL(500, 10) // profit of $500, fees $10
	snap := e.Equity()

	if snap.CumulativePnLUSD != 500 {
		t.Errorf("cumulative pnl: got %.0f, want 500", snap.CumulativePnLUSD)
	}
	if snap.CurrentEquityUSD != 100500 {
		t.Errorf("equity: got %.0f, want 100500", snap.CurrentEquityUSD)
	}
	if snap.PeakEquityUSD != 100500 {
		t.Errorf("peak equity: got %.0f, want 100500", snap.PeakEquityUSD)
	}
	if snap.TotalFeesUSD != 10 {
		t.Errorf("total fees: got %.0f, want 10", snap.TotalFeesUSD)
	}
	if snap.FillCount != 1 {
		t.Errorf("fill count: got %d, want 1", snap.FillCount)
	}

	// Record a loss.
	e.RecordPnL(-1000, 5)
	snap = e.Equity()

	if snap.CurrentEquityUSD != 99500 {
		t.Errorf("equity after loss: got %.0f, want 99500", snap.CurrentEquityUSD)
	}
	if snap.PeakEquityUSD != 100500 {
		t.Errorf("peak should remain at 100500, got %.0f", snap.PeakEquityUSD)
	}
	// Drawdown: (100500 - 99500) / 100500 ≈ 0.995%
	if snap.DrawdownPct < 0.99 || snap.DrawdownPct > 1.00 {
		t.Errorf("drawdown: got %.4f%%, want ~0.995%%", snap.DrawdownPct)
	}
}

// Test that the capital gate rejects intents when capital is exhausted.
func TestCapitalGate_Exhausted(t *testing.T) {
	p := testPolicy()
	p.InitialCapitalUSD = 5000
	p.KellyFraction = 0 // disable Kelly
	e := NewEngine(p)

	// First intent deploys most of the capital.
	intent1 := makeIntent("CROSS_VENUE_ARB", "BTC-PERP", 4000)
	o1 := e.Evaluate(intent1, "RUNNING", "MED")
	if !o1.Approved {
		t.Fatalf("first intent should be approved, got: %s", o1.Reason)
	}

	// Record a loss that wipes out remaining capital.
	e.RecordPnL(-2000, 0)
	// Equity is now 5000 - 2000 = 3000, deployed = 4000 → available = -1000

	// Second intent should be rejected — no capital left.
	intent2 := makeIntent("CROSS_VENUE_ARB", "ETH-PERP", 1000)
	o2 := e.Evaluate(intent2, "RUNNING", "MED")

	if o2.Approved {
		t.Error("expected intent to be rejected due to capital exhaustion")
	}
}

// Test that intents within capital are approved.
func TestCapitalGate_Approved(t *testing.T) {
	e := NewEngine(testPolicy())

	intent := makeIntent("CROSS_VENUE_ARB", "BTC-PERP", 10000) // well within $100k
	outcome := e.Evaluate(intent, "RUNNING", "MED")

	if !outcome.Approved {
		t.Errorf("expected intent to be approved, rejected with: %s", outcome.Reason)
	}
}

// Test that intent is scaled down when it partially exceeds capital.
func TestCapitalGate_ScaleDown(t *testing.T) {
	p := testPolicy()
	p.InitialCapitalUSD = 10000
	p.KellyFraction = 0 // disable Kelly so we can see the capital gate effect
	e := NewEngine(p)

	// Intent for $15000 but only $10000 equity → should be scaled down.
	intent := makeIntent("CROSS_VENUE_ARB", "BTC-PERP", 15000)
	outcome := e.Evaluate(intent, "RUNNING", "MED")

	if !outcome.Approved {
		t.Errorf("expected intent to be approved (scaled down), rejected with: %s", outcome.Reason)
		return
	}

	total := totalNotional(outcome.Intent)
	if total > 10000 {
		t.Errorf("notional should be capped at equity $10000, got $%.0f", total)
	}
}

// Test unlimited capital mode (legacy).
func TestUnlimitedCapital(t *testing.T) {
	p := testPolicy()
	p.InitialCapitalUSD = 0 // unlimited
	e := NewEngine(p)

	intent := makeIntent("CROSS_VENUE_ARB", "BTC-PERP", 50000)
	outcome := e.Evaluate(intent, "RUNNING", "MED")

	if !outcome.Approved {
		t.Errorf("unlimited mode: expected approval, got rejection: %s", outcome.Reason)
	}
}
