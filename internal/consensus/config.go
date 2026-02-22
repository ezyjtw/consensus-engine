package consensus

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Policy struct {
	SizeNotionalUSD float64 `yaml:"size_notional_usd"`
	StaleMs         int64   `yaml:"stale_ms"`

	OutlierBpsWarn      float64 `yaml:"outlier_bps_warn"`
	OutlierBpsBlacklist float64 `yaml:"outlier_bps_blacklist"`

	WarnPersistMs      int64 `yaml:"warn_persist_ms"`
	BlacklistPersistMs int64 `yaml:"blacklist_persist_ms"`
	BlacklistTtlMs     int64 `yaml:"blacklist_ttl_ms"`
	RecoveryMs         int64 `yaml:"recovery_ms"`

	SpreadWarnBps float64 `yaml:"spread_warn_bps"`
	MinCoreQuorum int     `yaml:"min_core_quorum"`

	CoreVenues     []string           `yaml:"core_venues"`
	OptionalVenues []string           `yaml:"optional_venues"`
	Symbols        []string           `yaml:"symbols"`
	BaseTrust      map[string]float64 `yaml:"base_trust"`

	SlippageBufferBps float64 `yaml:"slippage_buffer_bps"`
	DepthPenaltyBps   float64 `yaml:"depth_penalty_bps"`

	Redis RedisConfig `yaml:"redis"`
}

type RedisConfig struct {
	Addr            string `yaml:"addr"`
	Password        string `yaml:"password"`
	UseTLS          bool   `yaml:"use_tls"`
	InputStream     string `yaml:"input_stream"`
	OutputConsensus string `yaml:"output_consensus"`
	OutputAnomalies string `yaml:"output_anomalies"`
	OutputStatus    string `yaml:"output_status"`
	ConsumerGroup   string `yaml:"consumer_group"`
	ConsumerName    string `yaml:"consumer_name"`
	BlockMs         int64  `yaml:"block_ms"`
	BatchSize       int64  `yaml:"batch_size"`
}

func LoadPolicy(path string) (*Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading policy file %s: %w", path, err)
	}
	var p Policy
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing policy YAML: %w", err)
	}
	p.ApplyEnvOverrides()
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("invalid policy: %w", err)
	}
	return &p, nil
}

func (p *Policy) ApplyEnvOverrides() {
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		p.Redis.Addr = addr
	}
	if pass := os.Getenv("REDIS_PASSWORD"); pass != "" {
		p.Redis.Password = pass
	}
	if strings.EqualFold(os.Getenv("REDIS_TLS"), "true") {
		p.Redis.UseTLS = true
	}
}

func (p *Policy) validate() error {
	if p.MinCoreQuorum < 1 {
		return fmt.Errorf("min_core_quorum must be >= 1")
	}
	if p.SizeNotionalUSD <= 0 {
		return fmt.Errorf("size_notional_usd must be > 0")
	}
	if len(p.CoreVenues) == 0 {
		return fmt.Errorf("core_venues must not be empty")
	}
	return nil
}

func (p *Policy) CoreVenueSet() map[Venue]bool {
	m := make(map[Venue]bool, len(p.CoreVenues))
	for _, v := range p.CoreVenues {
		m[Venue(v)] = true
	}
	return m
}
