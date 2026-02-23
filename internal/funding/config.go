package funding

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

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
	Redis                  RedisPolicy        `yaml:"redis"`
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
