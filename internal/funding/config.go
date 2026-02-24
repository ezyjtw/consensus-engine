package funding

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// FundingStage controls the operational mode for a symbol in the funding engine.
// Symbols progress through stages as confidence in data quality and strategy
// performance grows: OBSERVE → PAPER → CONSERVATIVE → FULL.
type FundingStage string

const (
	// StageObserve monitors funding rates and logs what the engine would do,
	// but emits no intents. Use for validating data quality on new symbols.
	StageObserve FundingStage = "OBSERVE"

	// StagePaper emits intents tagged with ForcePaperMode so the execution
	// router simulates fills. Tracks theoretical P&L before risking capital.
	StagePaper FundingStage = "PAPER"

	// StageConservative emits real intents at reduced notional (controlled by
	// SizeScaleFactor, default 0.5). A gradual exposure ramp.
	StageConservative FundingStage = "CONSERVATIVE"

	// StageFull is normal operation with full notional limits. This is the
	// default when no stage is specified, maintaining backwards compatibility.
	StageFull FundingStage = "FULL"
)

// FundingSymbolOverride allows per-symbol tuning of funding strategy parameters.
// Mid-cap tokens warrant smaller position sizes, wider slippage tolerance, and
// staged rollout to validate strategy performance before committing capital.
type FundingSymbolOverride struct {
	MaxNotionalUSD    *float64            `yaml:"max_notional_usd,omitempty"`
	MaxSlippageBps    *float64            `yaml:"max_slippage_bps,omitempty"`
	Stage             FundingStage        `yaml:"stage,omitempty"`
	SizeScaleFactor   *float64            `yaml:"size_scale_factor,omitempty"`
	MinAnnualYieldPct map[string]float64  `yaml:"min_annual_yield_pct,omitempty"`
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

// stage returns the funding stage for a symbol. Defaults to FULL for
// backwards compatibility (all symbols trade at full notional unless overridden).
func (p *Policy) stage(symbol string) FundingStage {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.Stage != "" {
		return ovr.Stage
	}
	return StageFull
}

// sizeScale returns the notional scaling factor for CONSERVATIVE stage.
// Returns 1.0 for FULL or any non-CONSERVATIVE stage.
func (p *Policy) sizeScale(symbol string) float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.SizeScaleFactor != nil {
		return *ovr.SizeScaleFactor
	}
	return 0.5 // default conservative scale
}

// minYieldForSymbol returns the per-symbol yield threshold for the given quality,
// falling back to the global MinAnnualYieldPct.
func (p *Policy) minYieldForSymbol(symbol, quality string) (float64, bool) {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.MinAnnualYieldPct != nil {
		if v, ok := ovr.MinAnnualYieldPct[quality]; ok {
			return v, true
		}
	}
	if v, ok := p.MinAnnualYieldPct[quality]; ok {
		return v, true
	}
	return 0, false
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
