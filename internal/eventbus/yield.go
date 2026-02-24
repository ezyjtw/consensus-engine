package eventbus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Stream names for yield and on-chain infrastructure.
const (
	StreamDEXPoolState      = "dex:pool_state"
	StreamDEXQuotes         = "dex:quotes"
	StreamLendingRates      = "lending:rates"
	StreamLiquidationCands  = "liquidation:candidates"
	StreamOnchainTxEvents   = "onchain:tx_events"
	StreamBridgeStatus      = "bridge:status"
	StreamYieldAllocations  = "yield:allocations"
	StreamKeeperEvents      = "keeper:events"
)

// DEXPoolStateEvent is published when a DEX pool's state changes.
type DEXPoolStateEvent struct {
	SchemaVersion int     `json:"schema_version"`
	PoolAddr      string  `json:"pool_addr"`
	ChainID       uint64  `json:"chain_id"`
	Protocol      string  `json:"protocol"`
	Token0        string  `json:"token0"`
	Token1        string  `json:"token1"`
	Reserve0      float64 `json:"reserve0"`
	Reserve1      float64 `json:"reserve1"`
	TVLUSD        float64 `json:"tvl_usd"`
	Price         float64 `json:"price"`
	FeeRate       float64 `json:"fee_rate"`
	TsMs          int64   `json:"ts_ms"`
}

// DEXQuoteEvent is published when a DEX quote is requested/received.
type DEXQuoteEvent struct {
	SchemaVersion int     `json:"schema_version"`
	PoolAddr      string  `json:"pool_addr"`
	ChainID       uint64  `json:"chain_id"`
	TokenIn       string  `json:"token_in"`
	TokenOut      string  `json:"token_out"`
	AmountInUSD   float64 `json:"amount_in_usd"`
	AmountOutUSD  float64 `json:"amount_out_usd"`
	PriceImpactBps float64 `json:"price_impact_bps"`
	GasCostUSD    float64 `json:"gas_cost_usd"`
	TsMs          int64   `json:"ts_ms"`
}

// LendingRateEvent is published when lending rates update.
type LendingRateEvent struct {
	SchemaVersion int     `json:"schema_version"`
	Protocol      string  `json:"protocol"`
	ChainID       uint64  `json:"chain_id"`
	Asset         string  `json:"asset"`
	SupplyAPY     float64 `json:"supply_apy"`
	BorrowAPY     float64 `json:"borrow_apy"`
	Utilization   float64 `json:"utilization"`
	TotalSupplyUSD float64 `json:"total_supply_usd"`
	TotalBorrowUSD float64 `json:"total_borrow_usd"`
	TsMs          int64   `json:"ts_ms"`
}

// LiquidationCandidateEvent is published when a liquidation opportunity is found.
type LiquidationCandidateEvent struct {
	SchemaVersion int     `json:"schema_version"`
	Protocol      string  `json:"protocol"`
	ChainID       uint64  `json:"chain_id"`
	Account       string  `json:"account"`
	CollateralAsset string `json:"collateral_asset"`
	DebtAsset     string  `json:"debt_asset"`
	HealthFactor  float64 `json:"health_factor"`
	ProfitUSD     float64 `json:"profit_usd"`
	GasCostUSD    float64 `json:"gas_cost_usd"`
	NetProfitUSD  float64 `json:"net_profit_usd"`
	TsMs          int64   `json:"ts_ms"`
}

// OnchainTxEvent is published for on-chain transaction lifecycle events.
type OnchainTxEvent struct {
	SchemaVersion int    `json:"schema_version"`
	TxHash        string `json:"tx_hash"`
	ChainID       uint64 `json:"chain_id"`
	From          string `json:"from"`
	To            string `json:"to"`
	Status        string `json:"status"` // SUBMITTED, CONFIRMED, FAILED, DROPPED
	GasUsed       uint64 `json:"gas_used"`
	GasCostUSD    float64 `json:"gas_cost_usd"`
	Nonce         uint64 `json:"nonce"`
	TsMs          int64  `json:"ts_ms"`
}

// BridgeStatusEvent is published for bridge transfer status changes.
type BridgeStatusEvent struct {
	SchemaVersion int     `json:"schema_version"`
	TransferID    string  `json:"transfer_id"`
	Bridge        string  `json:"bridge"`
	SourceChain   uint64  `json:"source_chain"`
	DestChain     uint64  `json:"dest_chain"`
	Token         string  `json:"token"`
	AmountUSD     float64 `json:"amount_usd"`
	Status        string  `json:"status"`
	TsMs          int64   `json:"ts_ms"`
}

// YieldAllocationEvent is published when the treasury yield router rebalances.
type YieldAllocationEvent struct {
	SchemaVersion int     `json:"schema_version"`
	Action        string  `json:"action"` // DEPOSIT, WITHDRAW, SHIFT
	SourceID      string  `json:"source_id"`
	Protocol      string  `json:"protocol"`
	Asset         string  `json:"asset"`
	AmountUSD     float64 `json:"amount_usd"`
	ExpAPYPct     float64 `json:"exp_apy_pct"`
	Reason        string  `json:"reason"`
	TsMs          int64   `json:"ts_ms"`
}

// KeeperEvent is published for keeper execution lifecycle events.
type KeeperEvent struct {
	SchemaVersion int     `json:"schema_version"`
	Type          string  `json:"type"` // SCAN, EXECUTE, SUCCESS, FAILURE
	Protocol      string  `json:"protocol"`
	Account       string  `json:"account"`
	ProfitUSD     float64 `json:"profit_usd"`
	GasCostUSD    float64 `json:"gas_cost_usd"`
	TxHash        string  `json:"tx_hash,omitempty"`
	Error         string  `json:"error,omitempty"`
	TsMs          int64   `json:"ts_ms"`
}

// YieldRedisConfig configures the yield bus Redis connection.
type YieldRedisConfig struct {
	Addr          string
	Password      string
	UseTLS        bool
	ConsumerGroup string
	ConsumerName  string
	BlockMs       time.Duration
}

// YieldBus handles all Redis stream I/O for yield and on-chain services.
type YieldBus struct {
	rdb *redis.Client
	cfg YieldRedisConfig
}

// NewYieldBus creates a yield bus Redis client.
func NewYieldBus(cfg YieldRedisConfig) (*YieldBus, error) {
	opts, err := buildYieldRedisOptions(cfg)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opts)

	ctx := context.Background()
	// Create consumer groups for all yield streams
	streams := []string{
		StreamDEXPoolState, StreamDEXQuotes, StreamLendingRates,
		StreamLiquidationCands, StreamOnchainTxEvents, StreamBridgeStatus,
		StreamYieldAllocations, StreamKeeperEvents,
	}
	for _, s := range streams {
		err := rdb.XGroupCreateMkStream(ctx, s, cfg.ConsumerGroup, "$").Err()
		if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
			log.Printf("yield bus: consumer group on %q: %v (ok if already exists)", s, err)
		}
	}

	return &YieldBus{rdb: rdb, cfg: cfg}, nil
}

// PublishDEXPoolState publishes a DEX pool state event.
func (b *YieldBus) PublishDEXPoolState(ctx context.Context, evt DEXPoolStateEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamDEXPoolState, evt)
}

// PublishLendingRate publishes a lending rate event.
func (b *YieldBus) PublishLendingRate(ctx context.Context, evt LendingRateEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamLendingRates, evt)
}

// PublishLiquidationCandidate publishes a liquidation candidate event.
func (b *YieldBus) PublishLiquidationCandidate(ctx context.Context, evt LiquidationCandidateEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamLiquidationCands, evt)
}

// PublishOnchainTx publishes an on-chain transaction event.
func (b *YieldBus) PublishOnchainTx(ctx context.Context, evt OnchainTxEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamOnchainTxEvents, evt)
}

// PublishBridgeStatus publishes a bridge status event.
func (b *YieldBus) PublishBridgeStatus(ctx context.Context, evt BridgeStatusEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamBridgeStatus, evt)
}

// PublishYieldAllocation publishes a yield allocation event.
func (b *YieldBus) PublishYieldAllocation(ctx context.Context, evt YieldAllocationEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamYieldAllocations, evt)
}

// PublishKeeperEvent publishes a keeper lifecycle event.
func (b *YieldBus) PublishKeeperEvent(ctx context.Context, evt KeeperEvent) error {
	evt.SchemaVersion = 1
	return b.publish(ctx, StreamKeeperEvents, evt)
}

func (b *YieldBus) publish(ctx context.Context, stream string, evt interface{}) error {
	data, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("marshalling event: %w", err)
	}
	return b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// Close releases the underlying Redis connection.
func (b *YieldBus) Close() error {
	return b.rdb.Close()
}

func buildYieldRedisOptions(cfg YieldRedisConfig) (*redis.Options, error) {
	if strings.HasPrefix(cfg.Addr, "redis://") || strings.HasPrefix(cfg.Addr, "rediss://") {
		opts, err := redis.ParseURL(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %w", err)
		}
		if cfg.Password != "" {
			opts.Password = cfg.Password
		}
		if cfg.UseTLS && opts.TLSConfig == nil {
			opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		return opts, nil
	}
	opts := &redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
	}
	if cfg.UseTLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}
