package risk

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	MaxNetDeltaUSD             float64     `yaml:"max_net_delta_usd"`
	MaxMarginUtilisationPct    float64     `yaml:"max_margin_utilisation_pct"`
	SafeModeMarginUtilPct      float64     `yaml:"safe_mode_margin_utilisation_pct"`
	MinLiqDistancePct          float64     `yaml:"min_liquidation_distance_pct"`
	MaxHedgeDriftUSDSec        float64     `yaml:"max_hedge_drift_usd_seconds"`
	MaxDrawdownPct             float64     `yaml:"max_drawdown_pct"`
	SafeModeDrawdownPct        float64     `yaml:"safe_mode_drawdown_pct"`
	MaxErrorRate5mPct          float64     `yaml:"max_error_rate_5m_pct"`
	MaxReconciliationDivUSD    float64     `yaml:"max_reconciliation_divergence_usd"`
	MinCoreVenuesForSafeMode   int         `yaml:"min_core_venues_for_safe_mode"`
	PositionTruthPollIntervalS int         `yaml:"position_truth_poll_interval_s"`
	TenantID                   string      `yaml:"tenant_id"`
	Redis                      RedisPolicy `yaml:"redis"`

	// ── Exchange incident safety thresholds ───────────────────────────────

	// ADLRiskPausePct: pause trading when estimated ADL risk exceeds this %.
	// Elevated when insurance fund signals are thin or OI has spiked.
	// Default 40.
	ADLRiskPausePct float64 `yaml:"adl_risk_pause_pct"`

	// LiqClusterPauseCount: pause when the number of liquidation clusters
	// within LiqClusterWindowBps of current mid exceeds this count.
	// Default 3.
	LiqClusterPauseCount int `yaml:"liq_cluster_pause_count"`

	// VenueDelevSafeModeCount: enter SAFE MODE when this many venue-wide
	// deleveraging events are recorded in VenueDelevWindowMs.
	// Default 2.
	VenueDelevSafeModeCount int `yaml:"venue_delev_safe_mode_count"`

	// VenueDelevWindowMs: rolling window for deleveraging event counting (ms).
	// Default 300000 (5 minutes).
	VenueDelevWindowMs int64 `yaml:"venue_delev_window_ms"`
}

type RedisPolicy struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	EventsStream  string `yaml:"events_stream"`
	StatusStream  string `yaml:"status_stream"`
	AlertsStream  string `yaml:"alerts_stream"`
	StateStream   string `yaml:"state_stream"`
	ConsumerGroup string `yaml:"consumer_group"`
	ConsumerName  string `yaml:"consumer_name"`
	BlockMs       int64  `yaml:"block_ms"`
	BatchSize     int64  `yaml:"batch_size"`
}

func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		p.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		p.Redis.Password = v
	}
	if os.Getenv("REDIS_TLS") == "true" {
		p.Redis.UseTLS = true
	}
	if p.TenantID == "" {
		p.TenantID = "default"
	}
	if p.PositionTruthPollIntervalS == 0 {
		p.PositionTruthPollIntervalS = 30
	}
	// Safe defaults for exchange incident thresholds.
	if p.ADLRiskPausePct == 0 {
		p.ADLRiskPausePct = 40
	}
	if p.LiqClusterPauseCount == 0 {
		p.LiqClusterPauseCount = 3
	}
	if p.VenueDelevSafeModeCount == 0 {
		p.VenueDelevSafeModeCount = 2
	}
	if p.VenueDelevWindowMs == 0 {
		p.VenueDelevWindowMs = 5 * 60 * 1000
	}
	return &p, nil
}
