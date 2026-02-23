package dex

// Config holds DEX spot-leg routing configuration.
type Config struct {
	Enabled            bool     `yaml:"enabled"`
	PreferredProvider  Provider `yaml:"preferred_provider"`   // "1inch" or "paraswap"
	OneInchAPIKey      string   `yaml:"one_inch_api_key"`
	ParaswapAPIKey     string   `yaml:"paraswap_api_key"`
	DefaultChainID     int      `yaml:"default_chain_id"`     // 1, 42161, 10
	MaxSlippagePct     float64  `yaml:"max_slippage_pct"`     // reject quotes above this
	MaxPriceImpactPct  float64  `yaml:"max_price_impact_pct"` // reject quotes above this
	MEVProtect         bool     `yaml:"mev_protect"`          // use Flashbots/1inch Fusion
	GasLimitMultiplier float64  `yaml:"gas_limit_multiplier"` // safety headroom, e.g. 1.2
}
