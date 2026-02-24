// Package coinbase implements the Exchange interface for Coinbase Advanced Trade API.
// It also implements the Converter and DepositWatcher interfaces for treasury operations.
package coinbase

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

const baseURL = "https://api.coinbase.com"

// Client implements exchange.Exchange, exchange.Converter, and exchange.DepositWatcher.
type Client struct {
	http      *exchange.HTTPClient
	apiKey    string
	apiSecret string
}

// New creates a Coinbase Advanced Trade client.
func New(creds *exchange.Credentials) (exchange.Exchange, error) {
	if creds.APIKey == "" || creds.APISecret == "" {
		return nil, fmt.Errorf("coinbase: api_key and api_secret are required")
	}
	return &Client{
		http:      exchange.NewHTTPClient(baseURL, 15*time.Second),
		apiKey:    creds.APIKey,
		apiSecret: creds.APISecret,
	}, nil
}

func (c *Client) Name() string { return "coinbase" }

// ── Signing ──────────────────────────────────────────────────────────────────

func (c *Client) sign(method, path, body string) map[string]string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	message := ts + method + path + body
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(message))
	sig := hex.EncodeToString(mac.Sum(nil))
	return map[string]string{
		"CB-ACCESS-KEY":       c.apiKey,
		"CB-ACCESS-SIGN":      sig,
		"CB-ACCESS-TIMESTAMP": ts,
	}
}

// ── Trading ──────────────────────────────────────────────────────────────────

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	body := map[string]interface{}{
		"client_order_id": req.ClientOrderID,
		"product_id":      req.Symbol,
		"side":            strings.ToUpper(string(req.Side)),
	}

	switch req.Type {
	case exchange.OrderTypeMarket:
		body["order_configuration"] = map[string]interface{}{
			"market_market_ioc": map[string]interface{}{
				"quote_size": fmt.Sprintf("%.2f", req.NotionalUSD),
			},
		}
	case exchange.OrderTypeLimit, exchange.OrderTypeIOC:
		cfg := map[string]interface{}{
			"base_size":   fmt.Sprintf("%.8f", req.Quantity),
			"limit_price": fmt.Sprintf("%.2f", req.Price),
		}
		if req.Type == exchange.OrderTypeIOC {
			body["order_configuration"] = map[string]interface{}{"sor_limit_ioc": cfg}
		} else {
			cfg["end_time"] = time.Now().Add(5 * time.Minute).Format(time.RFC3339)
			body["order_configuration"] = map[string]interface{}{"limit_limit_gtd": cfg}
		}
	}

	payload, _ := json.Marshal(body)
	path := "/api/v3/brokerage/orders"
	headers := c.sign("POST", path, string(payload))

	var resp struct {
		Success       bool   `json:"success"`
		OrderID       string `json:"order_id"`
		FailureReason string `json:"failure_reason"`
	}
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(string(payload)), headers, &resp); err != nil {
		return nil, fmt.Errorf("coinbase PlaceOrder: %w", err)
	}
	if !resp.Success {
		return nil, fmt.Errorf("coinbase PlaceOrder rejected: %s", resp.FailureReason)
	}

	return &exchange.OrderResponse{
		OrderID:       resp.OrderID,
		ClientOrderID: req.ClientOrderID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Type:          req.Type,
		Status:        exchange.OrderStatusNew,
		CreatedAt:     time.Now(),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, _, orderID string) error {
	body := map[string]interface{}{
		"order_ids": []string{orderID},
	}
	payload, _ := json.Marshal(body)
	path := "/api/v3/brokerage/orders/batch_cancel"
	headers := c.sign("POST", path, string(payload))
	return c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(string(payload)), headers, nil)
}

func (c *Client) GetOrder(ctx context.Context, _, orderID string) (*exchange.OrderResponse, error) {
	path := "/api/v3/brokerage/orders/historical/" + orderID
	headers := c.sign("GET", path, "")

	var resp struct {
		Order struct {
			OrderID       string `json:"order_id"`
			ProductID     string `json:"product_id"`
			Side          string `json:"side"`
			Status        string `json:"status"`
			FilledSize    string `json:"filled_size"`
			AveragePrice  string `json:"average_filled_price"`
			TotalFees     string `json:"total_fees"`
			CreatedTime   string `json:"created_time"`
		} `json:"order"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	filledQty, _ := strconv.ParseFloat(resp.Order.FilledSize, 64)
	avgPrice, _ := strconv.ParseFloat(resp.Order.AveragePrice, 64)
	fees, _ := strconv.ParseFloat(resp.Order.TotalFees, 64)

	return &exchange.OrderResponse{
		OrderID:      resp.Order.OrderID,
		Symbol:       resp.Order.ProductID,
		Side:         exchange.Side(resp.Order.Side),
		Status:       mapCoinbaseStatus(resp.Order.Status),
		AvgFillPrice: avgPrice,
		FilledQty:    filledQty,
		FeesUSD:      fees,
	}, nil
}

// ── Account ──────────────────────────────────────────────────────────────────

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	path := "/api/v3/brokerage/accounts?limit=250"
	headers := c.sign("GET", path, "")

	var resp struct {
		Accounts []struct {
			Currency string `json:"currency"`
			Available struct{ Value string `json:"value"` } `json:"available_balance"`
			Hold      struct{ Value string `json:"value"` } `json:"hold"`
		} `json:"accounts"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var balances []exchange.Balance
	for _, a := range resp.Accounts {
		free, _ := strconv.ParseFloat(a.Available.Value, 64)
		locked, _ := strconv.ParseFloat(a.Hold.Value, 64)
		total := free + locked
		if total == 0 {
			continue
		}
		balances = append(balances, exchange.Balance{
			Asset:  a.Currency,
			Free:   free,
			Locked: locked,
			Total:  total,
		})
	}
	return balances, nil
}

func (c *Client) GetPositions(_ context.Context) ([]exchange.Position, error) {
	// Coinbase Advanced Trade does not support perpetual futures positions.
	return nil, nil
}

// ── Transfers ────────────────────────────────────────────────────────────────

func (c *Client) Withdraw(ctx context.Context, req exchange.WithdrawRequest) (*exchange.WithdrawResponse, error) {
	// Coinbase uses the Send Money endpoint for crypto withdrawals.
	// First we need to find the account UUID for this asset.
	balances, err := c.GetBalances(ctx)
	if err != nil {
		return nil, fmt.Errorf("coinbase Withdraw: listing accounts: %w", err)
	}
	_ = balances // account lookup would happen here in production

	// Coinbase Advanced Trade crypto send
	body := map[string]interface{}{
		"amount":   fmt.Sprintf("%.8f", req.Amount),
		"currency": req.Asset,
		"to":       req.Address,
		"network":  req.Network,
	}
	payload, _ := json.Marshal(body)
	path := "/v2/accounts/" + req.Asset + "/transactions"
	headers := c.sign("POST", path, string(payload))
	headers["CB-VERSION"] = "2024-01-01"

	var resp struct {
		Data struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Amount struct{ Amount string `json:"amount"` } `json:"amount"`
			Network struct{ Hash string `json:"hash"` } `json:"network"`
		} `json:"data"`
	}
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(string(payload)), headers, &resp); err != nil {
		return nil, fmt.Errorf("coinbase Withdraw: %w", err)
	}

	amt, _ := strconv.ParseFloat(resp.Data.Amount.Amount, 64)
	return &exchange.WithdrawResponse{
		WithdrawID: resp.Data.ID,
		Asset:      req.Asset,
		Amount:     amt,
		Status:     exchange.TransferPending,
		Network:    req.Network,
	}, nil
}

func (c *Client) GetWithdrawStatus(ctx context.Context, withdrawID string) (*exchange.WithdrawResponse, error) {
	// Would query /v2/accounts/:account_id/transactions/:tx_id
	return &exchange.WithdrawResponse{
		WithdrawID: withdrawID,
		Status:     exchange.TransferPending,
	}, nil
}

func (c *Client) GetDepositAddress(ctx context.Context, asset, network string) (*exchange.DepositAddress, error) {
	path := "/v2/accounts/" + asset + "/addresses"
	headers := c.sign("POST", path, "{}")
	headers["CB-VERSION"] = "2024-01-01"

	var resp struct {
		Data struct {
			Address string `json:"address"`
			Network string `json:"network"`
		} `json:"data"`
	}
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader("{}"), headers, &resp); err != nil {
		return nil, err
	}
	return &exchange.DepositAddress{
		Asset:   asset,
		Address: resp.Data.Address,
		Network: resp.Data.Network,
	}, nil
}

func (c *Client) GetDeposits(ctx context.Context, asset string, limit int) ([]exchange.DepositRecord, error) {
	path := fmt.Sprintf("/v2/accounts/%s/transactions?limit=%d&type=deposit", asset, limit)
	headers := c.sign("GET", path, "")
	headers["CB-VERSION"] = "2024-01-01"

	var resp struct {
		Data []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Amount    struct{ Amount string `json:"amount"`; Currency string `json:"currency"` } `json:"amount"`
			CreatedAt string `json:"created_at"`
			Network   struct{ Hash string `json:"hash"`; Name string `json:"name"` } `json:"network"`
		} `json:"data"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var records []exchange.DepositRecord
	for _, d := range resp.Data {
		amt, _ := strconv.ParseFloat(d.Amount.Amount, 64)
		ts, _ := time.Parse(time.RFC3339, d.CreatedAt)
		records = append(records, exchange.DepositRecord{
			DepositID: d.ID,
			Asset:     d.Amount.Currency,
			Amount:    amt,
			Status:    mapCoinbaseTransferStatus(d.Status),
			TxID:      d.Network.Hash,
			Network:   d.Network.Name,
			CreatedAt: ts,
		})
	}
	return records, nil
}

func (c *Client) GetNetworkFees(_ context.Context, _ string) ([]exchange.NetworkFee, error) {
	// Coinbase absorbs network fees for most transfers.
	return []exchange.NetworkFee{
		{Network: "ethereum", Fee: 0, MinAmount: 0.001},
		{Network: "arbitrum", Fee: 0, MinAmount: 0.001},
		{Network: "base", Fee: 0, MinAmount: 0.001},
	}, nil
}

func (c *Client) GetTickerPrice(ctx context.Context, symbol string) (*exchange.TickerPrice, error) {
	path := "/api/v3/brokerage/products/" + symbol
	headers := c.sign("GET", path, "")

	var resp struct {
		Price string `json:"price"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	price, _ := strconv.ParseFloat(resp.Price, 64)
	return &exchange.TickerPrice{
		Symbol: symbol,
		Price:  price,
		TsMs:   time.Now().UnixMilli(),
	}, nil
}

// ── Converter interface ──────────────────────────────────────────────────────

func (c *Client) Convert(ctx context.Context, req exchange.ConvertRequest) (*exchange.ConvertResponse, error) {
	body := map[string]interface{}{
		"from_account": req.FromAsset,
		"to_account":   req.ToAsset,
		"amount":       fmt.Sprintf("%.2f", req.Amount),
	}
	payload, _ := json.Marshal(body)
	path := "/api/v3/brokerage/convert/trade"
	headers := c.sign("POST", path, string(payload))

	var resp struct {
		Trade struct {
			ID            string `json:"id"`
			Status        string `json:"status"`
			SourceAmount  string `json:"source_amount"`
			TargetAmount  string `json:"target_amount"`
			ExchangeRate  string `json:"exchange_rate"`
			TotalFees     string `json:"total_fee"`
		} `json:"trade"`
	}
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(string(payload)), headers, &resp); err != nil {
		return nil, fmt.Errorf("coinbase Convert: %w", err)
	}

	fromAmt, _ := strconv.ParseFloat(resp.Trade.SourceAmount, 64)
	toAmt, _ := strconv.ParseFloat(resp.Trade.TargetAmount, 64)
	rate, _ := strconv.ParseFloat(resp.Trade.ExchangeRate, 64)
	fees, _ := strconv.ParseFloat(resp.Trade.TotalFees, 64)

	return &exchange.ConvertResponse{
		ConvertID:  resp.Trade.ID,
		FromAsset:  req.FromAsset,
		ToAsset:    req.ToAsset,
		FromAmount: fromAmt,
		ToAmount:   toAmt,
		Price:      rate,
		FeesUSD:    fees,
		Status:     resp.Trade.Status,
	}, nil
}

func (c *Client) GetConvertStatus(ctx context.Context, convertID string) (*exchange.ConvertResponse, error) {
	path := "/api/v3/brokerage/convert/trade/" + convertID
	headers := c.sign("GET", path, "")

	var resp struct {
		Trade struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			TargetAmount string `json:"target_amount"`
		} `json:"trade"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	toAmt, _ := strconv.ParseFloat(resp.Trade.TargetAmount, 64)
	return &exchange.ConvertResponse{
		ConvertID: resp.Trade.ID,
		ToAmount:  toAmt,
		Status:    resp.Trade.Status,
	}, nil
}

// ── DepositWatcher interface ─────────────────────────────────────────────────

func (c *Client) GetFiatDeposits(ctx context.Context, currency string, limit int) ([]exchange.DepositRecord, error) {
	path := fmt.Sprintf("/v2/accounts/%s/deposits?limit=%d", currency, limit)
	headers := c.sign("GET", path, "")
	headers["CB-VERSION"] = "2024-01-01"

	var resp struct {
		Data []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			Amount    struct{ Amount string `json:"amount"`; Currency string `json:"currency"` } `json:"amount"`
			CreatedAt string `json:"created_at"`
		} `json:"data"`
	}
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var records []exchange.DepositRecord
	for _, d := range resp.Data {
		amt, _ := strconv.ParseFloat(d.Amount.Amount, 64)
		ts, _ := time.Parse(time.RFC3339, d.CreatedAt)
		records = append(records, exchange.DepositRecord{
			DepositID: d.ID,
			Asset:     d.Amount.Currency,
			Amount:    amt,
			Status:    mapCoinbaseTransferStatus(d.Status),
			CreatedAt: ts,
		})
	}
	return records, nil
}

// ── Constraints ──────────────────────────────────────────────────────────────

func (c *Client) GetConstraints(_ context.Context, symbol string) (*exchange.VenueConstraints, error) {
	return &exchange.VenueConstraints{
		Symbol:      symbol,
		TickSize:    0.01,
		LotSize:     0.00000001,
		MinQty:      0.00000001,
		MinNotional: 1.0,
		PostOnly:    true,
		ReduceOnly:  false,
	}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func mapCoinbaseStatus(s string) exchange.OrderStatus {
	switch s {
	case "OPEN":
		return exchange.OrderStatusNew
	case "FILLED":
		return exchange.OrderStatusFilled
	case "CANCELLED":
		return exchange.OrderStatusCancelled
	case "EXPIRED":
		return exchange.OrderStatusExpired
	case "FAILED":
		return exchange.OrderStatusRejected
	default:
		return exchange.OrderStatus(s)
	}
}

func mapCoinbaseTransferStatus(s string) exchange.TransferStatus {
	switch s {
	case "completed":
		return exchange.TransferCompleted
	case "pending", "waiting_for_clearing":
		return exchange.TransferPending
	case "failed", "canceled":
		return exchange.TransferFailed
	default:
		return exchange.TransferPending
	}
}
