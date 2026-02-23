package risk

// Mode represents the system operating state.
type Mode string

const (
	ModeRunning  Mode = "RUNNING"
	ModePaused   Mode = "PAUSED"
	ModeSafe     Mode = "SAFE"
	ModeFlatten  Mode = "FLATTEN"
	ModeHalted   Mode = "HALTED"
)

// State is the full risk snapshot published to Redis and the events stream.
type State struct {
	TenantID          string  `json:"tenant_id"`
	Mode              Mode    `json:"mode"`
	TsMs              int64   `json:"ts_ms"`
	NetDeltaUSD       float64 `json:"net_delta_usd"`
	DrawdownPct       float64 `json:"drawdown_pct"`
	PeakEquityUSD     float64 `json:"peak_equity_usd"`
	CurrentEquityUSD  float64 `json:"current_equity_usd"`
	HedgeDriftUSDSec  float64 `json:"hedge_drift_usd_sec"`
	ErrorRate5mPct    float64 `json:"error_rate_5m_pct"`
	BlacklistedVenues []string `json:"blacklisted_venues"`
	Reason            string  `json:"reason,omitempty"`
}

// Alert is published to risk:alerts when a threshold is breached.
type Alert struct {
	TenantID string `json:"tenant_id"`
	TsMs     int64  `json:"ts_ms"`
	Source   string `json:"source"`
	Severity string `json:"severity"` // INFO | WARN | CRITICAL
	Message  string `json:"message"`
	Metric   string `json:"metric,omitempty"`
	Value    float64 `json:"value,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
}
