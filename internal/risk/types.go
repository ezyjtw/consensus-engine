package risk

// Mode represents the system operating state.
type Mode string

const (
	ModeRunning Mode = "RUNNING"
	ModePaused  Mode = "PAUSED"
	ModeSafe    Mode = "SAFE"
	ModeFlatten Mode = "FLATTEN"
	ModeHalted  Mode = "HALTED"
)

// State is the full risk snapshot published to Redis and the events stream.
type State struct {
	SchemaVersion    int      `json:"schema_version"`
	TenantID         string   `json:"tenant_id"`
	Mode             Mode     `json:"mode"`
	TsMs             int64    `json:"ts_ms"`
	NetDeltaUSD      float64  `json:"net_delta_usd"`
	DrawdownPct      float64  `json:"drawdown_pct"`
	PeakEquityUSD    float64  `json:"peak_equity_usd"`
	CurrentEquityUSD float64  `json:"current_equity_usd"`
	HedgeDriftUSDSec float64  `json:"hedge_drift_usd_sec"`
	ErrorRate5mPct   float64  `json:"error_rate_5m_pct"`
	BlacklistedVenues []string `json:"blacklisted_venues"`
	Reason           string   `json:"reason,omitempty"`

	// ── Exchange incident safety metrics ──────────────────────────────────

	// ADLRiskPct is the estimated probability (0–100) that open positions will
	// be auto-deleveraged at any venue in the current funding interval.
	// Elevated when a venue's insurance fund signal falls low or OI spikes.
	ADLRiskPct float64 `json:"adl_risk_pct"`

	// LiqClusterRisk counts estimated liquidation clusters within
	// LiqClusterWindowBps of the current consensus mid price.
	// High values indicate a liquidation waterfall could cascade on a small move.
	LiqClusterRisk int `json:"liq_cluster_risk"`

	// VenueDelevEventCount is the number of venue-wide deleveraging events
	// (mass-ADL signals) recorded in the recent rolling window.
	VenueDelevEventCount int `json:"venue_delev_event_count"`
}

// Alert is published to risk:alerts when a threshold is breached.
type Alert struct {
	SchemaVersion int     `json:"schema_version"`
	TenantID      string  `json:"tenant_id"`
	TsMs      int64   `json:"ts_ms"`
	Source    string  `json:"source"`
	Severity  string  `json:"severity"` // INFO | WARN | CRITICAL
	Message   string  `json:"message"`
	Metric    string  `json:"metric,omitempty"`
	Value     float64 `json:"value,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}
