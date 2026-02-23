package execution

import "github.com/ezyjtw/consensus-engine/internal/arb"

// ExecutionEvent is emitted for every order lifecycle transition.
type ExecutionEvent struct {
	EventType            string  `json:"event_type"`             // ORDER_FILLED | ORDER_REJECTED | HEDGE_FAILED | LEG_PARTIAL
	IntentID             string  `json:"intent_id"`
	LegIndex             int     `json:"leg_index"`
	Venue                string  `json:"venue"`
	Symbol               string  `json:"symbol"`
	Action               string  `json:"action"`
	Strategy             string  `json:"strategy"`
	Market               string  `json:"market"`
	RequestedNotionalUSD float64 `json:"requested_notional_usd"`
	FilledNotionalUSD    float64 `json:"filled_notional_usd"`
	FilledPrice          float64 `json:"filled_price"`
	SlippageBpsActual    float64 `json:"slippage_bps_actual"`
	SlippageBpsAllowed   float64 `json:"slippage_bps_allowed"`
	FeesUSDActual        float64 `json:"fees_usd_actual"`
	TsMs                 int64   `json:"ts_ms"`
	LatencySignalToFillMs int64  `json:"latency_signal_to_fill_ms"`
	TenantID             string  `json:"tenant_id"`
	Mode                 string  `json:"mode"` // PAPER | LIVE
}

// SimulatedFill records the full paper trading fill for PnL tracking.
// FillLeg records per-leg fill details for notional accounting.
type FillLeg struct {
	Venue            string  `json:"venue"`
	Action           string  `json:"action"`
	FilledNotionalUSD float64 `json:"filled_notional_usd"`
	FilledPrice       float64 `json:"filled_price"`
}

type SimulatedFill struct {
	IntentID              string    `json:"intent_id"`
	Strategy              string    `json:"strategy"`
	Symbol                string    `json:"symbol"`
	Legs                  []FillLeg `json:"legs,omitempty"` // per-leg fill details
	TsSignalMs            int64   `json:"ts_signal_ms"`
	TsFillSimulatedMs     int64   `json:"ts_fill_simulated_ms"`
	LatencyMs             int64   `json:"latency_ms"`
	EdgeAtSignalBps       float64 `json:"edge_at_signal_bps"`
	EdgeAtFillBps         float64 `json:"edge_at_fill_bps"`
	EdgeCapturedBps       float64 `json:"edge_captured_bps"`
	AdverseSelectionOccurred bool `json:"adverse_selection_occurred"`
	FillPriceBuy          float64 `json:"fill_price_buy"`
	FillPriceSell         float64 `json:"fill_price_sell"`
	FeesAssumedUSD        float64 `json:"fees_assumed_usd"`
	SlippageAssumedBps    float64 `json:"slippage_assumed_bps"`
	NetPnLUSD             float64 `json:"net_pnl_usd"`
	IntentExpired         bool    `json:"intent_expired"`
	Mode                  string  `json:"mode"`
	TenantID              string  `json:"tenant_id"`
}

// PositionUpdate is written to Redis whenever a position changes.
type PositionUpdate struct {
	TenantID    string  `json:"tenant_id"`
	Venue       string  `json:"venue"`
	Symbol      string  `json:"symbol"`
	Market      string  `json:"market"` // PERP | SPOT
	NotionalUSD float64 `json:"notional_usd"` // positive = long, negative = short
	EntryPrice  float64 `json:"entry_price"`
	TsMs        int64   `json:"ts_ms"`
	Mode        string  `json:"mode"`
}

// ApprovedIntent is a type alias for clarity.
type ApprovedIntent = arb.TradeIntent
