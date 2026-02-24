// Package taxuk computes UK tax reports for HMRC corporation tax filing.
//
// It implements HMRC's share matching rules for spot crypto disposals:
//   - Same-day rule: match disposals with same-day acquisitions first
//   - 30-day bed-and-breakfasting rule: match with acquisitions in the following 30 days
//   - Section 104 pool: weighted-average cost basis for remaining holdings
//
// Derivative (PERP) trades are reported as trading income without pooling.
package taxuk

import "time"

// TradeRecord represents a single filled trade leg for tax computation.
type TradeRecord struct {
	ID          string
	IntentID    string
	Timestamp   time.Time
	Symbol      string
	Action      string // "BUY" or "SELL"
	Venue       string
	Market      string  // "SPOT" or "PERP"
	Strategy    string
	Quantity    float64 // base-asset units (notional_usd / price)
	PriceUSD    float64 // per-unit price in USD
	NotionalUSD float64
	FeesUSD     float64
	// GBP-converted values (populated by ConvertToGBP).
	PriceGBP    float64
	NotionalGBP float64
	FeesGBP     float64
}

// Disposal is a single capital-gains disposal event produced by the matching engine.
type Disposal struct {
	Date        time.Time
	Symbol      string
	Quantity    float64
	ProceedsGBP float64
	CostGBP     float64
	GainGBP     float64
	MatchType   string   // "same-day", "30-day", "section-104", or "unmatched"
	MatchedWith []string // IDs of matched acquisition trades
}

// Section104Pool tracks the weighted-average cost basis for a single asset.
type Section104Pool struct {
	Symbol        string
	TotalQuantity float64
	TotalCostGBP  float64
}

// CostPerUnit returns the weighted-average cost per unit, or zero if the pool is empty.
func (p *Section104Pool) CostPerUnit() float64 {
	if p.TotalQuantity <= 0 {
		return 0
	}
	return p.TotalCostGBP / p.TotalQuantity
}

// TaxReport is the complete UK tax report for a given accounting period.
type TaxReport struct {
	TenantID    string
	PeriodStart time.Time
	PeriodEnd   time.Time
	GBPUSDRate  float64 // USD → GBP conversion rate used

	// All trades in the period, converted to GBP.
	Trades []TradeRecord

	// Spot capital gains.
	SpotDisposals      []Disposal
	TotalSpotGainsGBP  float64 // sum of positive gains
	TotalSpotLossesGBP float64 // sum of negative gains (as positive number)
	NetSpotGainGBP     float64

	// Derivative (PERP) trading income.
	DerivativePnLGBP       float64
	DerivativeFeesGBP      float64
	NetDerivativeIncomeGBP float64

	// Section 104 pool states at end of period.
	Pools map[string]*Section104Pool

	// Combined taxable totals.
	TotalTaxableGBP float64
}
