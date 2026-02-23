package liquidity

// Signal types the liquidity inefficiency engine detects.
type SignalType string

const (
	SignalSpreadBlowout      SignalType = "SPREAD_BLOWOUT"       // spread > N× rolling baseline
	SignalThinBook           SignalType = "THIN_BOOK"            // depth < threshold on one side
	SignalMarkIndexDivergence SignalType = "MARK_INDEX_DIVERGE"  // mark/index spread > threshold
	SignalOrderImbalance     SignalType = "ORDER_IMBALANCE"      // bid_depth / ask_depth extreme
	SignalLiquidationCascade SignalType = "LIQUIDATION_CASCADE"  // fast move + spread widen + depth collapse
)

// Signal is emitted when a liquidity anomaly is detected.
type Signal struct {
	Type     SignalType `json:"type"`
	Venue    string     `json:"venue"`
	Symbol   string     `json:"symbol"`
	Value    float64    `json:"value"`     // the raw metric that triggered
	Baseline float64    `json:"baseline"`  // the rolling baseline for context
	TsMs     int64      `json:"ts_ms"`
}
