package l2

// Config holds L2 transfer routing configuration.
type Config struct {
	Enabled       bool               `yaml:"enabled"`
	PreferNetwork Network            `yaml:"prefer_network"` // arbitrum/optimism/base
	Networks      map[Network]NetCfg `yaml:"networks"`
	MaxGasGwei    float64            `yaml:"max_gas_gwei"`    // reject if estimated gas exceeds this
	MinSavingsUSD float64            `yaml:"min_savings_usd"` // min cost saving vs L1 to use L2
}

// NetCfg holds per-network configuration.
type NetCfg struct {
	RPCURL         string `yaml:"rpc_url"`
	BridgeContract string `yaml:"bridge_contract"` // canonical bridge contract address
	ExplorerURL    string `yaml:"explorer_url"`
}
