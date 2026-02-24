// Package bybit implements the Exchange interface for Bybit V5 API.
package bybit

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

const baseURL = "https://api.bybit.com"

// Client implements exchange.Exchange for Bybit V5.
type Client struct {
	http      *exchange.HTTPClient
	apiKey    string
	apiSecret string
}

// New creates a Bybit exchange client.
func New(creds *exchange.Credentials) (exchange.Exchange, error) {
	if creds.APIKey == "" || creds.APISecret == "" {
		return nil, fmt.Errorf("bybit: api_key and api_secret are required")
	}
	return &Client{
		http:      exchange.NewHTTPClient(baseURL, 10*time.Second),
		apiKey:    creds.APIKey,
		apiSecret: creds.APISecret,
	}, nil
}

func (c *Client) Name() string { return "bybit" }

func (c *Client) sign(params string) map[string]string {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	recvWindow := "5000"
	preSign := ts + c.apiKey + recvWindow + params
	sig := exchange.HMACSha256(c.apiSecret, preSign)
	return map[string]string{
		"X-BAPI-API-KEY":     c.apiKey,
		"X-BAPI-SIGN":        sig,
		"X-BAPI-TIMESTAMP":   ts,
		"X-BAPI-RECV-WINDOW": recvWindow,
	}
}

// bybitResp is the common Bybit V5 envelope.
type bybitResp[T any] struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  T      `json:"result"`
}

func checkCode(code int, msg string) error {
	if code != 0 {
		return fmt.Errorf("bybit API error code=%d: %s", code, msg)
	}
	return nil
}

// ── Trading ──────────────────────────────────────────────────────────────────

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	body := fmt.Sprintf(`{"category":"linear","symbol":"%s","side":"%s","orderType":"%s","qty":"%s","orderLinkId":"%s"`,
		req.Symbol, capitalize(string(req.Side)), mapBybitOrderType(req.Type),
		fmt.Sprintf("%.8f", req.Quantity), req.ClientOrderID)
	if req.Type == exchange.OrderTypeLimit || req.Type == exchange.OrderTypeIOC {
		body += fmt.Sprintf(`,"price":"%s"`, fmt.Sprintf("%.2f", req.Price))
		if req.Type == exchange.OrderTypeIOC {
			body += `,"timeInForce":"IOC"`
		} else {
			body += `,"timeInForce":"GTC"`
		}
	}
	if req.ReduceOnly {
		body += `,"reduceOnly":true`
	}
	body += "}"

	path := "/v5/order/create"
	headers := c.sign(body)

	var resp bybitResp[struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(body), headers, &resp); err != nil {
		return nil, fmt.Errorf("bybit PlaceOrder: %w", err)
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}

	return &exchange.OrderResponse{
		OrderID:       resp.Result.OrderID,
		ClientOrderID: resp.Result.OrderLinkID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Type:          req.Type,
		Status:        exchange.OrderStatusNew,
		CreatedAt:     time.Now(),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	body := fmt.Sprintf(`{"category":"linear","symbol":"%s","orderId":"%s"}`, symbol, orderID)
	path := "/v5/order/cancel"
	headers := c.sign(body)
	var resp bybitResp[struct{}]
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(body), headers, &resp); err != nil {
		return err
	}
	return checkCode(resp.RetCode, resp.RetMsg)
}

func (c *Client) GetOrder(ctx context.Context, symbol, orderID string) (*exchange.OrderResponse, error) {
	query := fmt.Sprintf("category=linear&symbol=%s&orderId=%s", symbol, orderID)
	path := "/v5/order/realtime?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
			Symbol      string `json:"symbol"`
			Side        string `json:"side"`
			Status      string `json:"orderStatus"`
			AvgPrice    string `json:"avgPrice"`
			Qty         string `json:"qty"`
			CumExecQty  string `json:"cumExecQty"`
			CumExecFee  string `json:"cumExecFee"`
			UpdatedTime string `json:"updatedTime"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}
	if len(resp.Result.List) == 0 {
		return nil, fmt.Errorf("order %s not found", orderID)
	}

	o := resp.Result.List[0]
	avgPx, _ := strconv.ParseFloat(o.AvgPrice, 64)
	qty, _ := strconv.ParseFloat(o.Qty, 64)
	filledQty, _ := strconv.ParseFloat(o.CumExecQty, 64)
	fees, _ := strconv.ParseFloat(o.CumExecFee, 64)
	ut, _ := strconv.ParseInt(o.UpdatedTime, 10, 64)

	return &exchange.OrderResponse{
		OrderID:       o.OrderID,
		ClientOrderID: o.OrderLinkID,
		Symbol:        o.Symbol,
		Side:          exchange.Side(o.Side),
		Status:        mapBybitStatus(o.Status),
		AvgFillPrice:  avgPx,
		Quantity:      qty,
		FilledQty:     filledQty,
		FeesUSD:       fees,
		UpdatedAt:     time.UnixMilli(ut),
	}, nil
}

// ── Account ──────────────────────────────────────────────────────────────────

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	query := "accountType=UNIFIED"
	path := "/v5/account/wallet-balance?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			Coin []struct {
				Coin         string `json:"coin"`
				WalletBalance string `json:"walletBalance"`
				AvailableToWithdraw string `json:"availableToWithdraw"`
				Locked       string `json:"locked"`
			} `json:"coin"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}

	var balances []exchange.Balance
	if len(resp.Result.List) > 0 {
		for _, coin := range resp.Result.List[0].Coin {
			total, _ := strconv.ParseFloat(coin.WalletBalance, 64)
			free, _ := strconv.ParseFloat(coin.AvailableToWithdraw, 64)
			locked, _ := strconv.ParseFloat(coin.Locked, 64)
			if total == 0 {
				continue
			}
			balances = append(balances, exchange.Balance{
				Asset:  coin.Coin,
				Free:   free,
				Locked: locked,
				Total:  total,
			})
		}
	}
	return balances, nil
}

func (c *Client) GetPositions(ctx context.Context) ([]exchange.Position, error) {
	query := "category=linear&settleCoin=USDT"
	path := "/v5/position/list?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		List []struct {
			Symbol        string `json:"symbol"`
			Side          string `json:"side"`
			Size          string `json:"size"`
			AvgPrice      string `json:"avgPrice"`
			MarkPrice     string `json:"markPrice"`
			UnrealisedPnl string `json:"unrealisedPnl"`
			Leverage      string `json:"leverage"`
			PositionValue string `json:"positionValue"`
			LiqPrice      string `json:"liqPrice"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}

	var positions []exchange.Position
	for _, p := range resp.Result.List {
		qty, _ := strconv.ParseFloat(p.Size, 64)
		if qty == 0 {
			continue
		}
		entry, _ := strconv.ParseFloat(p.AvgPrice, 64)
		mark, _ := strconv.ParseFloat(p.MarkPrice, 64)
		pnl, _ := strconv.ParseFloat(p.UnrealisedPnl, 64)
		lev, _ := strconv.ParseFloat(p.Leverage, 64)
		value, _ := strconv.ParseFloat(p.PositionValue, 64)
		liq, _ := strconv.ParseFloat(p.LiqPrice, 64)

		side := "LONG"
		if p.Side == "Sell" {
			side = "SHORT"
		}

		positions = append(positions, exchange.Position{
			Symbol:        p.Symbol,
			Side:          side,
			Quantity:      qty,
			EntryPrice:    entry,
			MarkPrice:     mark,
			UnrealizedPnL: pnl,
			Leverage:      lev,
			NotionalUSD:   value,
			LiqPrice:      liq,
		})
	}
	return positions, nil
}

// ── Transfers ────────────────────────────────────────────────────────────────

func (c *Client) Withdraw(ctx context.Context, req exchange.WithdrawRequest) (*exchange.WithdrawResponse, error) {
	body := fmt.Sprintf(`{"coin":"%s","chain":"%s","address":"%s","amount":"%s","timestamp":%d,"forceChain":1,"accountType":"UNIFIED"}`,
		req.Asset, strings.ToUpper(req.Network), req.Address,
		fmt.Sprintf("%.8f", req.Amount), time.Now().UnixMilli())
	path := "/v5/asset/withdraw/create"
	headers := c.sign(body)

	var resp bybitResp[struct {
		ID string `json:"id"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(body), headers, &resp); err != nil {
		return nil, fmt.Errorf("bybit Withdraw: %w", err)
	}
	if err := checkCode(resp.RetCode, resp.RetMsg); err != nil {
		return nil, err
	}
	return &exchange.WithdrawResponse{
		WithdrawID: resp.Result.ID,
		Asset:      req.Asset,
		Amount:     req.Amount,
		Status:     exchange.TransferPending,
		Network:    req.Network,
	}, nil
}

func (c *Client) GetWithdrawStatus(ctx context.Context, withdrawID string) (*exchange.WithdrawResponse, error) {
	query := fmt.Sprintf("withdrawID=%s", withdrawID)
	path := "/v5/asset/withdraw/query-record?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		Rows []struct {
			WithdrawID string `json:"withdrawId"`
			Status     string `json:"status"`
			Amount     string `json:"amount"`
			TxID       string `json:"txID"`
		} `json:"rows"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result.Rows) == 0 {
		return nil, fmt.Errorf("withdrawal %s not found", withdrawID)
	}
	r := resp.Result.Rows[0]
	amt, _ := strconv.ParseFloat(r.Amount, 64)
	return &exchange.WithdrawResponse{
		WithdrawID: r.WithdrawID,
		Amount:     amt,
		Status:     mapBybitTransferStatus(r.Status),
		TxID:       r.TxID,
	}, nil
}

func (c *Client) GetDepositAddress(ctx context.Context, asset, network string) (*exchange.DepositAddress, error) {
	query := fmt.Sprintf("coin=%s&chainType=%s", asset, strings.ToUpper(network))
	path := "/v5/asset/deposit/query-address?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		Chains []struct {
			Chain   string `json:"chain"`
			Address string `json:"addressDeposit"`
			Tag     string `json:"tagDeposit"`
		} `json:"chains"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result.Chains) == 0 {
		return nil, fmt.Errorf("no deposit address for %s on %s", asset, network)
	}
	ch := resp.Result.Chains[0]
	return &exchange.DepositAddress{
		Asset:   asset,
		Address: ch.Address,
		Network: ch.Chain,
		Tag:     ch.Tag,
	}, nil
}

func (c *Client) GetDeposits(ctx context.Context, asset string, limit int) ([]exchange.DepositRecord, error) {
	query := fmt.Sprintf("coin=%s&limit=%d", asset, limit)
	path := "/v5/asset/deposit/query-record?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		Rows []struct {
			TxID        string `json:"txID"`
			Amount      string `json:"amount"`
			Coin        string `json:"coin"`
			Chain       string `json:"chain"`
			Status      int    `json:"status"`
			SuccessAt   string `json:"successAt"`
		} `json:"rows"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var records []exchange.DepositRecord
	for _, d := range resp.Result.Rows {
		amt, _ := strconv.ParseFloat(d.Amount, 64)
		status := exchange.TransferPending
		if d.Status == 3 {
			status = exchange.TransferCompleted
		}
		records = append(records, exchange.DepositRecord{
			DepositID: d.TxID,
			Asset:     d.Coin,
			Amount:    amt,
			Status:    status,
			TxID:      d.TxID,
			Network:   d.Chain,
		})
	}
	return records, nil
}

func (c *Client) GetNetworkFees(ctx context.Context, asset string) ([]exchange.NetworkFee, error) {
	query := fmt.Sprintf("coin=%s", asset)
	path := "/v5/asset/coin/query-info?" + query
	headers := c.sign(query)

	var resp bybitResp[struct {
		Rows []struct {
			Chains []struct {
				Chain      string `json:"chain"`
				WithdrawFee string `json:"withdrawFee"`
				MinWithdraw string `json:"withdrawMin"`
			} `json:"chains"`
		} `json:"rows"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var fees []exchange.NetworkFee
	if len(resp.Result.Rows) > 0 {
		for _, ch := range resp.Result.Rows[0].Chains {
			fee, _ := strconv.ParseFloat(ch.WithdrawFee, 64)
			min, _ := strconv.ParseFloat(ch.MinWithdraw, 64)
			fees = append(fees, exchange.NetworkFee{
				Network:   ch.Chain,
				Fee:       fee,
				MinAmount: min,
			})
		}
	}
	return fees, nil
}

func (c *Client) GetTickerPrice(ctx context.Context, symbol string) (*exchange.TickerPrice, error) {
	path := "/v5/market/tickers?category=linear&symbol=" + symbol
	var resp bybitResp[struct {
		List []struct {
			Symbol    string `json:"symbol"`
			LastPrice string `json:"lastPrice"`
		} `json:"list"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Result.List) == 0 {
		return nil, fmt.Errorf("no ticker for %s", symbol)
	}
	price, _ := strconv.ParseFloat(resp.Result.List[0].LastPrice, 64)
	return &exchange.TickerPrice{
		Symbol: symbol,
		Price:  price,
		TsMs:   time.Now().UnixMilli(),
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

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func mapBybitOrderType(t exchange.OrderType) string {
	switch t {
	case exchange.OrderTypeMarket:
		return "Market"
	case exchange.OrderTypeLimit, exchange.OrderTypeIOC:
		return "Limit"
	default:
		return "Market"
	}
}

func mapBybitStatus(s string) exchange.OrderStatus {
	switch s {
	case "New", "Created":
		return exchange.OrderStatusNew
	case "PartiallyFilled":
		return exchange.OrderStatusPartiallyFilled
	case "Filled":
		return exchange.OrderStatusFilled
	case "Cancelled":
		return exchange.OrderStatusCancelled
	case "Rejected":
		return exchange.OrderStatusRejected
	case "Deactivated":
		return exchange.OrderStatusExpired
	default:
		return exchange.OrderStatus(s)
	}
}

func mapBybitTransferStatus(s string) exchange.TransferStatus {
	switch s {
	case "success":
		return exchange.TransferCompleted
	case "fail", "Reject":
		return exchange.TransferFailed
	default:
		return exchange.TransferPending
	}
}
