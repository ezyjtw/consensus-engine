package taxuk

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestConvertToGBP(t *testing.T) {
	trades := []TradeRecord{
		{PriceUSD: 100, NotionalUSD: 1000, FeesUSD: 10},
		{PriceUSD: 200, NotionalUSD: 2000, FeesUSD: 20},
	}

	ConvertToGBP(trades, 0.80)

	assertClose(t, "t0 price gbp", trades[0].PriceGBP, 80)
	assertClose(t, "t0 notional gbp", trades[0].NotionalGBP, 800)
	assertClose(t, "t0 fees gbp", trades[0].FeesGBP, 8)
	assertClose(t, "t1 price gbp", trades[1].PriceGBP, 160)
	assertClose(t, "t1 notional gbp", trades[1].NotionalGBP, 1600)
	assertClose(t, "t1 fees gbp", trades[1].FeesGBP, 16)
}

func TestGenerateReport_SpotAndDeriv(t *testing.T) {
	start := d(2025, 4, 6)
	end := d(2026, 4, 5)

	trades := []TradeRecord{
		// Spot trades — should go through matching.
		{
			ID: "sb1", Timestamp: d(2025, 5, 1), Symbol: "BTC", Action: "BUY",
			Market: "SPOT", Quantity: 10, NotionalGBP: 1000, FeesGBP: 0,
		},
		{
			ID: "ss1", Timestamp: d(2025, 9, 1), Symbol: "BTC", Action: "SELL",
			Market: "SPOT", Quantity: 10, NotionalGBP: 1500, FeesGBP: 0,
		},
		// Derivative trades — net P&L.
		{
			ID: "db1", Timestamp: d(2025, 6, 1), Symbol: "BTC-PERP", Action: "BUY",
			Market: "PERP", Quantity: 1, NotionalGBP: 800, FeesGBP: 5,
		},
		{
			ID: "ds1", Timestamp: d(2025, 6, 2), Symbol: "BTC-PERP", Action: "SELL",
			Market: "PERP", Quantity: 1, NotionalGBP: 900, FeesGBP: 5,
		},
	}

	rpt := GenerateReport("default", start, end, 0.80, trades, nil)

	// Spot: buy 10 @ 100, sell 10 @ 150 → gain = 1500 - 1000 = 500
	assertClose(t, "net spot gain", rpt.NetSpotGainGBP, 500)
	if len(rpt.SpotDisposals) != 1 {
		t.Fatalf("expected 1 spot disposal, got %d", len(rpt.SpotDisposals))
	}

	// Derivatives: sell 900 - buy 800 = 100, fees = 10
	assertClose(t, "deriv pnl", rpt.DerivativePnLGBP, 100)
	assertClose(t, "deriv fees", rpt.DerivativeFeesGBP, 10)
	assertClose(t, "net deriv income", rpt.NetDerivativeIncomeGBP, 90)

	// Combined
	assertClose(t, "total taxable", rpt.TotalTaxableGBP, 590)
}

func TestGenerateReport_PoolsCarryForward(t *testing.T) {
	pools := map[string]*Section104Pool{
		"ETH": {Symbol: "ETH", TotalQuantity: 100, TotalCostGBP: 10000},
	}

	trades := []TradeRecord{
		{
			ID: "s1", Timestamp: d(2025, 5, 1), Symbol: "ETH", Action: "SELL",
			Market: "SPOT", Quantity: 50, NotionalGBP: 7500, FeesGBP: 0,
		},
	}

	rpt := GenerateReport("default", d(2025, 4, 6), d(2026, 4, 5), 0.80, trades, pools)

	// Sell 50 from pool at avg cost 100/unit → cost = 5000, proceeds = 7500
	assertClose(t, "net spot gain", rpt.NetSpotGainGBP, 2500)
	assertClose(t, "pool remaining", rpt.Pools["ETH"].TotalQuantity, 50)
	assertClose(t, "pool cost remaining", rpt.Pools["ETH"].TotalCostGBP, 5000)
}

func TestWriteTransactionLogCSV(t *testing.T) {
	trades := []TradeRecord{
		{
			ID: "t1", IntentID: "i1", Timestamp: d(2025, 5, 1),
			Symbol: "BTC", Action: "BUY", Market: "SPOT", Venue: "binance",
			Strategy: "arb", Quantity: 1.5, PriceUSD: 30000, NotionalUSD: 45000,
			FeesUSD: 10, PriceGBP: 24000, NotionalGBP: 36000, FeesGBP: 8,
		},
	}

	var buf bytes.Buffer
	if err := WriteTransactionLogCSV(&buf, trades); err != nil {
		t.Fatal(err)
	}

	csv := buf.String()
	if !strings.Contains(csv, "date,symbol,action,market") {
		t.Error("missing header row")
	}
	if !strings.Contains(csv, "BTC") {
		t.Error("missing trade data")
	}
	if !strings.Contains(csv, "binance") {
		t.Error("missing venue data")
	}
}

func TestWriteCapitalGainsCSV(t *testing.T) {
	disposals := []Disposal{
		{
			Date: d(2025, 9, 1), Symbol: "BTC", Quantity: 10,
			ProceedsGBP: 1500, CostGBP: 1000, GainGBP: 500, MatchType: "section-104",
		},
	}

	var buf bytes.Buffer
	if err := WriteCapitalGainsCSV(&buf, disposals); err != nil {
		t.Fatal(err)
	}

	csv := buf.String()
	if !strings.Contains(csv, "proceeds_gbp") {
		t.Error("missing header")
	}
	if !strings.Contains(csv, "500.00") {
		t.Error("missing gain data")
	}
}

func TestWriteSummaryCSV(t *testing.T) {
	rpt := &TaxReport{
		TenantID:               "acme",
		PeriodStart:            d(2025, 4, 6),
		PeriodEnd:              d(2026, 4, 5),
		GBPUSDRate:             0.80,
		TotalSpotGainsGBP:      1000,
		TotalSpotLossesGBP:     200,
		NetSpotGainGBP:         800,
		SpotDisposals:          make([]Disposal, 5),
		NetDerivativeIncomeGBP: 500,
		TotalTaxableGBP:        1300,
		Pools: map[string]*Section104Pool{
			"BTC": {Symbol: "BTC", TotalQuantity: 10, TotalCostGBP: 5000},
		},
	}

	var buf bytes.Buffer
	if err := WriteSummaryCSV(&buf, rpt); err != nil {
		t.Fatal(err)
	}

	csv := buf.String()
	if !strings.Contains(csv, "UK Tax Report Summary") {
		t.Error("missing summary header")
	}
	if !strings.Contains(csv, "1300.00") {
		t.Error("missing total taxable amount")
	}
	if !strings.Contains(csv, "BTC") {
		t.Error("missing pool data")
	}
}

func TestGenerateReport_EmptyTrades(t *testing.T) {
	rpt := GenerateReport("default", time.Now(), time.Now(), 0.80, nil, nil)
	assertClose(t, "total taxable", rpt.TotalTaxableGBP, 0)
	if len(rpt.SpotDisposals) != 0 {
		t.Errorf("expected 0 disposals, got %d", len(rpt.SpotDisposals))
	}
}
