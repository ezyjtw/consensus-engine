package arb

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ArbSymbolOverride allows per-symbol tuning of arb thresholds. Mid-cap
// tokens warrant higher minimum edge (wider spreads = less competition) and
// smaller size ladders (thinner liquidity). Fields left nil inherit from the
// top-level Policy defaults.
type ArbSymbolOverride struct {
	MinEdgeBpsNet    map[string]float64 `yaml:"min_edge_bps_net,omitempty"`
	LatencyBufferBps map[string]float64 `yaml:"latency_buffer_bps,omitempty"`
	SizeLadderUSD    []float64          `yaml:"size_ladder_usd,omitempty"`
	MaxSlippageBps   *float64           `yaml:"max_slippage_bps,omitempty"`
	CooldownMs       *int64             `yaml:"cooldown_ms,omitempty"`
}

// Policy is the full configuration for the arb opportunity engine.
type Policy struct {
	Symbols             []string              `yaml:"symbols"`
	MinConsensusQuality string                `yaml:"min_consensus_quality"`
	MinEdgeBpsNet       map[string]float64    `yaml:"min_edge_bps_net"`
	IntentTTLMs         map[string]int64      `yaml:"intent_ttl_ms"`
	LatencyBufferBps    map[string]float64    `yaml:"latency_buffer_bps"`
	SizeLadderUSD       []float64             `yaml:"size_ladder_usd"`
	MaxSlippageBps      float64               `yaml:"max_slippage_bps"`
	CooldownMs          int64                 `yaml:"cooldown_ms"`
	AllowWarnVenues     bool                  `yaml:"allow_warn_venues"`
	IgnoreFlaggedVenues bool                  `yaml:"ignore_flagged_venues"`
	EnabledPairs        map[string][][]string `yaml:"enabled_pairs"`

	// SymbolOverrides provides per-symbol arb threshold tuning. Keyed by
	// canonical symbol (e.g. "SOL-PERP"). Unset fields inherit from global.
	SymbolOverrides map[string]ArbSymbolOverride `yaml:"symbol_overrides"`

	BasisTrade  BasisTradePolicy `yaml:"basis_trade"`
	Cascade     CascadeConfig    `yaml:"cascade"`
	Correlation CorrelationConfig `yaml:"correlation"`
	DEXCEX      DEXCEXConfig     `yaml:"dex_cex"`
	Redis       ArbRedisPolicy   `yaml:"redis"`
}

// BasisTradePolicy configures the spot-futures basis convergence strategy.
type BasisTradePolicy struct {
	Enabled        bool    `yaml:"enabled"`
	MinBasisBps    float64 `yaml:"min_basis_bps"`
	MinZScore      float64 `yaml:"min_z_score"`
	MaxNotionalUSD float64 `yaml:"max_notional_usd"`
	MaxSlippageBps float64 `yaml:"max_slippage_bps"`
	IntentTTLMs    int64   `yaml:"intent_ttl_ms"`
	CooldownMs     int64   `yaml:"cooldown_ms"`
	WindowSize     int     `yaml:"window_size"`
	EvalIntervalS  int     `yaml:"eval_interval_s"`
}

// ArbRedisPolicy holds the Redis connection and stream configuration.
type ArbRedisPolicy struct {
	Addr               string `yaml:"addr"`
	Password           string `yaml:"password"`
	UseTLS             bool   `yaml:"use_tls"`
	InputStream        string `yaml:"input_stream"`
	MarketQuotesStream string `yaml:"market_quotes_stream"`
	OIStream           string `yaml:"oi_stream"`
	OutputIntents      string `yaml:"output_intents"`
	ConsumerGroup      string `yaml:"consumer_group"`
	ConsumerName       string `yaml:"consumer_name"`
	BlockMs            int64  `yaml:"block_ms"`
	BatchSize          int64  `yaml:"batch_size"`
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
	p.applyEnvOverrides()
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("invalid policy: %w", err)
	}
	return &p, nil
}

func (p *Policy) applyEnvOverrides() {
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
	if len(p.Symbols) == 0 {
		return fmt.Errorf("symbols must not be empty")
	}
	if len(p.SizeLadderUSD) == 0 {
		return fmt.Errorf("size_ladder_usd must not be empty")
	}
	if p.MaxSlippageBps <= 0 {
		return fmt.Errorf("max_slippage_bps must be > 0")
	}
	if p.CooldownMs < 0 {
		return fmt.Errorf("cooldown_ms must be >= 0")
	}
	return nil
}

// minEdge returns the minimum net edge bps for the given quality and symbol.
// Per-symbol overrides take precedence, then global, then 9999 (never trade).
func (p *Policy) minEdge(quality, symbol string) float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.MinEdgeBpsNet != nil {
		if v, ok := ovr.MinEdgeBpsNet[quality]; ok {
			return v
		}
	}
	if v, ok := p.MinEdgeBpsNet[quality]; ok {
		return v
	}
	return 9999
}

// intentTTL returns the intent TTL in milliseconds for the given quality.
// Returns 0 when not configured (no intents should be emitted).
func (p *Policy) intentTTL(quality string) int64 {
	if v, ok := p.IntentTTLMs[quality]; ok {
		return v
	}
	return 0
}

// latencyBuffer returns the per-leg latency buffer in bps for the given
// quality and symbol. Per-symbol overrides take precedence.
func (p *Policy) latencyBuffer(quality, symbol string) float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.LatencyBufferBps != nil {
		if v, ok := ovr.LatencyBufferBps[quality]; ok {
			return v
		}
	}
	if v, ok := p.LatencyBufferBps[quality]; ok {
		return v
	}
	return 10
}

// sizeLadder returns the size ladder for a symbol, falling back to global.
func (p *Policy) sizeLadder(symbol string) []float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && len(ovr.SizeLadderUSD) > 0 {
		return ovr.SizeLadderUSD
	}
	return p.SizeLadderUSD
}

// maxSlippage returns the max slippage for a symbol, falling back to global.
func (p *Policy) maxSlippage(symbol string) float64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.MaxSlippageBps != nil {
		return *ovr.MaxSlippageBps
	}
	return p.MaxSlippageBps
}

// cooldown returns the cooldown in ms for a symbol, falling back to global.
func (p *Policy) cooldown(symbol string) int64 {
	if ovr, ok := p.SymbolOverrides[symbol]; ok && ovr.CooldownMs != nil {
		return *ovr.CooldownMs
	}
	return p.CooldownMs
}

// qualityRank returns a numeric ordering: HIGH(2) > MED(1) > LOW(0).
func qualityRank(q string) int {
	switch q {
	case "HIGH":
		return 2
	case "MED":
		return 1
	default:
		return 0
	}
}
