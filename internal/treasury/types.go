// Package treasury provides deposit detection, fiat-to-crypto conversion,
// and multi-venue fund distribution.
package treasury

import "time"

// DepositEvent represents a detected incoming deposit.
type DepositEvent struct {
	TenantID  string    `json:"tenant_id"`
	DepositID string    `json:"deposit_id"`
	Source    string    `json:"source"`       // venue where deposit landed (e.g. "coinbase")
	Asset     string    `json:"asset"`        // deposited asset (e.g. "GBP", "USD", "USDC")
	Amount    float64   `json:"amount"`
	TxID      string    `json:"tx_id,omitempty"`
	Status    string    `json:"status"`       // DETECTED | CONVERTED | DISTRIBUTED
	TsMs      int64     `json:"ts_ms"`
}

// ConversionEvent records a fiat/crypto→USDC conversion.
type ConversionEvent struct {
	TenantID   string  `json:"tenant_id"`
	ConvertID  string  `json:"convert_id"`
	FromAsset  string  `json:"from_asset"`
	ToAsset    string  `json:"to_asset"`
	FromAmount float64 `json:"from_amount"`
	ToAmount   float64 `json:"to_amount"`
	FeesUSD    float64 `json:"fees_usd"`
	TsMs       int64   `json:"ts_ms"`
}

// DistributionLeg records one venue allocation in a distribution.
type DistributionLeg struct {
	Venue      string  `json:"venue"`
	Asset      string  `json:"asset"`
	Amount     float64 `json:"amount"`
	Network    string  `json:"network"`
	WithdrawID string  `json:"withdraw_id,omitempty"`
	Status     string  `json:"status"` // PENDING | SENT | CONFIRMED | FAILED
}

// DistributionEvent records a multi-venue fund distribution.
type DistributionEvent struct {
	TenantID    string            `json:"tenant_id"`
	DepositID   string            `json:"deposit_id"`
	TotalAmount float64           `json:"total_amount"`
	Legs        []DistributionLeg `json:"legs"`
	TsMs        int64             `json:"ts_ms"`
}

// SweepEvent records a profit sweep back to the treasury venue.
type SweepEvent struct {
	TenantID    string  `json:"tenant_id"`
	FromVenue   string  `json:"from_venue"`
	ToVenue     string  `json:"to_venue"`
	Asset       string  `json:"asset"`
	Amount      float64 `json:"amount"`
	WithdrawID  string  `json:"withdraw_id"`
	Status      string  `json:"status"`
	TsMs        int64   `json:"ts_ms"`
}

// ReconciliationReport is the output of a balance reconciliation check.
type ReconciliationReport struct {
	TenantID   string              `json:"tenant_id"`
	TsMs       int64               `json:"ts_ms"`
	TotalUSD   float64             `json:"total_usd"`
	Venues     []VenueReconcile    `json:"venues"`
	Healthy    bool                `json:"healthy"`
	Alerts     []string            `json:"alerts,omitempty"`
}

// VenueReconcile holds one venue's reconciliation data.
type VenueReconcile struct {
	Venue        string  `json:"venue"`
	BalanceUSD   float64 `json:"balance_usd"`
	PositionUSD  float64 `json:"position_usd"`
	ExpectedUSD  float64 `json:"expected_usd"` // from internal ledger
	DriftUSD     float64 `json:"drift_usd"`    // actual - expected
	DriftPct     float64 `json:"drift_pct"`
}

// AllocationWeight defines what fraction of new deposits a venue should receive.
type AllocationWeight struct {
	Venue   string  `yaml:"venue"   json:"venue"`
	Weight  float64 `yaml:"weight"  json:"weight"`  // 0.0–1.0
	Network string  `yaml:"network" json:"network"` // preferred withdrawal network
}

// Config holds the treasury service configuration.
type Config struct {
	TenantID            string             `yaml:"tenant_id"`
	TreasuryVenue       string             `yaml:"treasury_venue"`        // e.g. "coinbase"
	PollIntervalSec     int                `yaml:"poll_interval_sec"`
	AutoConvert         bool               `yaml:"auto_convert"`
	ConvertTo           string             `yaml:"convert_to"`            // e.g. "USDC"
	MinDistributeUSD    float64            `yaml:"min_distribute_usd"`    // minimum to trigger distribution
	Allocation          []AllocationWeight `yaml:"allocation"`
	SweepEnabled        bool               `yaml:"sweep_enabled"`
	SweepIntervalMin    int                `yaml:"sweep_interval_min"`
	SweepThresholdUSD   float64            `yaml:"sweep_threshold_usd"`   // min profit to sweep
	ReconcileIntervalMin int               `yaml:"reconcile_interval_min"`
	DriftAlertPct       float64            `yaml:"drift_alert_pct"`       // trigger alert above this drift
	TransferPolicyURL   string             `yaml:"transfer_policy_url"`   // e.g. "http://transfer-policy:8085"
	Redis               RedisConf          `yaml:"redis"`
}

// RedisConf holds Redis connection settings.
type RedisConf struct {
	Addr     string `yaml:"addr"`
	Password string `yaml:"password"`
	UseTLS   bool   `yaml:"use_tls"`
}

// LastSeen tracks the last deposit ID seen per asset to avoid reprocessing.
type LastSeen struct {
	DepositIDs map[string]time.Time // depositID → first seen time
}

// NewLastSeen creates an empty tracker.
func NewLastSeen() *LastSeen {
	return &LastSeen{DepositIDs: make(map[string]time.Time)}
}

// Seen returns true if this deposit has been processed.
func (ls *LastSeen) Seen(id string) bool {
	_, ok := ls.DepositIDs[id]
	return ok
}

// Mark records a deposit as processed.
func (ls *LastSeen) Mark(id string) {
	ls.DepositIDs[id] = time.Now()
}

// Prune removes entries older than the given duration.
func (ls *LastSeen) Prune(maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	for id, ts := range ls.DepositIDs {
		if ts.Before(cutoff) {
			delete(ls.DepositIDs, id)
		}
	}
}
