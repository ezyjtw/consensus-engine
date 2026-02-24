package dashboard

// pricepoller.go — Lightweight background goroutine that fetches live prices
// from public exchange REST APIs and publishes them to the market:quotes Redis
// stream. This ensures the dashboard displays real-time prices even when the
// dedicated market-data WebSocket service is not running.
//
// Supported venues: Binance Futures, OKX, Bybit.
// All endpoints used are public (no API key required).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
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

func (p *PricePoller) poll(ctx context.Context) {
	// Fetch from all venues concurrently.
	type result struct {
		quotes []consensus.Quote
		err    error
	}

	ch := make(chan result, 3)
	go func() { q, e := p.fetchBinance(ctx); ch <- result{q, e} }()
	go func() { q, e := p.fetchOKX(ctx); ch <- result{q, e} }()
	go func() { q, e := p.fetchBybit(ctx); ch <- result{q, e} }()

	published := 0
	for i := 0; i < 3; i++ {
		r := <-ch
		if r.err != nil {
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
	data, err := json.Marshal(q)
	if err != nil {
		return err
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		MaxLen: 5000, // cap stream size
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// ── Binance Futures ──────────────────────────────────────────────────────────

// Symbols to track (exchange symbol → canonical symbol).
var binanceSymbols = map[string]string{
	"BTCUSDT": "BTC-PERP",
	"ETHUSDT": "ETH-PERP",
}

type binanceTicker struct {
	Symbol   string `json:"symbol"`
	BidPrice string `json:"bidPrice"`
	AskPrice string `json:"askPrice"`
}

type binancePremiumIndex struct {
	Symbol          string `json:"symbol"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	LastFundingRate string `json:"lastFundingRate"`
}

func (p *PricePoller) fetchBinance(ctx context.Context) ([]consensus.Quote, error) {
	// Fetch book tickers.
	tickers, err := httpGetJSON[[]binanceTicker](ctx, p.client,
		"https://fapi.binance.com/fapi/v1/ticker/bookTicker")
	if err != nil {
		return nil, fmt.Errorf("binance bookTicker: %w", err)
	}
	tickerMap := make(map[string]binanceTicker)
	for _, t := range *tickers {
		if _, ok := binanceSymbols[t.Symbol]; ok {
			tickerMap[t.Symbol] = t
		}
	}

	// Fetch mark/index/funding.
	premiums, err := httpGetJSON[[]binancePremiumIndex](ctx, p.client,
		"https://fapi.binance.com/fapi/v1/premiumIndex")
	if err != nil {
		return nil, fmt.Errorf("binance premiumIndex: %w", err)
	}
	premiumMap := make(map[string]binancePremiumIndex)
	for _, pi := range *premiums {
		if _, ok := binanceSymbols[pi.Symbol]; ok {
			premiumMap[pi.Symbol] = pi
		}
	}

	now := time.Now().UnixMilli()
	var quotes []consensus.Quote
	for exSym, canSym := range binanceSymbols {
		t, tok := tickerMap[exSym]
		if !tok {
			continue
		}
		bid, _ := strconv.ParseFloat(t.BidPrice, 64)
		ask, _ := strconv.ParseFloat(t.AskPrice, 64)
		if bid <= 0 || ask <= 0 {
			continue
		}
		q := consensus.Quote{
			TenantID:    p.tenantID,
			Venue:       "binance",
			Symbol:      consensus.Symbol(canSym),
			TsMs:        now,
			BestBid:     bid,
			BestAsk:     ask,
			FeeBpsTaker: 4.0,
			FeedHealth:  consensus.FeedHealth{WsConnected: false, LastMsgTsMs: now},
		}
		if pi, ok := premiumMap[exSym]; ok {
			q.Mark, _ = strconv.ParseFloat(pi.MarkPrice, 64)
			q.Index, _ = strconv.ParseFloat(pi.IndexPrice, 64)
			q.FundingRate, _ = strconv.ParseFloat(pi.LastFundingRate, 64)
		}
		quotes = append(quotes, q)
	}
	return quotes, nil
}

// ── OKX ──────────────────────────────────────────────────────────────────────

var okxSymbols = map[string]string{
	"BTC-USDT-SWAP": "BTC-PERP",
	"ETH-USDT-SWAP": "ETH-PERP",
}

type okxTickerResp struct {
	Data []struct {
		InstID  string `json:"instId"`
		BidPx   string `json:"bidPx"`
		AskPx   string `json:"askPx"`
		Last    string `json:"last"`
		FundingRate string `json:"fundingRate,omitempty"`
	} `json:"data"`
}

func (p *PricePoller) fetchOKX(ctx context.Context) ([]consensus.Quote, error) {
	now := time.Now().UnixMilli()
	var quotes []consensus.Quote
	for exSym, canSym := range okxSymbols {
		url := "https://www.okx.com/api/v5/market/ticker?instId=" + exSym
		resp, err := httpGetJSON[okxTickerResp](ctx, p.client, url)
		if err != nil || len(resp.Data) == 0 {
			continue
		}
		d := resp.Data[0]
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
}

type bybitTickerResp struct {
	Result struct {
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
