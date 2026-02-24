// Package deribit implements the Exchange interface for Deribit.
package deribit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

const baseURL = "https://www.deribit.com"

// Client implements exchange.Exchange for Deribit.
type Client struct {
	http      *exchange.HTTPClient
	apiKey    string
	apiSecret string
	token     string // bearer token from auth
	tokenExp  time.Time
}

// New creates a Deribit exchange client.
func New(creds *exchange.Credentials) (exchange.Exchange, error) {
	if creds.APIKey == "" || creds.APISecret == "" {
		return nil, fmt.Errorf("deribit: api_key and api_secret are required")
	}
	return &Client{
		http:      exchange.NewHTTPClient(baseURL, 10*time.Second),
		apiKey:    creds.APIKey,
		apiSecret: creds.APISecret,
	}, nil
}

func (c *Client) Name() string { return "deribit" }

// authenticate obtains or refreshes a bearer token via client_credentials.
func (c *Client) authenticate(ctx context.Context) error {
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return nil
	}
	path := fmt.Sprintf("/api/v2/public/auth?client_id=%s&client_secret=%s&grant_type=client_credentials",
		c.apiKey, c.apiSecret)

	var resp struct {
		Result struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int64  `json:"expires_in"`
		} `json:"result"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return fmt.Errorf("deribit auth: %w", err)
	}
	c.token = resp.Result.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(resp.Result.ExpiresIn-60) * time.Second)
	return nil
}

func (c *Client) authHeaders(ctx context.Context) (map[string]string, error) {
	if err := c.authenticate(ctx); err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + c.token}, nil
}

// deribitResp is the common Deribit JSON-RPC response envelope.
type deribitResp[T any] struct {
	Result T `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// ── Trading ──────────────────────────────────────────────────────────────────

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}

	endpoint := "/api/v2/private/buy"
	if req.Side == exchange.SideSell {
		endpoint = "/api/v2/private/sell"
	}

	params := fmt.Sprintf("instrument_name=%s&amount=%.8f&type=%s&label=%s",
		req.Symbol, req.Quantity, mapDeribitOrderType(req.Type), req.ClientOrderID)
	if req.Type == exchange.OrderTypeLimit || req.Type == exchange.OrderTypeIOC {
		params += fmt.Sprintf("&price=%.2f", req.Price)
		if req.Type == exchange.OrderTypeIOC {
			params += "&time_in_force=immediate_or_cancel"
		}
	}
	if req.ReduceOnly {
		params += "&reduce_only=true"
	}

	path := endpoint + "?" + params

	var resp deribitResp[struct {
		Order struct {
			OrderID       string  `json:"order_id"`
			Label         string  `json:"label"`
			InstrumentName string `json:"instrument_name"`
			Direction     string  `json:"direction"`
			OrderState    string  `json:"order_state"`
			AveragePrice  float64 `json:"average_price"`
			Amount        float64 `json:"amount"`
			FilledAmount  float64 `json:"filled_amount"`
			Commission    float64 `json:"commission"`
		} `json:"order"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, fmt.Errorf("deribit PlaceOrder: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("deribit PlaceOrder: code=%d %s", resp.Error.Code, resp.Error.Message)
	}

	o := resp.Result.Order
	return &exchange.OrderResponse{
		OrderID:       o.OrderID,
		ClientOrderID: o.Label,
		Symbol:        o.InstrumentName,
		Side:          exchange.Side(strings.ToUpper(o.Direction)),
		Status:        mapDeribitStatus(o.OrderState),
		AvgFillPrice:  o.AveragePrice,
		Quantity:      o.Amount,
		FilledQty:     o.FilledAmount,
		FeesUSD:       o.Commission,
		CreatedAt:     time.Now(),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, _, orderID string) error {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return err
	}
	path := "/api/v2/private/cancel?order_id=" + orderID
	var resp deribitResp[json.RawMessage]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("deribit cancel: %s", resp.Error.Message)
	}
	return nil
}

func (c *Client) GetOrder(ctx context.Context, _, orderID string) (*exchange.OrderResponse, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	path := "/api/v2/private/get_order_state?order_id=" + orderID

	var resp deribitResp[struct {
		OrderID        string  `json:"order_id"`
		Label          string  `json:"label"`
		InstrumentName string  `json:"instrument_name"`
		Direction      string  `json:"direction"`
		OrderState     string  `json:"order_state"`
		AveragePrice   float64 `json:"average_price"`
		Amount         float64 `json:"amount"`
		FilledAmount   float64 `json:"filled_amount"`
		Commission     float64 `json:"commission"`
		LastUpdateTs   int64   `json:"last_update_timestamp"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("deribit GetOrder: %s", resp.Error.Message)
	}

	o := resp.Result
	return &exchange.OrderResponse{
		OrderID:       o.OrderID,
		ClientOrderID: o.Label,
		Symbol:        o.InstrumentName,
		Side:          exchange.Side(strings.ToUpper(o.Direction)),
		Status:        mapDeribitStatus(o.OrderState),
		AvgFillPrice:  o.AveragePrice,
		Quantity:      o.Amount,
		FilledQty:     o.FilledAmount,
		FeesUSD:       o.Commission,
		UpdatedAt:     time.UnixMilli(o.LastUpdateTs),
	}, nil
}

// ── Account ──────────────────────────────────────────────────────────────────

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}

	var balances []exchange.Balance
	for _, ccy := range []string{"BTC", "ETH", "USDC", "USDT"} {
		path := "/api/v2/private/get_account_summary?currency=" + ccy
		var resp deribitResp[struct {
			Currency         string  `json:"currency"`
			Balance          float64 `json:"balance"`
			AvailableFunds   float64 `json:"available_funds"`
			Equity           float64 `json:"equity"`
		}]
		if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
			continue
		}
		if resp.Result.Balance == 0 {
			continue
		}
		balances = append(balances, exchange.Balance{
			Asset:  resp.Result.Currency,
			Free:   resp.Result.AvailableFunds,
			Locked: resp.Result.Balance - resp.Result.AvailableFunds,
			Total:  resp.Result.Balance,
		})
	}
	return balances, nil
}

func (c *Client) GetPositions(ctx context.Context) ([]exchange.Position, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}

	var positions []exchange.Position
	for _, ccy := range []string{"BTC", "ETH"} {
		path := "/api/v2/private/get_positions?currency=" + ccy
		var resp deribitResp[[]struct {
			InstrumentName string  `json:"instrument_name"`
			Direction      string  `json:"direction"`
			Size           float64 `json:"size"`
			AveragePrice   float64 `json:"average_price"`
			MarkPrice      float64 `json:"mark_price"`
			FloatingPnL    float64 `json:"floating_profit_loss"`
			Leverage       float64 `json:"leverage"`
			EstLiqPrice    float64 `json:"estimated_liquidation_price"`
		}]
		if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
			continue
		}
		for _, p := range resp.Result {
			if p.Size == 0 {
				continue
			}
			side := "LONG"
			if p.Direction == "sell" {
				side = "SHORT"
			}
			notional := p.Size * p.MarkPrice
			positions = append(positions, exchange.Position{
				Symbol:        p.InstrumentName,
				Side:          side,
				Quantity:      p.Size,
				EntryPrice:    p.AveragePrice,
				MarkPrice:     p.MarkPrice,
				UnrealizedPnL: p.FloatingPnL,
				Leverage:      p.Leverage,
				NotionalUSD:   notional,
				LiqPrice:      p.EstLiqPrice,
			})
		}
	}
	return positions, nil
}

// ── Transfers ────────────────────────────────────────────────────────────────

func (c *Client) Withdraw(ctx context.Context, req exchange.WithdrawRequest) (*exchange.WithdrawResponse, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/v2/private/withdraw?currency=%s&address=%s&amount=%.8f&priority=high",
		req.Asset, req.Address, req.Amount)

	var resp deribitResp[struct {
		ID     int64  `json:"id"`
		State  string `json:"state"`
		Amount float64 `json:"amount"`
		Fee    float64 `json:"fee"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, fmt.Errorf("deribit Withdraw: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("deribit Withdraw: %s", resp.Error.Message)
	}

	return &exchange.WithdrawResponse{
		WithdrawID: strconv.FormatInt(resp.Result.ID, 10),
		Asset:      req.Asset,
		Amount:     resp.Result.Amount,
		Fee:        resp.Result.Fee,
		Status:     exchange.TransferPending,
		Network:    req.Network,
	}, nil
}

func (c *Client) GetWithdrawStatus(ctx context.Context, withdrawID string) (*exchange.WithdrawResponse, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	// Deribit doesn't have a single-withdrawal query; list recent and filter.
	path := "/api/v2/private/get_withdrawals?currency=BTC&count=20"

	var resp deribitResp[[]struct {
		ID     int64   `json:"id"`
		State  string  `json:"state"`
		Amount float64 `json:"amount"`
		Fee    float64 `json:"fee"`
		TxID   string  `json:"transaction_id"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	for _, w := range resp.Result {
		if strconv.FormatInt(w.ID, 10) == withdrawID {
			return &exchange.WithdrawResponse{
				WithdrawID: withdrawID,
				Amount:     w.Amount,
				Fee:        w.Fee,
				Status:     mapDeribitTransferStatus(w.State),
				TxID:       w.TxID,
			}, nil
		}
	}
	return nil, fmt.Errorf("withdrawal %s not found", withdrawID)
}

func (c *Client) GetDepositAddress(ctx context.Context, asset, _ string) (*exchange.DepositAddress, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	path := "/api/v2/private/get_current_deposit_address?currency=" + asset

	var resp deribitResp[struct {
		Address  string `json:"address"`
		Currency string `json:"currency"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	return &exchange.DepositAddress{
		Asset:   asset,
		Address: resp.Result.Address,
	}, nil
}

func (c *Client) GetDeposits(ctx context.Context, asset string, limit int) ([]exchange.DepositRecord, error) {
	headers, err := c.authHeaders(ctx)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/api/v2/private/get_deposits?currency=%s&count=%d", asset, limit)

	var resp deribitResp[[]struct {
		State           string  `json:"state"`
		Amount          float64 `json:"amount"`
		Currency        string  `json:"currency"`
		TransactionID   string  `json:"transaction_id"`
		ReceivedTs      int64   `json:"received_timestamp"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var records []exchange.DepositRecord
	for _, d := range resp.Result {
		records = append(records, exchange.DepositRecord{
			DepositID: d.TransactionID,
			Asset:     d.Currency,
			Amount:    d.Amount,
			Status:    mapDeribitTransferStatus(d.State),
			TxID:      d.TransactionID,
			CreatedAt: time.UnixMilli(d.ReceivedTs),
		})
	}
	return records, nil
}

func (c *Client) GetNetworkFees(_ context.Context, _ string) ([]exchange.NetworkFee, error) {
	// Deribit fees are dynamic; return representative defaults.
	return []exchange.NetworkFee{
		{Network: "BTC", Fee: 0.0001, MinAmount: 0.001},
		{Network: "ETH", Fee: 0.005, MinAmount: 0.01},
	}, nil
}

func (c *Client) GetTickerPrice(ctx context.Context, symbol string) (*exchange.TickerPrice, error) {
	path := "/api/v2/public/ticker?instrument_name=" + symbol
	var resp deribitResp[struct {
		InstrumentName string  `json:"instrument_name"`
		LastPrice      float64 `json:"last_price"`
		Timestamp      int64   `json:"timestamp"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	return &exchange.TickerPrice{
		Symbol: resp.Result.InstrumentName,
		Price:  resp.Result.LastPrice,
		TsMs:   resp.Result.Timestamp,
	}, nil
}

// ── Constraints ──────────────────────────────────────────────────────────────

func (c *Client) GetConstraints(_ context.Context, symbol string) (*exchange.VenueConstraints, error) {
	return &exchange.VenueConstraints{
		Symbol:      symbol,
		TickSize:    0.50,
		LotSize:     0.001,
		MinQty:      0.001,
		MinNotional: 1.0,
		PostOnly:    true,
		ReduceOnly:  true,
	}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func mapDeribitOrderType(t exchange.OrderType) string {
	switch t {
	case exchange.OrderTypeMarket:
		return "market"
	case exchange.OrderTypeLimit, exchange.OrderTypeIOC:
		return "limit"
	default:
		return "market"
	}
}

func mapDeribitStatus(s string) exchange.OrderStatus {
	switch s {
	case "open":
		return exchange.OrderStatusNew
	case "filled":
		return exchange.OrderStatusFilled
	case "cancelled":
		return exchange.OrderStatusCancelled
	case "rejected":
		return exchange.OrderStatusRejected
	default:
		return exchange.OrderStatus(s)
	}
}

func mapDeribitTransferStatus(s string) exchange.TransferStatus {
	switch s {
	case "completed":
		return exchange.TransferCompleted
	case "rejected", "cancelled":
		return exchange.TransferFailed
	default:
		return exchange.TransferPending
	}
}
