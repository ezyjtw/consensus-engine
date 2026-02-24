package taxuk

import (
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"time"
)

// ConvertToGBP populates the GBP fields on each trade using a fixed USD/GBP rate.
func ConvertToGBP(trades []TradeRecord, gbpPerUSD float64) {
	for i := range trades {
		trades[i].PriceGBP = trades[i].PriceUSD * gbpPerUSD
		trades[i].NotionalGBP = trades[i].NotionalUSD * gbpPerUSD
		trades[i].FeesGBP = trades[i].FeesUSD * gbpPerUSD
	}
}

// GenerateReport computes a full UK tax report from a set of GBP-converted trades.
// The pools parameter carries forward Section 104 pool state from prior periods;
// pass nil to start fresh.
func GenerateReport(tenantID string, start, end time.Time, gbpRate float64,
	trades []TradeRecord, pools map[string]*Section104Pool) *TaxReport {

	if pools == nil {
		pools = make(map[string]*Section104Pool)
	}

	rpt := &TaxReport{
		TenantID:    tenantID,
		PeriodStart: start,
		PeriodEnd:   end,
		GBPUSDRate:  gbpRate,
		Trades:      trades,
		Pools:       pools,
	}

	// Partition trades by market type and symbol.
	spotBySymbol := make(map[string][]TradeRecord)
	var derivBuys, derivSells []TradeRecord

	for _, t := range trades {
		switch t.Market {
		case "SPOT":
			spotBySymbol[t.Symbol] = append(spotBySymbol[t.Symbol], t)
		default: // PERP and any other derivative
			if t.Action == "BUY" {
				derivBuys = append(derivBuys, t)
			} else {
				derivSells = append(derivSells, t)
			}
		}
	}

	// ── Spot: apply HMRC matching per symbol ────────────────────────────
	for symbol, symbolTrades := range spotBySymbol {
		pool, ok := pools[symbol]
		if !ok {
			pool = &Section104Pool{Symbol: symbol}
			pools[symbol] = pool
		}
		disposals := MatchSpotTrades(symbolTrades, pool)
		rpt.SpotDisposals = append(rpt.SpotDisposals, disposals...)
	}

	// Sort disposals chronologically for the report.
	sort.Slice(rpt.SpotDisposals, func(i, j int) bool {
		return rpt.SpotDisposals[i].Date.Before(rpt.SpotDisposals[j].Date)
	})

	for _, d := range rpt.SpotDisposals {
		if d.GainGBP >= 0 {
			rpt.TotalSpotGainsGBP += d.GainGBP
		} else {
			rpt.TotalSpotLossesGBP += -d.GainGBP
		}
	}
	rpt.NetSpotGainGBP = rpt.TotalSpotGainsGBP - rpt.TotalSpotLossesGBP

	// ── Derivatives: net P&L ────────────────────────────────────────────
	for _, t := range derivSells {
		rpt.DerivativePnLGBP += t.NotionalGBP
		rpt.DerivativeFeesGBP += t.FeesGBP
	}
	for _, t := range derivBuys {
		rpt.DerivativePnLGBP -= t.NotionalGBP
		rpt.DerivativeFeesGBP += t.FeesGBP
	}
	rpt.NetDerivativeIncomeGBP = rpt.DerivativePnLGBP - rpt.DerivativeFeesGBP

	// ── Combined ────────────────────────────────────────────────────────
	rpt.TotalTaxableGBP = rpt.NetSpotGainGBP + rpt.NetDerivativeIncomeGBP

	return rpt
}

// WriteTransactionLogCSV writes all trades as a CSV suitable for HMRC review.
func WriteTransactionLogCSV(w io.Writer, trades []TradeRecord) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"date", "symbol", "action", "market", "venue", "strategy",
		"quantity", "price_usd", "notional_usd", "fees_usd",
		"price_gbp", "notional_gbp", "fees_gbp",
		"trade_id", "intent_id",
	}); err != nil {
		return err
	}

	for _, t := range trades {
		if err := cw.Write([]string{
			t.Timestamp.Format("2006-01-02 15:04:05"),
			t.Symbol,
			t.Action,
			t.Market,
			t.Venue,
			t.Strategy,
			fmt.Sprintf("%.8f", t.Quantity),
			fmt.Sprintf("%.2f", t.PriceUSD),
			fmt.Sprintf("%.2f", t.NotionalUSD),
			fmt.Sprintf("%.2f", t.FeesUSD),
			fmt.Sprintf("%.2f", t.PriceGBP),
			fmt.Sprintf("%.2f", t.NotionalGBP),
			fmt.Sprintf("%.2f", t.FeesGBP),
			t.ID,
			t.IntentID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// WriteCapitalGainsCSV writes the spot capital gains schedule as CSV.
func WriteCapitalGainsCSV(w io.Writer, disposals []Disposal) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	if err := cw.Write([]string{
		"date", "symbol", "quantity",
		"proceeds_gbp", "cost_gbp", "gain_loss_gbp",
		"match_type",
	}); err != nil {
		return err
	}

	for _, d := range disposals {
		if err := cw.Write([]string{
			d.Date.Format("2006-01-02"),
			d.Symbol,
			fmt.Sprintf("%.8f", d.Quantity),
			fmt.Sprintf("%.2f", d.ProceedsGBP),
			fmt.Sprintf("%.2f", d.CostGBP),
			fmt.Sprintf("%.2f", d.GainGBP),
			d.MatchType,
		}); err != nil {
			return err
		}
	}
	return nil
}

// WriteSummaryCSV writes the CT600-ready summary as CSV.
func WriteSummaryCSV(w io.Writer, rpt *TaxReport) error {
	cw := csv.NewWriter(w)
	defer cw.Flush()

	rows := [][]string{
		{"UK Tax Report Summary"},
		{"Tenant", rpt.TenantID},
		{"Period", rpt.PeriodStart.Format("2006-01-02") + " to " + rpt.PeriodEnd.Format("2006-01-02")},
		{"GBP/USD Rate", fmt.Sprintf("%.6f", rpt.GBPUSDRate)},
		{""},
		{"Spot Capital Gains (Chargeable Gains)"},
		{"Total Gains (GBP)", fmt.Sprintf("%.2f", rpt.TotalSpotGainsGBP)},
		{"Total Losses (GBP)", fmt.Sprintf("%.2f", rpt.TotalSpotLossesGBP)},
		{"Net Chargeable Gain (GBP)", fmt.Sprintf("%.2f", rpt.NetSpotGainGBP)},
		{"Number of Disposals", fmt.Sprintf("%d", len(rpt.SpotDisposals))},
		{""},
		{"Derivative Trading Income"},
		{"Gross P&L (GBP)", fmt.Sprintf("%.2f", rpt.DerivativePnLGBP)},
		{"Total Fees (GBP)", fmt.Sprintf("%.2f", rpt.DerivativeFeesGBP)},
		{"Net Trading Income (GBP)", fmt.Sprintf("%.2f", rpt.NetDerivativeIncomeGBP)},
		{""},
		{"Combined"},
		{"Total Taxable Amount (GBP)", fmt.Sprintf("%.2f", rpt.TotalTaxableGBP)},
		{""},
		{"Section 104 Pool Balances at Period End"},
	}

	// Pool balances.
	symbols := make([]string, 0, len(rpt.Pools))
	for s := range rpt.Pools {
		symbols = append(symbols, s)
	}
	sort.Strings(symbols)
	for _, s := range symbols {
		p := rpt.Pools[s]
		if p.TotalQuantity > 0 {
			rows = append(rows, []string{
				s,
				fmt.Sprintf("%.8f units", p.TotalQuantity),
				fmt.Sprintf("%.2f GBP cost basis", p.TotalCostGBP),
			})
		}
	}

	for _, row := range rows {
		if err := cw.Write(row); err != nil {
			return err
		}
	}
	return nil
}
