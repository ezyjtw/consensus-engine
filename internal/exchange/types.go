// Package exchange defines the unified interface for interacting with
// cryptocurrency exchange REST APIs. Every venue adapter (Coinbase, Binance,
// OKX, Bybit, Deribit) implements this interface so the live executor,
// treasury, and reconciliation services can work against any exchange.
package exchange

import "time"

// Side represents the direction of an order.
type Side string

const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// OrderType describes how an order should be matched.
type OrderType string

const (
	OrderTypeMarket OrderType = "MARKET"
	OrderTypeLimit  OrderType = "LIMIT"
	OrderTypeIOC    OrderType = "IOC" // immediate-or-cancel
)

// OrderStatus tracks the lifecycle of an exchange order.
type OrderStatus string

const (
	OrderStatusNew             OrderStatus = "NEW"
	OrderStatusPartiallyFilled OrderStatus = "PARTIALLY_FILLED"
	OrderStatusFilled          OrderStatus = "FILLED"
	OrderStatusCancelled       OrderStatus = "CANCELLED"
	OrderStatusRejected        OrderStatus = "REJECTED"
	OrderStatusExpired         OrderStatus = "EXPIRED"
)

// TransferStatus tracks the lifecycle of a withdrawal or deposit.
type TransferStatus string

const (
	TransferPending   TransferStatus = "PENDING"
	TransferCompleted TransferStatus = "COMPLETED"
	TransferFailed    TransferStatus = "FAILED"
)

// OrderRequest is the input for placing an order.
type OrderRequest struct {
	Symbol        string    `json:"symbol"`          // exchange-native symbol (e.g. BTCUSDT)
	Side          Side      `json:"side"`
	Type          OrderType `json:"type"`
	Quantity      float64   `json:"quantity"`         // base asset qty
	Price         float64   `json:"price,omitempty"`  // required for LIMIT
	NotionalUSD   float64   `json:"notional_usd"`     // informational
	MaxSlippageBps float64  `json:"max_slippage_bps"`
	ClientOrderID string    `json:"client_order_id"`
	ReduceOnly    bool      `json:"reduce_only,omitempty"`
}

// OrderResponse is the result of placing or querying an order.
type OrderResponse struct {
	OrderID       string      `json:"order_id"`
	ClientOrderID string      `json:"client_order_id"`
	Symbol        string      `json:"symbol"`
	Side          Side        `json:"side"`
	Type          OrderType   `json:"type"`
	Status        OrderStatus `json:"status"`
	Price         float64     `json:"price"`
	AvgFillPrice  float64     `json:"avg_fill_price"`
	Quantity      float64     `json:"quantity"`
	FilledQty     float64     `json:"filled_qty"`
	FeesUSD       float64     `json:"fees_usd"`
	FeesAsset     string      `json:"fees_asset"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
}

// Balance represents a single asset balance on an exchange.
type Balance struct {
	Asset     string  `json:"asset"`
	Free      float64 `json:"free"`       // available for trading/withdrawal
	Locked    float64 `json:"locked"`     // in open orders
	Total     float64 `json:"total"`      // free + locked
	USDValue  float64 `json:"usd_value"`  // estimated USD value
}

// Position represents an open derivatives position.
type Position struct {
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`           // LONG | SHORT
	Quantity      float64 `json:"quantity"`
	EntryPrice    float64 `json:"entry_price"`
	MarkPrice     float64 `json:"mark_price"`
	UnrealizedPnL float64 `json:"unrealized_pnl"`
	Leverage      float64 `json:"leverage"`
	NotionalUSD   float64 `json:"notional_usd"`
	LiqPrice      float64 `json:"liquidation_price"`
}

// WithdrawRequest specifies a withdrawal from the exchange.
type WithdrawRequest struct {
	Asset   string  `json:"asset"`
	Amount  float64 `json:"amount"`
	Address string  `json:"address"`
	Network string  `json:"network"` // e.g. "arbitrum", "optimism", "ETH"
	Tag     string  `json:"tag,omitempty"` // memo/tag for certain chains
}

// WithdrawResponse is the result of a withdrawal submission.
type WithdrawResponse struct {
	WithdrawID string         `json:"withdraw_id"`
	Asset      string         `json:"asset"`
	Amount     float64        `json:"amount"`
	Fee        float64        `json:"fee"`
	Status     TransferStatus `json:"status"`
	TxID       string         `json:"tx_id,omitempty"`
	Network    string         `json:"network"`
}

// DepositRecord represents a detected deposit on the exchange.
type DepositRecord struct {
	DepositID string         `json:"deposit_id"`
	Asset     string         `json:"asset"`
	Amount    float64        `json:"amount"`
	Status    TransferStatus `json:"status"`
	TxID      string         `json:"tx_id"`
	Network   string         `json:"network"`
	CreatedAt time.Time      `json:"created_at"`
}

// DepositAddress is the address to receive deposits on a specific network.
type DepositAddress struct {
	Asset   string `json:"asset"`
	Address string `json:"address"`
	Network string `json:"network"`
	Tag     string `json:"tag,omitempty"`
}

// ConvertRequest specifies a fiat-to-crypto or crypto-to-crypto conversion.
type ConvertRequest struct {
	FromAsset string  `json:"from_asset"` // e.g. "GBP", "USD"
	ToAsset   string  `json:"to_asset"`   // e.g. "USDC"
	Amount    float64 `json:"amount"`     // amount of FromAsset
}

// ConvertResponse is the result of a conversion.
type ConvertResponse struct {
	ConvertID   string  `json:"convert_id"`
	FromAsset   string  `json:"from_asset"`
	ToAsset     string  `json:"to_asset"`
	FromAmount  float64 `json:"from_amount"`
	ToAmount    float64 `json:"to_amount"`
	Price       float64 `json:"price"`
	FeesUSD     float64 `json:"fees_usd"`
	Status      string  `json:"status"`
}

// NetworkFee describes the withdrawal fee for a specific asset+network combo.
type NetworkFee struct {
	Network    string  `json:"network"`
	Fee        float64 `json:"fee"`
	MinAmount  float64 `json:"min_amount"`
	MaxAmount  float64 `json:"max_amount"`
	EstTimeSec int     `json:"est_time_sec"`
}

// VenueInfo contains metadata about a venue for the treasury allocator.
type VenueInfo struct {
	Name             string   `json:"name"`
	SupportedAssets  []string `json:"supported_assets"`
	SupportedNetworks []string `json:"supported_networks"`
	FeeTakerBps      float64  `json:"fee_taker_bps"`
	FeeMakerBps      float64  `json:"fee_maker_bps"`
}

// TickerPrice is a simple mid-price snapshot from an exchange.
type TickerPrice struct {
	Symbol string  `json:"symbol"`
	Price  float64 `json:"price"`
	TsMs   int64   `json:"ts_ms"`
}
