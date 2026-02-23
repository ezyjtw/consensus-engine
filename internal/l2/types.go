package l2

// Network identifies an L2 network.
type Network string

const (
	NetworkArbitrum Network = "arbitrum"
	NetworkOptimism Network = "optimism"
	NetworkBase     Network = "base"
)

// ChainIDs maps network names to EVM chain IDs.
var ChainIDs = map[Network]int{
	NetworkArbitrum: 42161,
	NetworkOptimism: 10,
	NetworkBase:     8453,
}

// BridgeRequest specifies an L2 transfer.
type BridgeRequest struct {
	Network     Network `json:"network"`
	FromAddress string  `json:"from_address"`
	ToAddress   string  `json:"to_address"`
	TokenSymbol string  `json:"token_symbol"` // ETH, USDC, USDT, etc.
	AmountWei   string  `json:"amount_wei"`   // big-int string
	MaxGasWei   string  `json:"max_gas_wei"`  // gas budget; "" = auto-estimate
}

// BridgeEstimate holds cost/time estimates before committing.
type BridgeEstimate struct {
	Network         Network `json:"network"`
	L1GasCostWei    string  `json:"l1_gas_cost_wei"`
	L2GasCostWei    string  `json:"l2_gas_cost_wei"`
	TotalCostUSD    float64 `json:"total_cost_usd"`
	EstSettlementMs int64   `json:"est_settlement_ms"`
}

// BridgeResult captures the outcome of a submitted bridge transaction.
type BridgeResult struct {
	L1TxHash    string `json:"l1_tx_hash"`
	L2TxHash    string `json:"l2_tx_hash,omitempty"`
	Status      string `json:"status"`        // submitted/pending/completed/failed
	SubmittedAt int64  `json:"submitted_at_ms"`
}
