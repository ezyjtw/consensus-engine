// Package okx implements the Exchange interface for OKX.
package okx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

const baseURL = "https://www.okx.com"

// Client implements exchange.Exchange for OKX.
type Client struct {
	http       *exchange.HTTPClient
	apiKey     string
	apiSecret  string
	passphrase string
}

// New creates an OKX exchange client.
func New(creds *exchange.Credentials) (exchange.Exchange, error) {
	if creds.APIKey == "" || creds.APISecret == "" || creds.Passphrase == "" {
		return nil, fmt.Errorf("okx: api_key, api_secret, and passphrase are required")
	}
	return &Client{
		http:       exchange.NewHTTPClient(baseURL, 10*time.Second),
		apiKey:     creds.APIKey,
		apiSecret:  creds.APISecret,
		passphrase: creds.Passphrase,
	}, nil
}

func (c *Client) Name() string { return "okx" }

func (c *Client) sign(method, path, body string) map[string]string {
	ts := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	preSign := ts + method + path + body
	mac := hmac.New(sha256.New, []byte(c.apiSecret))
	mac.Write([]byte(preSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return map[string]string{
		"OK-ACCESS-KEY":        c.apiKey,
		"OK-ACCESS-SIGN":       sig,
		"OK-ACCESS-TIMESTAMP":  ts,
		"OK-ACCESS-PASSPHRASE": c.passphrase,
	}
}

// okxResp is the common OKX API envelope.
type okxResp[T any] struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []T    `json:"data"`
}

func checkCode(code, msg string) error {
	if code != "0" {
		return fmt.Errorf("okx API error code=%s: %s", code, msg)
	}
	return nil
}

// ── Trading ──────────────────────────────────────────────────────────────────

func (c *Client) PlaceOrder(ctx context.Context, req exchange.OrderRequest) (*exchange.OrderResponse, error) {
	body := fmt.Sprintf(`{"instId":"%s","tdMode":"cross","side":"%s","ordType":"%s","sz":"%s","clOrdId":"%s"`,
		req.Symbol, strings.ToLower(string(req.Side)), mapOKXOrderType(req.Type),
		fmt.Sprintf("%.8f", req.Quantity), req.ClientOrderID)
	if req.Type == exchange.OrderTypeLimit || req.Type == exchange.OrderTypeIOC {
		body += fmt.Sprintf(`,"px":"%s"`, fmt.Sprintf("%.2f", req.Price))
	}
	if req.ReduceOnly {
		body += `,"reduceOnly":true`
	}
	body += "}"

	path := "/api/v5/trade/order"
	headers := c.sign("POST", path, body)

	var resp okxResp[struct {
		OrdID  string `json:"ordId"`
		ClOrdID string `json:"clOrdId"`
		SCode  string `json:"sCode"`
		SMsg   string `json:"sMsg"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(body), headers, &resp); err != nil {
		return nil, fmt.Errorf("okx PlaceOrder: %w", err)
	}
	if err := checkCode(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("okx PlaceOrder: empty response")
	}
	d := resp.Data[0]
	if d.SCode != "0" {
		return nil, fmt.Errorf("okx PlaceOrder rejected: %s", d.SMsg)
	}

	return &exchange.OrderResponse{
		OrderID:       d.OrdID,
		ClientOrderID: d.ClOrdID,
		Symbol:        req.Symbol,
		Side:          req.Side,
		Type:          req.Type,
		Status:        exchange.OrderStatusNew,
		CreatedAt:     time.Now(),
	}, nil
}

func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	body := fmt.Sprintf(`{"instId":"%s","ordId":"%s"}`, symbol, orderID)
	path := "/api/v5/trade/cancel-order"
	headers := c.sign("POST", path, body)
	var resp okxResp[struct{}]
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(body), headers, &resp); err != nil {
		return err
	}
	return checkCode(resp.Code, resp.Msg)
}

func (c *Client) GetOrder(ctx context.Context, symbol, orderID string) (*exchange.OrderResponse, error) {
	path := fmt.Sprintf("/api/v5/trade/order?instId=%s&ordId=%s", symbol, orderID)
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		OrdID   string `json:"ordId"`
		InstID  string `json:"instId"`
		Side    string `json:"side"`
		State   string `json:"state"`
		AvgPx   string `json:"avgPx"`
		Sz      string `json:"sz"`
		FillSz  string `json:"fillSz"`
		Fee     string `json:"fee"`
		UTime   string `json:"uTime"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("order %s not found", orderID)
	}

	d := resp.Data[0]
	avgPx, _ := strconv.ParseFloat(d.AvgPx, 64)
	sz, _ := strconv.ParseFloat(d.Sz, 64)
	fillSz, _ := strconv.ParseFloat(d.FillSz, 64)
	fee, _ := strconv.ParseFloat(d.Fee, 64)
	uTime, _ := strconv.ParseInt(d.UTime, 10, 64)

	return &exchange.OrderResponse{
		OrderID:      d.OrdID,
		Symbol:       d.InstID,
		Side:         exchange.Side(strings.ToUpper(d.Side)),
		Status:       mapOKXStatus(d.State),
		AvgFillPrice: avgPx,
		Quantity:     sz,
		FilledQty:    fillSz,
		FeesUSD:      -fee, // OKX reports fees as negative
		UpdatedAt:    time.UnixMilli(uTime),
	}, nil
}

// ── Account ──────────────────────────────────────────────────────────────────

func (c *Client) GetBalances(ctx context.Context) ([]exchange.Balance, error) {
	path := "/api/v5/account/balance"
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		Details []struct {
			Ccy       string `json:"ccy"`
			CashBal   string `json:"cashBal"`
			AvailBal  string `json:"availBal"`
			FrozenBal string `json:"frozenBal"`
		} `json:"details"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.Code, resp.Msg); err != nil {
		return nil, err
	}

	var balances []exchange.Balance
	if len(resp.Data) > 0 {
		for _, d := range resp.Data[0].Details {
			total, _ := strconv.ParseFloat(d.CashBal, 64)
			free, _ := strconv.ParseFloat(d.AvailBal, 64)
			frozen, _ := strconv.ParseFloat(d.FrozenBal, 64)
			if total == 0 {
				continue
			}
			balances = append(balances, exchange.Balance{
				Asset:  d.Ccy,
				Free:   free,
				Locked: frozen,
				Total:  total,
			})
		}
	}
	return balances, nil
}

func (c *Client) GetPositions(ctx context.Context) ([]exchange.Position, error) {
	path := "/api/v5/account/positions"
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		InstID  string `json:"instId"`
		Pos     string `json:"pos"`
		AvgPx   string `json:"avgPx"`
		MarkPx  string `json:"markPx"`
		Upl     string `json:"upl"`
		Lever   string `json:"lever"`
		Notional string `json:"notionalUsd"`
		LiqPx   string `json:"liqPx"`
		PosSide string `json:"posSide"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.Code, resp.Msg); err != nil {
		return nil, err
	}

	var positions []exchange.Position
	for _, p := range resp.Data {
		qty, _ := strconv.ParseFloat(p.Pos, 64)
		if qty == 0 {
			continue
		}
		entry, _ := strconv.ParseFloat(p.AvgPx, 64)
		mark, _ := strconv.ParseFloat(p.MarkPx, 64)
		pnl, _ := strconv.ParseFloat(p.Upl, 64)
		lev, _ := strconv.ParseFloat(p.Lever, 64)
		notional, _ := strconv.ParseFloat(p.Notional, 64)
		liq, _ := strconv.ParseFloat(p.LiqPx, 64)

		side := "LONG"
		if qty < 0 {
			side = "SHORT"
			qty = -qty
		}
		if p.PosSide == "short" {
			side = "SHORT"
		}

		positions = append(positions, exchange.Position{
			Symbol:        p.InstID,
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
	body := fmt.Sprintf(`{"ccy":"%s","amt":"%s","dest":"4","toAddr":"%s","chain":"%s-%s","fee":"0"}`,
		req.Asset, fmt.Sprintf("%.8f", req.Amount), req.Address, req.Asset, strings.ToUpper(req.Network))
	path := "/api/v5/asset/withdrawal"
	headers := c.sign("POST", path, body)

	var resp okxResp[struct {
		WdID string `json:"wdId"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodPost, path, strings.NewReader(body), headers, &resp); err != nil {
		return nil, fmt.Errorf("okx Withdraw: %w", err)
	}
	if err := checkCode(resp.Code, resp.Msg); err != nil {
		return nil, err
	}
	wdID := ""
	if len(resp.Data) > 0 {
		wdID = resp.Data[0].WdID
	}
	return &exchange.WithdrawResponse{
		WithdrawID: wdID,
		Asset:      req.Asset,
		Amount:     req.Amount,
		Status:     exchange.TransferPending,
		Network:    req.Network,
	}, nil
}

func (c *Client) GetWithdrawStatus(ctx context.Context, withdrawID string) (*exchange.WithdrawResponse, error) {
	path := fmt.Sprintf("/api/v5/asset/deposit-withdraw-status?wdId=%s", withdrawID)
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		WdID  string `json:"wdId"`
		State string `json:"state"`
		TxID  string `json:"txId"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	status := exchange.TransferPending
	if len(resp.Data) > 0 {
		switch resp.Data[0].State {
		case "2":
			status = exchange.TransferCompleted
		case "-1", "-2":
			status = exchange.TransferFailed
		}
	}
	txID := ""
	if len(resp.Data) > 0 {
		txID = resp.Data[0].TxID
	}
	return &exchange.WithdrawResponse{
		WithdrawID: withdrawID,
		Status:     status,
		TxID:       txID,
	}, nil
}

func (c *Client) GetDepositAddress(ctx context.Context, asset, network string) (*exchange.DepositAddress, error) {
	path := fmt.Sprintf("/api/v5/asset/deposit-address?ccy=%s", asset)
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		Addr  string `json:"addr"`
		Tag   string `json:"tag"`
		Chain string `json:"chain"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}
	if err := checkCode(resp.Code, resp.Msg); err != nil {
		return nil, err
	}

	target := fmt.Sprintf("%s-%s", asset, strings.ToUpper(network))
	for _, d := range resp.Data {
		if strings.EqualFold(d.Chain, target) {
			return &exchange.DepositAddress{
				Asset:   asset,
				Address: d.Addr,
				Network: network,
				Tag:     d.Tag,
			}, nil
		}
	}
	if len(resp.Data) > 0 {
		return &exchange.DepositAddress{
			Asset:   asset,
			Address: resp.Data[0].Addr,
			Network: network,
			Tag:     resp.Data[0].Tag,
		}, nil
	}
	return nil, fmt.Errorf("no deposit address found for %s on %s", asset, network)
}

func (c *Client) GetDeposits(ctx context.Context, asset string, limit int) ([]exchange.DepositRecord, error) {
	path := fmt.Sprintf("/api/v5/asset/deposit-history?ccy=%s&limit=%d", asset, limit)
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		DepID string `json:"depId"`
		Ccy   string `json:"ccy"`
		Amt   string `json:"amt"`
		State string `json:"state"`
		TxID  string `json:"txId"`
		Chain string `json:"chain"`
		Ts    string `json:"ts"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var records []exchange.DepositRecord
	for _, d := range resp.Data {
		amt, _ := strconv.ParseFloat(d.Amt, 64)
		ts, _ := strconv.ParseInt(d.Ts, 10, 64)
		status := exchange.TransferPending
		if d.State == "2" {
			status = exchange.TransferCompleted
		}
		records = append(records, exchange.DepositRecord{
			DepositID: d.DepID,
			Asset:     d.Ccy,
			Amount:    amt,
			Status:    status,
			TxID:      d.TxID,
			Network:   d.Chain,
			CreatedAt: time.UnixMilli(ts),
		})
	}
	return records, nil
}

func (c *Client) GetNetworkFees(ctx context.Context, asset string) ([]exchange.NetworkFee, error) {
	path := fmt.Sprintf("/api/v5/asset/currencies?ccy=%s", asset)
	headers := c.sign("GET", path, "")

	var resp okxResp[struct {
		Chain      string `json:"chain"`
		MinFee     string `json:"minFee"`
		MinWd      string `json:"minWd"`
		MaxWd      string `json:"maxWd"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, headers, &resp); err != nil {
		return nil, err
	}

	var fees []exchange.NetworkFee
	for _, d := range resp.Data {
		fee, _ := strconv.ParseFloat(d.MinFee, 64)
		min, _ := strconv.ParseFloat(d.MinWd, 64)
		max, _ := strconv.ParseFloat(d.MaxWd, 64)
		fees = append(fees, exchange.NetworkFee{
			Network:   d.Chain,
			Fee:       fee,
			MinAmount: min,
			MaxAmount: max,
		})
	}
	return fees, nil
}

func (c *Client) GetTickerPrice(ctx context.Context, symbol string) (*exchange.TickerPrice, error) {
	path := "/api/v5/market/ticker?instId=" + symbol
	var resp okxResp[struct {
		InstID string `json:"instId"`
		Last   string `json:"last"`
		Ts     string `json:"ts"`
	}]
	if err := c.http.DoJSON(ctx, http.MethodGet, path, nil, nil, &resp); err != nil {
		return nil, err
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("no ticker for %s", symbol)
	}
	price, _ := strconv.ParseFloat(resp.Data[0].Last, 64)
	ts, _ := strconv.ParseInt(resp.Data[0].Ts, 10, 64)
	return &exchange.TickerPrice{Symbol: symbol, Price: price, TsMs: ts}, nil
}

// ── Constraints ──────────────────────────────────────────────────────────────

func (c *Client) GetConstraints(_ context.Context, symbol string) (*exchange.VenueConstraints, error) {
	return &exchange.VenueConstraints{
		Symbol:      symbol,
		TickSize:    0.1,
		LotSize:     0.001,
		MinQty:      0.001,
		MinNotional: 5.0,
		PostOnly:    true,
		ReduceOnly:  true,
	}, nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func mapOKXOrderType(t exchange.OrderType) string {
	switch t {
	case exchange.OrderTypeMarket:
		return "market"
	case exchange.OrderTypeLimit:
		return "limit"
	case exchange.OrderTypeIOC:
		return "ioc"
	default:
		return "market"
	}
}

func mapOKXStatus(s string) exchange.OrderStatus {
	switch s {
	case "live":
		return exchange.OrderStatusNew
	case "partially_filled":
		return exchange.OrderStatusPartiallyFilled
	case "filled":
		return exchange.OrderStatusFilled
	case "canceled":
		return exchange.OrderStatusCancelled
	default:
		return exchange.OrderStatus(s)
	}
}
