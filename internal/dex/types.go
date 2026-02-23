package dex

// Provider identifies a DEX aggregator.
type Provider string

const (
	Provider1inch    Provider = "1inch"
	ProviderParaswap Provider = "paraswap"
)

// QuoteRequest specifies a DEX spot-leg swap quote.
type QuoteRequest struct {
	ChainID    int     `json:"chain_id"`    // 1=Ethereum, 42161=Arbitrum, 10=Optimism
	FromToken  string  `json:"from_token"`  // contract address or well-known symbol
	ToToken    string  `json:"to_token"`
	AmountWei  string  `json:"amount_wei"`  // big-int string to avoid float overflow
	Slippage   float64 `json:"slippage"`    // max slippage percent, e.g. 0.5
	MEVProtect bool    `json:"mev_protect"` // route through Flashbots / 1inch Fusion
}

// QuoteResponse is a normalised swap quote from any provider.
type QuoteResponse struct {
	Provider       Provider `json:"provider"`
	FromToken      string   `json:"from_token"`
	ToToken        string   `json:"to_token"`
	FromAmount     string   `json:"from_amount"`
	ToAmount       string   `json:"to_amount"`
	EstGasWei      string   `json:"est_gas_wei"`
	PriceImpactPct float64  `json:"price_impact_pct"`
	RouterAddr     string   `json:"router_addr,omitempty"`
	CallData       string   `json:"call_data,omitempty"`
}

// SwapResult captures the outcome of a submitted DEX swap.
type SwapResult struct {
	TxHash      string `json:"tx_hash"`
	Status      string `json:"status"`       // submitted/confirmed/failed
	GasUsed     uint64 `json:"gas_used"`
	FilledAtMs  int64  `json:"filled_at_ms"`
}
