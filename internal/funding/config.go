package funding

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// FundingSymbolOverride allows per-symbol tuning of funding strategy parameters.
// Mid-cap tokens warrant smaller position sizes and wider slippage tolerance.
type FundingSymbolOverride struct {
	MaxNotionalUSD *float64 `yaml:"max_notional_usd,omitempty"`
	MaxSlippageBps *float64 `yaml:"max_slippage_bps,omitempty"`
}

type Policy struct {
	Symbols                []string           `yaml:"symbols"`
	MinAnnualYieldPct      map[string]float64 `yaml:"min_annual_yield_pct"`
	MaxNotionalUSD         float64            `yaml:"max_notional_usd"`
	MinDifferentialBps8h   float64            `yaml:"min_differential_bps_per_8h"`
	HedgeDriftUnwindUSD    float64            `yaml:"hedge_drift_unwind_usd"`
	VolatilityGate         VolatilityGate     `yaml:"volatility_gate"`
	Venues                 []string           `yaml:"venues"`
	EvalIntervalS          int                `yaml:"eval_interval_s"`
	IntentTTLMs            int64              `yaml:"intent_ttl_ms"`
	MaxSlippageBps         float64            `yaml:"max_slippage_bps"`
	CooldownS              int64              `yaml:"cooldown_s"`

	// SymbolOverrides provides per-symbol funding parameter tuning.
	// Keyed by canonical symbol (e.g. "SOL-PERP").
	SymbolOverrides map[string]FundingSymbolOverride `yaml:"symbol_overrides"`

	Redis RedisPolicy `yaml:"redis"`
}

// maxNotional returns the max notional for a symbol, falling back to global.
func (p *Policy) maxNotional(symbol string) float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.MaxNotionalUSD != nil {
		return *ovr.MaxNotionalUSD
	}
	return p.MaxNotionalUSD
}

// maxSlippage returns the max slippage for a symbol, falling back to global.
func (p *Policy) maxSlippage(symbol string) float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.MaxSlippageBps != nil {
		return *ovr.MaxSlippageBps
	}
	return p.MaxSlippageBps
}

type VolatilityGate struct {
	VolThresholdPct     float64 `yaml:"vol_threshold_pct"`
	SizeReductionFactor float64 `yaml:"size_reduction_factor"`
}

type RedisPolicy struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	InputStream   string `yaml:"input_stream"`   // market:quotes
	EventsStream  string `yaml:"events_stream"`  // execution:events (position tracking)
	OutputIntents string `yaml:"output_intents"` // trade:intents
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
		return nil, fmt.Errorf("parsing policy: %w", err)
	}
	// Env overrides.
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		p.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		p.Redis.Password = v
	}
	if os.Getenv("REDIS_TLS") == "true" {
		p.Redis.UseTLS = true
	}
	// Defaults.
	if p.EvalIntervalS == 0 {
		p.EvalIntervalS = 30
	}
	if p.IntentTTLMs == 0 {
		p.IntentTTLMs = 10000
	}
	if p.MaxSlippageBps == 0 {
		p.MaxSlippageBps = 8
	}
	if p.CooldownS == 0 {
		p.CooldownS = 300
	}
	return &p, nil
}
