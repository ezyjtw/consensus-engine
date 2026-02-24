package dashboard

// pricepoller.go — Lightweight background goroutine that fetches live prices
// from public exchange REST APIs and publishes them to the market:quotes Redis
// stream. This ensures the dashboard displays real-time prices even when the
// dedicated market-data WebSocket service is not running.
//
// Supported venues: Binance (spot), OKX, Bybit, Deribit.
// All endpoints used are public (no API key required).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/redis/go-redis/v9"
)

// PricePoller periodically fetches prices from public exchange APIs and
// writes them to the market:quotes Redis stream.
type PricePoller struct {
	rdb      *redis.Client
	client   *http.Client
	tenantID string
	interval time.Duration
	stream   string
}

// NewPricePoller creates a price poller. interval controls how frequently
// prices are fetched (recommend 5–10s to avoid rate limits).
func NewPricePoller(rdb *redis.Client, tenantID string, interval time.Duration) *PricePoller {
	if interval == 0 {
		interval = 5 * time.Second
	}
	return &PricePoller{
		rdb:      rdb,
		client:   &http.Client{Timeout: 8 * time.Second},
		tenantID: tenantID,
		interval: interval,
		stream:   "market:quotes",
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (p *PricePoller) Run(ctx context.Context) {
	log.Printf("price-poller: started (interval=%s)", p.interval)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	// Fetch immediately on start, then on each tick.
	p.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Println("price-poller: stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// venueFetcher is a named fetch function for a single venue.
type venueFetcher struct {
	name  string
	fetch func(context.Context) ([]consensus.Quote, error)
}

func (p *PricePoller) poll(ctx context.Context) {
	fetchers := []venueFetcher{
		{"binance", p.fetchBinance},
		{"okx", p.fetchOKX},
		{"bybit", p.fetchBybit},
		{"deribit", p.fetchDeribit},
	}

	type result struct {
		name   string
		quotes []consensus.Quote
		err    error
	}

	ch := make(chan result, len(fetchers))
	for _, f := range fetchers {
		f := f
		go func() {
			q, e := f.fetch(ctx)
			ch <- result{f.name, q, e}
		}()
	}

	published := 0
	for i := 0; i < len(fetchers); i++ {
		r := <-ch
		if r.err != nil {
			if ctx.Err() == nil {
				log.Printf("price-poller: %s: %v", r.name, r.err)
			}
			continue
		}
		for _, q := range r.quotes {
			if err := p.publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("price-poller: publish: %v", err)
			} else {
				published++
			}
		}
	}
	if published > 0 {
		log.Printf("price-poller: published %d quotes", published)
	}
}

func (p *PricePoller) publish(ctx context.Context, q consensus.Quote) error {
	q.SchemaVersion = 1
	data, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		MaxLen: 5000,
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// ── Binance (Spot API — more accessible from cloud IPs than Futures API) ─────

// binanceSymbols maps exchange symbol → canonical symbol.
var binanceSymbols = map[string]string{
	"BTCUSDT": "BTC-PERP",
	"ETHUSDT": "ETH-PERP",
	"BNBUSDT": "BNB-PERP",
	"SOLUSDT": "SOL-PERP",
	"XRPUSDT": "XRP-PERP",
}

type binanceSpotTicker struct {
	Symbol   string `json:"symbol"`
	BidPrice string `json:"bidPrice"`
	AskPrice string `json:"askPrice"`
}

func (p *PricePoller) fetchBinance(ctx context.Context) ([]consensus.Quote, error) {
	// Use the spot API (api.binance.com) which is accessible from cloud IPs.
	// The futures API (fapi.binance.com) blocks many cloud provider IPs.
	tickers, err := httpGetJSON[[]binanceSpotTicker](ctx, p.client,
		"https://api.binance.com/api/v3/ticker/bookTicker")
	if err != nil {
		return nil, fmt.Errorf("binance bookTicker: %w", err)
	}

	now := time.Now().UnixMilli()
	var quotes []consensus.Quote
	for _, t := range *tickers {
		canSym, ok := binanceSymbols[t.Symbol]
		if !ok {
			continue
		}
		bid, _ := strconv.ParseFloat(t.BidPrice, 64)
		ask, _ := strconv.ParseFloat(t.AskPrice, 64)
		if bid <= 0 || ask <= 0 {
			continue
		}
		quotes = append(quotes, consensus.Quote{
			TenantID:    p.tenantID,
			Venue:       "binance",
			Symbol:      consensus.Symbol(canSym),
			TsMs:        now,
			BestBid:     bid,
			BestAsk:     ask,
			Mark:        (bid + ask) / 2,
			FeeBpsTaker: 4.0,
			FeedHealth:  consensus.FeedHealth{WsConnected: false, LastMsgTsMs: now},
		})
	}
	if len(quotes) == 0 {
		return nil, fmt.Errorf("no matching symbols in response")
	}
	return quotes, nil
}

// ── OKX ──────────────────────────────────────────────────────────────────────

var okxSymbols = map[string]string{
	"BTC-USDT-SWAP": "BTC-PERP",
	"ETH-USDT-SWAP": "ETH-PERP",
	"SOL-USDT-SWAP": "SOL-PERP",
	"XRP-USDT-SWAP": "XRP-PERP",
}

type okxTickerResp struct {
	Data []struct {
		InstID      string `json:"instId"`
		BidPx       string `json:"bidPx"`
		AskPx       string `json:"askPx"`
		Last        string `json:"last"`
		FundingRate string `json:"fundingRate,omitempty"`
	} `json:"data"`
}

func (p *PricePoller) fetchOKX(ctx context.Context) ([]consensus.Quote, error) {
	// OKX supports fetching multiple tickers for a given instrument type.
	resp, err := httpGetJSON[okxTickerResp](ctx, p.client,
		"https://www.okx.com/api/v5/market/tickers?instType=SWAP")
	if err != nil {
		return nil, fmt.Errorf("okx tickers: %w", err)
	}

	now := time.Now().UnixMilli()
	var quotes []consensus.Quote
	for _, d := range resp.Data {
		canSym, ok := okxSymbols[d.InstID]
		if !ok {
			continue
		}
		bid, _ := strconv.ParseFloat(d.BidPx, 64)
		ask, _ := strconv.ParseFloat(d.AskPx, 64)
		if bid <= 0 || ask <= 0 {
			continue
		}
		q := consensus.Quote{
			TenantID:    p.tenantID,
			Venue:       "okx",
			Symbol:      consensus.Symbol(canSym),
			TsMs:        now,
			BestBid:     bid,
			BestAsk:     ask,
			FeeBpsTaker: 5.0,
			FeedHealth:  consensus.FeedHealth{WsConnected: false, LastMsgTsMs: now},
		}
		q.Mark, _ = strconv.ParseFloat(d.Last, 64)
		quotes = append(quotes, q)
	}
	return quotes, nil
}

// ── Bybit ────────────────────────────────────────────────────────────────────

var bybitSymbols = map[string]string{
	"BTCUSDT": "BTC-PERP",
	"ETHUSDT": "ETH-PERP",
	"BNBUSDT": "BNB-PERP",
	"SOLUSDT": "SOL-PERP",
	"XRPUSDT": "XRP-PERP",
}

type bybitTickerResp struct {
	RetCode int `json:"retCode"`
	Result  struct {
		List []struct {
			Symbol      string `json:"symbol"`
			Bid1Price   string `json:"bid1Price"`
			Ask1Price   string `json:"ask1Price"`
			MarkPrice   string `json:"markPrice"`
			IndexPrice  string `json:"indexPrice"`
			FundingRate string `json:"fundingRate"`
		} `json:"list"`
	} `json:"result"`
}

func (p *PricePoller) fetchBybit(ctx context.Context) ([]consensus.Quote, error) {
	resp, err := httpGetJSON[bybitTickerResp](ctx, p.client,
		"https://api.bybit.com/v5/market/tickers?category=linear")
	if err != nil {
		return nil, fmt.Errorf("bybit tickers: %w", err)
	}
	if resp.RetCode != 0 {
		return nil, fmt.Errorf("bybit retCode=%d", resp.RetCode)
	}

	now := time.Now().UnixMilli()
	var quotes []consensus.Quote
	for _, t := range resp.Result.List {
		canSym, ok := bybitSymbols[t.Symbol]
		if !ok {
			continue
		}
		bid, _ := strconv.ParseFloat(t.Bid1Price, 64)
		ask, _ := strconv.ParseFloat(t.Ask1Price, 64)
		if bid <= 0 || ask <= 0 {
			continue
		}
		q := consensus.Quote{
			TenantID:    p.tenantID,
			Venue:       "bybit",
			Symbol:      consensus.Symbol(canSym),
			TsMs:        now,
			BestBid:     bid,
			BestAsk:     ask,
			FeeBpsTaker: 5.5,
			FeedHealth:  consensus.FeedHealth{WsConnected: false, LastMsgTsMs: now},
		}
		q.Mark, _ = strconv.ParseFloat(t.MarkPrice, 64)
		q.Index, _ = strconv.ParseFloat(t.IndexPrice, 64)
		q.FundingRate, _ = strconv.ParseFloat(t.FundingRate, 64)
		quotes = append(quotes, q)
	}
	return quotes, nil
}

// ── Deribit ──────────────────────────────────────────────────────────────────

// Deribit only offers BTC and ETH perpetuals.
var deribitSymbols = map[string]string{
	"BTC-PERPETUAL": "BTC-PERP",
	"ETH-PERPETUAL": "ETH-PERP",
}

type deribitTickerResp struct {
	Result struct {
		BestBidPrice float64 `json:"best_bid_price"`
		BestAskPrice float64 `json:"best_ask_price"`
		MarkPrice    float64 `json:"mark_price"`
		IndexPrice   float64 `json:"index_price"`
		FundingRate  float64 `json:"current_funding"`
	} `json:"result"`
}

func (p *PricePoller) fetchDeribit(ctx context.Context) ([]consensus.Quote, error) {
	now := time.Now().UnixMilli()
	var quotes []consensus.Quote
	for exSym, canSym := range deribitSymbols {
		url := "https://www.deribit.com/api/v2/public/ticker?instrument_name=" + exSym
		resp, err := httpGetJSON[deribitTickerResp](ctx, p.client, url)
		if err != nil {
			log.Printf("price-poller: deribit %s: %v", exSym, err)
			continue
		}
		r := resp.Result
		if r.BestBidPrice <= 0 || r.BestAskPrice <= 0 {
			continue
		}
		quotes = append(quotes, consensus.Quote{
			TenantID:    p.tenantID,
			Venue:       "deribit",
			Symbol:      consensus.Symbol(canSym),
			TsMs:        now,
			BestBid:     r.BestBidPrice,
			BestAsk:     r.BestAskPrice,
			Mark:        r.MarkPrice,
			Index:       r.IndexPrice,
			FundingRate: r.FundingRate,
			FeeBpsTaker: 3.0,
			FeedHealth:  consensus.FeedHealth{WsConnected: false, LastMsgTsMs: now},
		})
	}
	return quotes, nil
}

// ── HTTP helper ──────────────────────────────────────────────────────────────

func httpGetJSON[T any](ctx context.Context, client *http.Client, url string) (*T, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}
