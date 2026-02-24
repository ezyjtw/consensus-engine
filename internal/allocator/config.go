package allocator

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	PerStrategyMaxUSD        map[string]float64 `yaml:"per_strategy_max_usd"`
	PerVenueMaxUSD           map[string]float64 `yaml:"per_venue_max_usd"`
	MarginUtilisationGatePct float64            `yaml:"margin_utilisation_gate_pct"`
	MinQualityForArb         string             `yaml:"min_quality_for_arb"`
	MinQualityForFunding     string             `yaml:"min_quality_for_funding"`
	MinQualityForLiquidity   string             `yaml:"min_quality_for_liquidity"`

	// MaxSingleIntentPct limits any single intent's notional as a percentage of
	// its strategy cap. Prevents position concentration risk.
	// 0 = disabled. 25 = no single intent may exceed 25% of the strategy cap.
	MaxSingleIntentPct float64 `yaml:"max_single_intent_pct"`

	// KellyFraction applies fractional Kelly position sizing to each approved intent.
	// 0 = disabled (full notional up to cap).
	// 0.25 = quarter-Kelly (recommended for crypto — conservative and risk-adjusted).
	// 0.5  = half-Kelly.
	// The Kelly-adjusted size = intent.notional * KellyFraction * (edge_bps / edge_variance_bps).
	// When edge_variance_bps is 0 (not provided), the fraction alone is applied as a
	// simple proportional scale against the strategy cap.
	KellyFraction float64 `yaml:"kelly_fraction"`

	// BaselineOI is the reference open interest (contracts) for OI-gated sizing.
	// When current OI falls below this, position sizes are reduced.
	// 0 = OI gating disabled.
	BaselineOI float64 `yaml:"baseline_oi"`

	// OIStream is the Redis stream for open interest updates.
	OIStream string `yaml:"oi_stream"`

	Redis RedisPolicy `yaml:"redis"`
}

type RedisPolicy struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	InputStream   string `yaml:"input_stream"`    // trade:intents
	OutputStream  string `yaml:"output_stream"`   // trade:intents:approved
	FillsStream   string `yaml:"fills_stream"`    // demo:fills | live:fills for notional release
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
	return &p, nil
}
