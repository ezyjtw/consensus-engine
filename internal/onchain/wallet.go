// Package onchain provides on-chain execution infrastructure including wallet
// management, nonce tracking, transaction building, and simulation.
package onchain

import (
	"encoding/hex"
	"errors"
	"math/big"
	"sync"
	"time"
)

// ChainID identifies an EVM-compatible chain.
type ChainID uint64

const (
	Ethereum ChainID = 1
	Arbitrum ChainID = 42161
	Optimism ChainID = 10
	Base     ChainID = 8453
	Polygon  ChainID = 137
	BSC      ChainID = 56
)

// WalletConfig configures a hot wallet.
type WalletConfig struct {
	ChainID         ChainID `yaml:"chain_id"`
	RPCEndpoint     string  `yaml:"rpc_endpoint"`
	MaxPendingTxs   int     `yaml:"max_pending_txs"`
	GasLimitDefault uint64  `yaml:"gas_limit_default"`
	MaxGasPriceGwei float64 `yaml:"max_gas_price_gwei"`
	ConfirmTimeoutS int     `yaml:"confirm_timeout_s"`
}

// Wallet manages signing keys and nonce tracking for on-chain execution.
type Wallet struct {
	mu           sync.Mutex
	address      string
	nonces       map[ChainID]uint64
	pendingTxs   map[string]*PendingTx // txHash → pending
	maxPending   int
	rpcEndpoints map[ChainID]string
	gasConfig    GasConfig
}

// GasConfig controls gas price and limit behaviour.
type GasConfig struct {
	MaxGasPriceGwei  float64 `yaml:"max_gas_price_gwei"`
	PriorityFeeGwei  float64 `yaml:"priority_fee_gwei"`
	GasLimitDefault  uint64  `yaml:"gas_limit_default"`
	GasLimitMultiple float64 `yaml:"gas_limit_multiple"` // multiply estimated gas
}

// PendingTx tracks an in-flight transaction.
type PendingTx struct {
	Hash        string    `json:"hash"`
	ChainID     ChainID   `json:"chain_id"`
	Nonce       uint64    `json:"nonce"`
	To          string    `json:"to"`
	Value       *big.Int  `json:"value"`
	Data        []byte    `json:"data"`
	GasLimit    uint64    `json:"gas_limit"`
	GasPrice    *big.Int  `json:"gas_price"`
	SubmittedAt time.Time `json:"submitted_at"`
	Status      TxStatus  `json:"status"`
	Confirmations int     `json:"confirmations"`
}

// TxStatus represents on-chain transaction state.
type TxStatus string

const (
	TxPending   TxStatus = "PENDING"
	TxConfirmed TxStatus = "CONFIRMED"
	TxFailed    TxStatus = "FAILED"
	TxDropped   TxStatus = "DROPPED"
	TxReplaced  TxStatus = "REPLACED"
)

// NewWallet creates a wallet manager.
func NewWallet(address string, maxPending int) *Wallet {
	return &Wallet{
		address:      address,
		nonces:       make(map[ChainID]uint64),
		pendingTxs:   make(map[string]*PendingTx),
		maxPending:   maxPending,
		rpcEndpoints: make(map[ChainID]string),
		gasConfig: GasConfig{
			MaxGasPriceGwei:  100,
			PriorityFeeGwei:  1.5,
			GasLimitDefault:  300000,
			GasLimitMultiple: 1.2,
		},
	}
}

// Address returns the wallet address.
func (w *Wallet) Address() string { return w.address }

// SetRPC configures an RPC endpoint for a chain.
func (w *Wallet) SetRPC(chain ChainID, endpoint string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rpcEndpoints[chain] = endpoint
}

// SetNonce sets the current nonce for a chain (from on-chain state).
func (w *Wallet) SetNonce(chain ChainID, nonce uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nonces[chain] = nonce
}

// NextNonce returns and increments the nonce for a chain.
func (w *Wallet) NextNonce(chain ChainID) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.nonces[chain]
	w.nonces[chain] = n + 1
	return n
}

// PendingCount returns the number of pending transactions.
func (w *Wallet) PendingCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pendingCountLocked()
}

func (w *Wallet) pendingCountLocked() int {
	count := 0
	for _, tx := range w.pendingTxs {
		if tx.Status == TxPending {
			count++
		}
	}
	return count
}

// CanSubmit checks if we can submit more transactions.
func (w *Wallet) CanSubmit() bool {
	return w.PendingCount() < w.maxPending
}

// TrackTx records a pending transaction for monitoring.
func (w *Wallet) TrackTx(tx *PendingTx) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pendingCountLocked() >= w.maxPending {
		return errors.New("max pending transactions reached")
	}
	tx.Status = TxPending
	tx.SubmittedAt = time.Now()
	w.pendingTxs[tx.Hash] = tx
	return nil
}

// UpdateTxStatus updates the status of a tracked transaction.
func (w *Wallet) UpdateTxStatus(hash string, status TxStatus, confirmations int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if tx, ok := w.pendingTxs[hash]; ok {
		tx.Status = status
		tx.Confirmations = confirmations
	}
}

// PendingTxs returns all currently pending transactions.
func (w *Wallet) PendingTxs() []*PendingTx {
	w.mu.Lock()
	defer w.mu.Unlock()
	var result []*PendingTx
	for _, tx := range w.pendingTxs {
		if tx.Status == TxPending {
			result = append(result, tx)
		}
	}
	return result
}

// PurgeConfirmed removes confirmed/failed transactions older than cutoff.
func (w *Wallet) PurgeConfirmed(cutoff time.Duration) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	threshold := time.Now().Add(-cutoff)
	removed := 0
	for hash, tx := range w.pendingTxs {
		if tx.Status != TxPending && tx.SubmittedAt.Before(threshold) {
			delete(w.pendingTxs, hash)
			removed++
		}
	}
	return removed
}

// NonceManager provides high-throughput nonce management with gap detection.
type NonceManager struct {
	mu       sync.Mutex
	wallets  map[string]*Wallet // address → wallet
	gapQueue map[ChainID][]uint64
}

// NewNonceManager creates a nonce manager.
func NewNonceManager() *NonceManager {
	return &NonceManager{
		wallets:  make(map[string]*Wallet),
		gapQueue: make(map[ChainID][]uint64),
	}
}

// RegisterWallet adds a wallet to the nonce manager.
func (nm *NonceManager) RegisterWallet(w *Wallet) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.wallets[w.address] = w
}

// AcquireNonce gets the next available nonce, filling gaps first.
func (nm *NonceManager) AcquireNonce(address string, chain ChainID) (uint64, error) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	w, ok := nm.wallets[address]
	if !ok {
		return 0, errors.New("wallet not registered")
	}

	// Try to fill a gap first
	if gaps, ok := nm.gapQueue[chain]; ok && len(gaps) > 0 {
		nonce := gaps[0]
		nm.gapQueue[chain] = gaps[1:]
		return nonce, nil
	}

	return w.NextNonce(chain), nil
}

// ReleaseNonce returns a nonce to the gap queue (for failed transactions).
func (nm *NonceManager) ReleaseNonce(chain ChainID, nonce uint64) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.gapQueue[chain] = append(nm.gapQueue[chain], nonce)
}

// TxBuilder constructs unsigned transaction payloads.
type TxBuilder struct {
	defaultGasLimit uint64
	chainConfigs    map[ChainID]TxBuildConfig
}

// TxBuildConfig holds per-chain build configuration.
type TxBuildConfig struct {
	ChainID     ChainID `yaml:"chain_id"`
	IsEIP1559   bool    `yaml:"is_eip1559"`
	BaseFeeGwei float64 `yaml:"base_fee_gwei"`
}

// UnsignedTx is a transaction ready for signing.
type UnsignedTx struct {
	ChainID  ChainID  `json:"chain_id"`
	To       string   `json:"to"`
	Value    *big.Int `json:"value"`
	Data     []byte   `json:"data"`
	GasLimit uint64   `json:"gas_limit"`
	Nonce    uint64   `json:"nonce"`
	// EIP-1559 fields
	MaxFeePerGas         *big.Int `json:"max_fee_per_gas,omitempty"`
	MaxPriorityFeePerGas *big.Int `json:"max_priority_fee_per_gas,omitempty"`
	// Legacy
	GasPrice *big.Int `json:"gas_price,omitempty"`
}

// NewTxBuilder creates a transaction builder.
func NewTxBuilder(defaultGasLimit uint64) *TxBuilder {
	return &TxBuilder{
		defaultGasLimit: defaultGasLimit,
		chainConfigs:    make(map[ChainID]TxBuildConfig),
	}
}

// SetChainConfig configures a chain for building.
func (b *TxBuilder) SetChainConfig(cfg TxBuildConfig) {
	b.chainConfigs[cfg.ChainID] = cfg
}

// BuildSwap constructs a DEX swap transaction.
func (b *TxBuilder) BuildSwap(chain ChainID, router, tokenIn, tokenOut string, amountIn *big.Int, minAmountOut *big.Int, deadline int64) (*UnsignedTx, error) {
	// Encode swap function selector + params
	// swapExactTokensForTokens(uint256,uint256,address[],address,uint256)
	selector, _ := hex.DecodeString("38ed1739")
	data := make([]byte, len(selector))
	copy(data, selector)
	// In production, full ABI encoding would go here

	gasLimit := b.defaultGasLimit
	if cfg, ok := b.chainConfigs[chain]; ok && cfg.IsEIP1559 {
		return &UnsignedTx{
			ChainID:  chain,
			To:       router,
			Value:    big.NewInt(0),
			Data:     data,
			GasLimit: gasLimit,
		}, nil
	}

	return &UnsignedTx{
		ChainID:  chain,
		To:       router,
		Value:    big.NewInt(0),
		Data:     data,
		GasLimit: gasLimit,
	}, nil
}

// BuildApproval constructs an ERC-20 approval transaction.
func (b *TxBuilder) BuildApproval(chain ChainID, token, spender string, amount *big.Int) (*UnsignedTx, error) {
	// approve(address,uint256) selector
	selector, _ := hex.DecodeString("095ea7b3")
	data := make([]byte, len(selector))
	copy(data, selector)

	return &UnsignedTx{
		ChainID:  chain,
		To:       token,
		Value:    big.NewInt(0),
		Data:     data,
		GasLimit: 60000,
	}, nil
}

// TxSimulator simulates transactions before submission.
type TxSimulator struct {
	mu          sync.RWMutex
	simResults  map[string]*SimResult
	maxResults  int
}

// SimResult holds simulation output.
type SimResult struct {
	TxHash      string  `json:"tx_hash"`
	Success     bool    `json:"success"`
	GasUsed     uint64  `json:"gas_used"`
	ReturnData  []byte  `json:"return_data"`
	RevertReason string `json:"revert_reason,omitempty"`
	StateChanges []StateChange `json:"state_changes"`
	SimulatedAt time.Time `json:"simulated_at"`
}

// StateChange describes a simulated state diff.
type StateChange struct {
	Address  string `json:"address"`
	Slot     string `json:"slot"`
	OldValue string `json:"old_value"`
	NewValue string `json:"new_value"`
}

// NewTxSimulator creates a transaction simulator.
func NewTxSimulator(maxResults int) *TxSimulator {
	return &TxSimulator{
		simResults: make(map[string]*SimResult),
		maxResults: maxResults,
	}
}

// Simulate runs a transaction simulation (stub — real impl would call eth_call / tenderly).
func (s *TxSimulator) Simulate(tx *UnsignedTx) *SimResult {
	result := &SimResult{
		Success:     true,
		GasUsed:     tx.GasLimit * 7 / 10, // estimate 70% usage
		SimulatedAt: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Capacity management
	if len(s.simResults) >= s.maxResults {
		// Remove oldest
		var oldestKey string
		var oldestTime time.Time
		for k, v := range s.simResults {
			if oldestKey == "" || v.SimulatedAt.Before(oldestTime) {
				oldestKey = k
				oldestTime = v.SimulatedAt
			}
		}
		delete(s.simResults, oldestKey)
	}

	return result
}

// RecentResults returns the most recent simulation results.
func (s *TxSimulator) RecentResults(limit int) []*SimResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var results []*SimResult
	for _, r := range s.simResults {
		results = append(results, r)
		if len(results) >= limit {
			break
		}
	}
	return results
}
