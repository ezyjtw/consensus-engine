package exchange

import "context"

// Exchange is the unified interface every venue adapter must implement.
// Methods are grouped by capability: trading, account, transfers, and conversion.
type Exchange interface {
	// Name returns the venue identifier (e.g. "binance", "coinbase").
	Name() string

	// ── Trading ───────────────────────────────────────────────────────────

	// PlaceOrder submits an order and returns the initial response.
	PlaceOrder(ctx context.Context, req OrderRequest) (*OrderResponse, error)

	// CancelOrder cancels an open order by its exchange order ID.
	CancelOrder(ctx context.Context, symbol, orderID string) error

	// GetOrder retrieves the current state of an order.
	GetOrder(ctx context.Context, symbol, orderID string) (*OrderResponse, error)

	// ── Account ───────────────────────────────────────────────────────────

	// GetBalances returns all non-zero asset balances.
	GetBalances(ctx context.Context) ([]Balance, error)

	// GetPositions returns all open derivatives positions.
	GetPositions(ctx context.Context) ([]Position, error)

	// ── Transfers ─────────────────────────────────────────────────────────

	// Withdraw submits a withdrawal request.
	Withdraw(ctx context.Context, req WithdrawRequest) (*WithdrawResponse, error)

	// GetWithdrawStatus checks the status of a withdrawal.
	GetWithdrawStatus(ctx context.Context, withdrawID string) (*WithdrawResponse, error)

	// GetDepositAddress returns the deposit address for an asset+network.
	GetDepositAddress(ctx context.Context, asset, network string) (*DepositAddress, error)

	// GetDeposits returns recent deposits, optionally filtered by asset.
	GetDeposits(ctx context.Context, asset string, limit int) ([]DepositRecord, error)

	// GetNetworkFees returns withdrawal fees for an asset across networks.
	GetNetworkFees(ctx context.Context, asset string) ([]NetworkFee, error)

	// ── Pricing ───────────────────────────────────────────────────────────

	// GetTickerPrice returns the current price for a symbol.
	GetTickerPrice(ctx context.Context, symbol string) (*TickerPrice, error)

	// ── Constraints ──────────────────────────────────────────────────────

	// GetConstraints returns venue-specific trading rules for a symbol.
	// Implementations may return sensible defaults if the exchange does not
	// expose this information via API.
	GetConstraints(ctx context.Context, symbol string) (*VenueConstraints, error)
}

// Converter is an optional interface for exchanges that support fiat-to-crypto
// or crypto-to-crypto conversion (e.g. Coinbase).
type Converter interface {
	// Convert executes a fiat→crypto or crypto→crypto conversion.
	Convert(ctx context.Context, req ConvertRequest) (*ConvertResponse, error)

	// GetConvertStatus checks the status of a conversion.
	GetConvertStatus(ctx context.Context, convertID string) (*ConvertResponse, error)
}

// Amender is an optional interface for exchanges that support in-place order
// amendment (modify price/qty without cancel/replace). Binance, OKX, and Bybit
// support this; Coinbase and Deribit do not.
type Amender interface {
	// AmendOrder modifies an existing open order in-place.
	AmendOrder(ctx context.Context, req AmendOrderRequest) (*OrderResponse, error)
}

// ADLDetector is an optional interface for exchanges that expose auto-deleverage
// event information. When a position is forcibly reduced by the exchange's ADL
// mechanism, the adapter should detect this and report it.
type ADLDetector interface {
	// DetectADLEvents returns any ADL (auto-deleverage) events since the given
	// timestamp. Each event includes the affected symbol, reduced quantity, and
	// the price at which the reduction occurred.
	DetectADLEvents(ctx context.Context, sinceMs int64) ([]ADLEvent, error)
}

// DepositWatcher is an optional interface for exchanges that support polling
// for new fiat deposits (e.g. Coinbase).
type DepositWatcher interface {
	// GetFiatDeposits returns recent fiat deposits (bank transfers, card, etc).
	GetFiatDeposits(ctx context.Context, currency string, limit int) ([]DepositRecord, error)
}
