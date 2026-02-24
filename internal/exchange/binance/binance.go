// Package binance implements the Exchange interface for Binance Futures (USD-M).
package binance

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

const (
	futuresBaseURL = "https://fapi.binance.com"
	spotBaseURL    = "https://api.binance.com"
)

// Client implements exchange.Exchange for Binance USD-M Futures.
type Client struct {
	futures   *exchange.HTTPClient
	spot      *exchange.HTTPClient
	apiKey    string
	apiSecret string
}

// New creates a Binance exchange client.
func New(creds *exchange.Credentials) (exchange.Exchange, error) {
	if creds.APIKey == "" || creds.APISecret == "" {
		return nil, fmt.Errorf("binance: api_key and api_secret are required")
	}
	return &Client{
		futures:   exchange.NewHTTPClient(futuresBaseURL, 10*time.Second),
		spot:      exchange.NewHTTPClient(spotBaseURL, 10*time.Second),
		apiKey:    creds.APIKey,
		apiSecret: creds.APISecret,
	}, nil
}

func (c *Client) Name() string { return "binance" }

func (c *Client) signQuery(params url.Values) string {
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	params.Set("recvWindow", "5000")
	raw := params.Encode()
	sig := exchange.HMACSha256(c.apiSecret, raw)
	return raw + "&signature=" + sig
}

func (c *Client) authHeaders() map[string]string {
	return map[string]string{"X-MBX-APIKEY": c.apiKey}
}

// ── Trading ──────────────────────────────────────────────────────────────────

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	params := url.Values{
		"symbol":           {req.Symbol},
		"side":             {string(req.Side)},
		"type":             {mapOrderType(req.Type)},
		"quantity":         {fmt.Sprintf("%.8f", req.Quantity)},
		"newClientOrderId": {req.ClientOrderID},
	}
	if req.Type == exchange.OrderTypeLimit || req.Type == exchange.OrderTypeIOC {
		params.Set("price", fmt.Sprintf("%.2f", req.Price))
		if req.Type == exchange.OrderTypeIOC {
			params.Set("timeInForce", "IOC")
		} else {
			params.Set("timeInForce", "GTC")
		}
	}
	if req.ReduceOnly {
		params.Set("reduceOnly", "true")
	}

	query := c.signQuery(params)
	path := "/fapi/v1/order?" + query
	headers := c.authHeaders()

	var resp struct {
		OrderID       int64  `json:"orderId"`
		ClientOrderID string `json:"clientOrderId"`
		Symbol        string `json:"symbol"`
		Side          string `json:"side"`
		Status        string `json:"status"`
		Price         string `json:"price"`
		AvgPrice      string `json:"avgPrice"`
		OrigQty       string `json:"origQty"`
		ExecutedQty   string `json:"executedQty"`
		UpdateTime    int64  `json:"updateTime"`
	}
	if err := c.futures.DoJSON(ctx, http.MethodPost, path, nil, headers, &resp); err != nil {
		return nil, fmt.Errorf("binance PlaceOrder: %w", err)
	}

	filledQty, _ := strconv.ParseFloat(resp.ExecutedQty, 64)
	avgPrice, _ := strconv.ParseFloat(resp.AvgPrice, 64)
	origQty, _ := strconv.ParseFloat(resp.OrigQty, 64)

	return &exchange.OrderResponse{
		OrderID:       strconv.FormatInt(resp.OrderID, 10),
		ClientOrderID: resp.ClientOrderID,
		Symbol:        resp.Symbol,
		Side:          exchange.Side(resp.Side),
		Status:        mapBinanceStatus(resp.Status),
		AvgFillPrice:  avgPrice,
		Quantity:      origQty,
		FilledQty:     filledQty,
		CreatedAt:     time.UnixMilli(resp.UpdateTime),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	params := url.Values{
		"symbol":  {symbol},
		"orderId": {orderID},
	}
	query := c.signQuery(params)
	path := "/fapi/v1/order?" + query
	return c.futures.DoJSON(ctx, http.MethodDelete, path, nil, c.authHeaders(), nil)
}

func (c *Client) GetOrder(ctx context.Context, symbol, orderID string) (*exchange.OrderResponse, error) {
	params := url.Values{
		"symbol":  {symbol},
		"orderId": {orderID},
	}
	query := c.signQuery(params)
	path := "/fapi/v1/order?" + query

	var resp struct {
		OrderID     int64  `json:"orderId"`
		Symbol      string `json:"symbol"`
		Side        string `json:"side"`
		Status      string `json:"status"`
		AvgPrice    string `json:"avgPrice"`
		OrigQty     string `json:"origQty"`
		ExecutedQty string `json:"executedQty"`
		UpdateTime  int64  `json:"updateTime"`
	}
	if err := c.futures.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}

	filledQty, _ := strconv.ParseFloat(resp.ExecutedQty, 64)
	avgPrice, _ := strconv.ParseFloat(resp.AvgPrice, 64)
	origQty, _ := strconv.ParseFloat(resp.OrigQty, 64)

	return &exchange.OrderResponse{
		OrderID:      strconv.FormatInt(resp.OrderID, 10),
		Symbol:       resp.Symbol,
		Side:         exchange.Side(resp.Side),
		Status:       mapBinanceStatus(resp.Status),
		AvgFillPrice: avgPrice,
		Quantity:     origQty,
		FilledQty:    filledQty,
		UpdatedAt:    time.UnixMilli(resp.UpdateTime),
	}, nil
}

// ── Account ──────────────────────────────────────────────────────────────────

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	params := url.Values{}
	query := c.signQuery(params)
	path := "/fapi/v2/balance?" + query

	var resp []struct {
		Asset            string `json:"asset"`
		Balance          string `json:"balance"`
		AvailableBalance string `json:"availableBalance"`
	}
	if err := c.futures.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}

	var balances []exchange.Balance
	for _, b := range resp {
		total, _ := strconv.ParseFloat(b.Balance, 64)
		free, _ := strconv.ParseFloat(b.AvailableBalance, 64)
		if total == 0 {
			continue
		}
		balances = append(balances, exchange.Balance{
			Asset:  b.Asset,
			Free:   free,
			Locked: total - free,
			Total:  total,
		})
	}
	return balances, nil
}

func (c *Client) GetPositions(ctx context.Context) ([]exchange.Position, error) {
	params := url.Values{}
	query := c.signQuery(params)
	path := "/fapi/v2/positionRisk?" + query

	var resp []struct {
		Symbol           string `json:"symbol"`
		PositionAmt      string `json:"positionAmt"`
		EntryPrice       string `json:"entryPrice"`
		MarkPrice        string `json:"markPrice"`
		UnRealizedProfit string `json:"unRealizedProfit"`
		Leverage         string `json:"leverage"`
		Notional         string `json:"notional"`
		LiquidationPrice string `json:"liquidationPrice"`
	}
	if err := c.futures.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}

	var positions []exchange.Position
	for _, p := range resp {
		qty, _ := strconv.ParseFloat(p.PositionAmt, 64)
		if qty == 0 {
			continue
		}
		entry, _ := strconv.ParseFloat(p.EntryPrice, 64)
		mark, _ := strconv.ParseFloat(p.MarkPrice, 64)
		pnl, _ := strconv.ParseFloat(p.UnRealizedProfit, 64)
		lev, _ := strconv.ParseFloat(p.Leverage, 64)
		notional, _ := strconv.ParseFloat(p.Notional, 64)
		liq, _ := strconv.ParseFloat(p.LiquidationPrice, 64)

		side := "LONG"
		if qty < 0 {
			side = "SHORT"
			qty = -qty
		}

		positions = append(positions, exchange.Position{
			Symbol:        p.Symbol,
			Side:          side,
			Quantity:      qty,
			EntryPrice:    entry,
			MarkPrice:     mark,
			UnrealizedPnL: pnl,
			Leverage:      lev,
			NotionalUSD:   notional,
			LiqPrice:      liq,
		})
	}
	return positions, nil
}

// ── Transfers ────────────────────────────────────────────────────────────────

func (c *Client) Withdraw(ctx context.Context, req exchange.WithdrawRequest) (*exchange.WithdrawResponse, error) {
	params := url.Values{
		"coin":    {req.Asset},
		"address": {req.Address},
		"amount":  {fmt.Sprintf("%.8f", req.Amount)},
		"network": {strings.ToUpper(req.Network)},
	}
	query := c.signQuery(params)
	path := "/sapi/v1/capital/withdraw/apply?" + query

	var resp struct {
		ID string `json:"id"`
	}
	if err := c.spot.DoJSON(ctx, http.MethodPost, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, fmt.Errorf("binance Withdraw: %w", err)
	}
	return &exchange.WithdrawResponse{
		WithdrawID: resp.ID,
		Asset:      req.Asset,
		Amount:     req.Amount,
		Status:     exchange.TransferPending,
		Network:    req.Network,
	}, nil
}

func (c *Client) GetWithdrawStatus(ctx context.Context, withdrawID string) (*exchange.WithdrawResponse, error) {
	params := url.Values{"id": {withdrawID}}
	query := c.signQuery(params)
	path := "/sapi/v1/capital/withdraw/history?" + query

	var resp []struct {
		ID        string  `json:"id"`
		Amount    string  `json:"amount"`
		Asset     string  `json:"coin"`
		Network   string  `json:"network"`
		Status    int     `json:"status"`
		TxID      string  `json:"txId"`
		Fee       string  `json:"transactionFee"`
	}
	if err := c.spot.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}
	if len(resp) == 0 {
		return nil, fmt.Errorf("withdrawal %s not found", withdrawID)
	}

	w := resp[0]
	amt, _ := strconv.ParseFloat(w.Amount, 64)
	fee, _ := strconv.ParseFloat(w.Fee, 64)
	return &exchange.WithdrawResponse{
		WithdrawID: w.ID,
		Asset:      w.Asset,
		Amount:     amt,
		Fee:        fee,
		Status:     mapBinanceWithdrawStatus(w.Status),
		TxID:       w.TxID,
		Network:    w.Network,
	}, nil
}

func (c *Client) GetDepositAddress(ctx context.Context, asset, network string) (*exchange.DepositAddress, error) {
	params := url.Values{
		"coin":    {asset},
		"network": {strings.ToUpper(network)},
	}
	query := c.signQuery(params)
	path := "/sapi/v1/capital/deposit/address?" + query

	var resp struct {
		Address string `json:"address"`
		Tag     string `json:"tag"`
	}
	if err := c.spot.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}
	return &exchange.DepositAddress{
		Asset:   asset,
		Address: resp.Address,
		Network: network,
		Tag:     resp.Tag,
	}, nil
}

func (c *Client) GetDeposits(ctx context.Context, asset string, limit int) ([]exchange.DepositRecord, error) {
	params := url.Values{"limit": {strconv.Itoa(limit)}}
	if asset != "" {
		params.Set("coin", asset)
	}
	query := c.signQuery(params)
	path := "/sapi/v1/capital/deposit/hisrec?" + query

	var resp []struct {
		ID        string `json:"id"`
		Amount    string `json:"amount"`
		Coin      string `json:"coin"`
		Network   string `json:"network"`
		Status    int    `json:"status"`
		TxID      string `json:"txId"`
		InsertTime int64 `json:"insertTime"`
	}
	if err := c.spot.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}

	var records []exchange.DepositRecord
	for _, d := range resp {
		amt, _ := strconv.ParseFloat(d.Amount, 64)
		status := exchange.TransferPending
		if d.Status == 1 {
			status = exchange.TransferCompleted
		}
		records = append(records, exchange.DepositRecord{
			DepositID: d.ID,
			Asset:     d.Coin,
			Amount:    amt,
			Status:    status,
			TxID:      d.TxID,
			Network:   d.Network,
			CreatedAt: time.UnixMilli(d.InsertTime),
		})
	}
	return records, nil
}

func (c *Client) GetNetworkFees(ctx context.Context, asset string) ([]exchange.NetworkFee, error) {
	params := url.Values{}
	query := c.signQuery(params)
	path := "/sapi/v1/capital/config/getall?" + query

	var resp []struct {
		Coin     string `json:"coin"`
		Networks []struct {
			Network      string `json:"network"`
			WithdrawFee  string `json:"withdrawFee"`
			WithdrawMin  string `json:"withdrawMin"`
			WithdrawMax  string `json:"withdrawMax"`
			EstimatedArr int    `json:"estimatedArrivalTime"` // minutes
		} `json:"networkList"`
	}
	if err := c.spot.DoJSON(ctx, http.MethodGet, path, nil, c.authHeaders(), &resp); err != nil {
		return nil, err
	}

	var fees []exchange.NetworkFee
	for _, coin := range resp {
		if !strings.EqualFold(coin.Coin, asset) {
			continue
		}
		for _, n := range coin.Networks {
			fee, _ := strconv.ParseFloat(n.WithdrawFee, 64)
			min, _ := strconv.ParseFloat(n.WithdrawMin, 64)
			max, _ := strconv.ParseFloat(n.WithdrawMax, 64)
			fees = append(fees, exchange.NetworkFee{
				Network:    n.Network,
				Fee:        fee,
				MinAmount:  min,
				MaxAmount:  max,
				EstTimeSec: n.EstimatedArr * 60,
			})
		}
	}
	return fees, nil
}

func (c *Client) GetTickerPrice(ctx context.Context, symbol string) (*exchange.TickerPrice, error) {
	path := "/fapi/v1/ticker/price?symbol=" + symbol
	var resp struct {
		Symbol string `json:"symbol"`
		Price  string `json:"price"`
		Time   int64  `json:"time"`
	}
	if err := c.futures.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	price, _ := strconv.ParseFloat(resp.Price, 64)
	return &exchange.TickerPrice{
		Symbol: resp.Symbol,
		Price:  price,
		TsMs:   resp.Time,
	}, nil
}

// ── Constraints ──────────────────────────────────────────────────────────────

func (c *Client) GetConstraints(_ context.Context, symbol string) (*exchange.VenueConstraints, error) {
	return &exchange.VenueConstraints{
		Symbol:      symbol,
		TickSize:    0.10,
		LotSize:     0.001,
		MinQty:      0.001,
		MinNotional: 5.0,
		PostOnly:    true,
		ReduceOnly:  true,
	}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func mapOrderType(t exchange.OrderType) string {
	switch t {
	case exchange.OrderTypeMarket:
		return "MARKET"
	case exchange.OrderTypeLimit:
		return "LIMIT"
	case exchange.OrderTypeIOC:
		return "LIMIT" // with timeInForce=IOC
	default:
		return "MARKET"
	}
}

func mapBinanceStatus(s string) exchange.OrderStatus {
	switch s {
	case "NEW":
		return exchange.OrderStatusNew
	case "PARTIALLY_FILLED":
		return exchange.OrderStatusPartiallyFilled
	case "FILLED":
		return exchange.OrderStatusFilled
	case "CANCELED":
		return exchange.OrderStatusCancelled
	case "REJECTED":
		return exchange.OrderStatusRejected
	case "EXPIRED":
		return exchange.OrderStatusExpired
	default:
		return exchange.OrderStatus(s)
	}
}

func mapBinanceWithdrawStatus(status int) exchange.TransferStatus {
	switch status {
	case 6: // completed
		return exchange.TransferCompleted
	case 3, 5: // rejected, failure
		return exchange.TransferFailed
	default:
		return exchange.TransferPending
	}
}
