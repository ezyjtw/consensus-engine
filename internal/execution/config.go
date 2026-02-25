package execution

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// VenueProfile holds per-venue simulation parameters for realistic paper fills.
// Fee rates should reflect the actual fee tier on each exchange — check your
// account's 30-day volume tier and token discount (e.g. BNB on Binance,
// OKB on OKX) and update accordingly. Defaults assume base tier, no discounts.
type VenueProfile struct {
	LatencyMinMs   int64   `yaml:"latency_min_ms"`   // minimum latency in ms
	LatencyMaxMs   int64   `yaml:"latency_max_ms"`   // maximum latency in ms
	SlippageBps    float64 `yaml:"slippage_bps"`     // base slippage for small orders
	DepthSlopeBps  float64 `yaml:"depth_slope_bps"`  // additional bps per $10k notional
	FeeBpsTaker    float64 `yaml:"fee_bps_taker"`    // taker fee in bps (crossing the spread)
	FeeBpsMaker    float64 `yaml:"fee_bps_maker"`    // maker fee in bps (resting limit order)
	PartialFillPct float64 `yaml:"partial_fill_pct"` // probability of partial fill (0-1)
}

type Config struct {
	TradingMode        string                  `yaml:"trading_mode"`      // PAPER | LIVE
	SimSlippageBps     float64                 `yaml:"sim_slippage_bps"`
	SimLatencyMs       int64                   `yaml:"sim_latency_ms"`
	AdverseSelBps      float64                 `yaml:"adverse_selection_bps"`
	MaxOrdersPerMinute int                     `yaml:"max_orders_per_minute"`
	TenantID           string                  `yaml:"tenant_id"`
	Redis              RedisConf               `yaml:"redis"`
	VenueProfiles      map[string]VenueProfile `yaml:"venue_profiles"`

	// Live execution safety parameters.
	MaxRetriesPerLeg  int     `yaml:"max_retries_per_leg"`   // max order retries per leg (default 3)
	HedgeDriftMaxMs   int64   `yaml:"hedge_drift_max_ms"`    // max ms exposed before abort (default 5000)
	MinPartialFillPct float64 `yaml:"min_partial_fill_pct"`  // min fill % to proceed with leg B (default 0.10)
	ReconDelayMs      int64   `yaml:"recon_delay_ms"`        // delay before recon check (default 2000)

	// Micro-live graduation caps — hard limits during initial live period.
	LiveMaxOrderNotionalUSD float64 `yaml:"live_max_order_notional_usd"` // per-order cap (default 10000)
	LiveMaxDailyNotionalUSD float64 `yaml:"live_max_daily_notional_usd"` // rolling 24h cap (default 100000)
	LiveMaxOpenOrders       int     `yaml:"live_max_open_orders"`        // concurrent order cap (default 4)
}

type RedisConf struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	InputStream   string `yaml:"input_stream"`   // trade:intents:approved
	OutputEvents  string `yaml:"output_events"`  // execution:events
	OutputFills   string `yaml:"output_fills"`   // demo:fills or live:fills
	ConsumerGroup string `yaml:"consumer_group"`
	ConsumerName  string `yaml:"consumer_name"`
	BlockMs       int64  `yaml:"block_ms"`
	BatchSize     int64  `yaml:"batch_size"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		c.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		c.Redis.Password = v
	}
	if os.Getenv("REDIS_TLS") == "true" {
		c.Redis.UseTLS = true
	}
	if v := os.Getenv("TRADING_MODE"); v != "" {
		c.TradingMode = v
	}
	if c.TradingMode == "" {
		c.TradingMode = "PAPER"
	}
	if c.SimSlippageBps == 0 {
		c.SimSlippageBps = 4.0
	}
	if c.SimLatencyMs == 0 {
		c.SimLatencyMs = 50
	}
	if c.AdverseSelBps == 0 {
		c.AdverseSelBps = 10.0
	}
	if c.MaxOrdersPerMinute == 0 {
		c.MaxOrdersPerMinute = 120 // 2 orders/sec default
	}
	if c.TenantID == "" {
		c.TenantID = "default"
	}
	if c.MaxRetriesPerLeg == 0 {
		c.MaxRetriesPerLeg = 3
	}
	if c.HedgeDriftMaxMs == 0 {
		c.HedgeDriftMaxMs = 5000
	}
	if c.MinPartialFillPct == 0 {
		c.MinPartialFillPct = 0.10
	}
	if c.ReconDelayMs == 0 {
		c.ReconDelayMs = 2000
	}
	if c.LiveMaxOrderNotionalUSD == 0 {
		c.LiveMaxOrderNotionalUSD = 10000 // $10k per order — conservative micro-live cap
	}
	if c.LiveMaxDailyNotionalUSD == 0 {
		c.LiveMaxDailyNotionalUSD = 100000 // $100k rolling 24h — conservative micro-live cap
	}
	if c.LiveMaxOpenOrders == 0 {
		c.LiveMaxOpenOrders = 4
	}
	if c.VenueProfiles == nil {
		c.VenueProfiles = defaultVenueProfiles()
	}
	return &c, nil
}

// defaultVenueProfiles returns per-venue simulation parameters.
// Fee rates are base-tier (no volume discount, no token discount).
// Override via execution_router.yaml venue_profiles to match your actual tier.
//
// Reference fee schedules (perpetual futures, base tier, as of 2025-Q1):
//
//	Venue     Maker    Taker    Notes
//	Binance   2.0 bps  4.5 bps  USDT-M futures; -10% with BNB
//	OKX       2.0 bps  5.0 bps  USDT-M perps; -15% with OKB
//	Bybit     2.0 bps  5.5 bps  USDT perps
//	Deribit   0.0 bps  3.0 bps  BTC/ETH perps; maker rebate at higher tiers
//	Coinbase  4.0 bps  6.0 bps  Perps (Intl)
//	HTX       2.0 bps  5.0 bps  USDT-M; -10% with HT
//	Gate      2.0 bps  5.0 bps  USDT perps; -25% with GT
func defaultVenueProfiles() map[string]VenueProfile {
	return map[string]VenueProfile{
		"binance":  {LatencyMinMs: 8, LatencyMaxMs: 25, SlippageBps: 2.0, DepthSlopeBps: 0.5, FeeBpsTaker: 4.5, FeeBpsMaker: 2.0, PartialFillPct: 0.05},
		"okx":      {LatencyMinMs: 15, LatencyMaxMs: 40, SlippageBps: 3.0, DepthSlopeBps: 0.8, FeeBpsTaker: 5.0, FeeBpsMaker: 2.0, PartialFillPct: 0.08},
		"bybit":    {LatencyMinMs: 12, LatencyMaxMs: 35, SlippageBps: 3.0, DepthSlopeBps: 0.7, FeeBpsTaker: 5.5, FeeBpsMaker: 2.0, PartialFillPct: 0.07},
		"deribit":  {LatencyMinMs: 25, LatencyMaxMs: 60, SlippageBps: 4.0, DepthSlopeBps: 1.2, FeeBpsTaker: 3.0, FeeBpsMaker: 0.0, PartialFillPct: 0.10},
		"coinbase": {LatencyMinMs: 20, LatencyMaxMs: 50, SlippageBps: 5.0, DepthSlopeBps: 1.5, FeeBpsTaker: 6.0, FeeBpsMaker: 4.0, PartialFillPct: 0.12},
		"htx":      {LatencyMinMs: 18, LatencyMaxMs: 45, SlippageBps: 3.5, DepthSlopeBps: 1.0, FeeBpsTaker: 5.0, FeeBpsMaker: 2.0, PartialFillPct: 0.09},
		"gate":     {LatencyMinMs: 20, LatencyMaxMs: 50, SlippageBps: 3.5, DepthSlopeBps: 1.0, FeeBpsTaker: 5.0, FeeBpsMaker: 2.0, PartialFillPct: 0.09},
	}
}
