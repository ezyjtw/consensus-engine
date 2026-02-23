package allocator

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	PerStrategyMaxUSD       map[string]float64 `yaml:"per_strategy_max_usd"`
	PerVenueMaxUSD          map[string]float64 `yaml:"per_venue_max_usd"`
	MarginUtilisationGatePct float64           `yaml:"margin_utilisation_gate_pct"`
	MinQualityForArb        string             `yaml:"min_quality_for_arb"`
	MinQualityForFunding    string             `yaml:"min_quality_for_funding"`
	MinQualityForLiquidity  string             `yaml:"min_quality_for_liquidity"`
	Redis                   RedisPolicy        `yaml:"redis"`
}

type RedisPolicy struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	InputStream   string `yaml:"input_stream"`    // trade:intents
	OutputStream  string `yaml:"output_stream"`   // trade:intents:approved
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
