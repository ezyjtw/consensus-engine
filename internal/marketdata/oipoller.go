package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// OpenInterestUpdate is published to the market:open_interest Redis stream.
type OpenInterestUpdate struct {
	TenantID     string  `json:"tenant_id"`
	Venue        string  `json:"venue"`
	Symbol       string  `json:"symbol"`
	TsMs         int64   `json:"ts_ms"`
	OiUSD        float64 `json:"oi_usd"`         // total open interest in USD
	OiContracts  float64 `json:"oi_contracts"`    // OI in contracts (exchange-native)
	FundingRate  float64 `json:"funding_rate"`    // current funding rate (if available)
	NextFundingMs int64  `json:"next_funding_ms"` // next funding timestamp (if available)
}

// OIPoller periodically fetches open interest from exchange REST APIs.
type OIPoller struct {
	rdb      *redis.Client
	client   *http.Client
	tenantID string
	interval time.Duration
	stream   string
	symbols  map[string]string // canonical → exchange symbol mapping
}

// NewOIPoller creates an open interest poller.
func NewOIPoller(rdb *redis.Client, tenantID string, interval time.Duration) *OIPoller {
	if interval == 0 {
		interval = 60 * time.Second
	}
	return &OIPoller{
		rdb:      rdb,
		client:   &http.Client{Timeout: 10 * time.Second},
		tenantID: tenantID,
		interval: interval,
		stream:   "market:open_interest",
		symbols: map[string]string{
			"BTC-PERP": "BTCUSDT",
			"ETH-PERP": "ETHUSDT",
		},
	}
}

// Run starts the OI polling loop. Blocks until ctx is cancelled.
func (p *OIPoller) Run(ctx context.Context) {
	log.Printf("oi-poller: started (interval=%s)", p.interval)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	p.poll(ctx)
	for {
		select {
		case <-ctx.Done():
			log.Println("oi-poller: stopped")
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *OIPoller) poll(ctx context.Context) {
	type result struct {
		updates []OpenInterestUpdate
		err     error
	}

	ch := make(chan result, 4)
	go func() { u, e := p.fetchBinanceOI(ctx); ch <- result{u, e} }()
	go func() { u, e := p.fetchBybitOI(ctx); ch <- result{u, e} }()
	go func() { u, e := p.fetchOKXOI(ctx); ch <- result{u, e} }()
	go func() { u, e := p.fetchDeribitOI(ctx); ch <- result{u, e} }()

	published := 0
	for i := 0; i < 4; i++ {
		r := <-ch
		if r.err != nil {
			if ctx.Err() == nil {
				log.Printf("oi-poller: %v", r.err)
			}
			continue
		}
		for _, u := range r.updates {
			if err := p.publish(ctx, u); err != nil && ctx.Err() == nil {
				log.Printf("oi-poller: publish: %v", err)
			} else {
				published++
			}
		}
	}
	if published > 0 {
		log.Printf("oi-poller: published %d OI updates", published)
	}
}

func (p *OIPoller) publish(ctx context.Context, u OpenInterestUpdate) error {
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		MaxLen: 2000,
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// ── Binance Futures ──────────────────────────────────────────────────────────

type binanceOIResp struct {
	Symbol       string `json:"symbol"`
	OpenInterest string `json:"openInterest"`
	Time         int64  `json:"time"`
}

func (p *OIPoller) fetchBinanceOI(ctx context.Context) ([]OpenInterestUpdate, error) {
	now := time.Now().UnixMilli()
	var updates []OpenInterestUpdate
	for canSym, exSym := range p.symbols {
		url := "https://fapi.binance.com/fapi/v1/openInterest?symbol=" + exSym
		resp, err := oiGetJSON[binanceOIResp](ctx, p.client, url)
		if err != nil {
			// Binance Futures may be blocked from cloud IPs.
			continue
		}
		oi, _ := strconv.ParseFloat(resp.OpenInterest, 64)
		if oi <= 0 {
			continue
		}
		updates = append(updates, OpenInterestUpdate{
			TenantID:    p.tenantID,
			Venue:       "binance",
			Symbol:      canSym,
			TsMs:        now,
			OiContracts: oi,
		})
	}
	return updates, nil
}

// ── Bybit ────────────────────────────────────────────────────────────────────

type bybitOIResp struct {
	RetCode int `json:"retCode"`
	Result  struct {
		List []struct {
			Symbol       string `json:"symbol"`
			OpenInterest string `json:"openInterest"`
		} `json:"list"`
	} `json:"result"`
}

func (p *OIPoller) fetchBybitOI(ctx context.Context) ([]OpenInterestUpdate, error) {
	now := time.Now().UnixMilli()
	var updates []OpenInterestUpdate
	for canSym, exSym := range p.symbols {
		url := "https://api.bybit.com/v5/market/open-interest?category=linear&symbol=" + exSym + "&intervalTime=5min&limit=1"
		resp, err := oiGetJSON[bybitOIResp](ctx, p.client, url)
		if err != nil || resp.RetCode != 0 {
			continue
		}
		if len(resp.Result.List) == 0 {
			continue
		}
		oi, _ := strconv.ParseFloat(resp.Result.List[0].OpenInterest, 64)
		if oi <= 0 {
			continue
		}
		updates = append(updates, OpenInterestUpdate{
			TenantID:    p.tenantID,
			Venue:       "bybit",
			Symbol:      canSym,
			TsMs:        now,
			OiContracts: oi,
		})
	}
	return updates, nil
}

// ── OKX ──────────────────────────────────────────────────────────────────────

type okxOIResp struct {
	Data []struct {
		InstID string `json:"instId"`
		Oi     string `json:"oi"`    // contracts
		OiCcy  string `json:"oiCcy"` // in coin terms
	} `json:"data"`
}

func (p *OIPoller) fetchOKXOI(ctx context.Context) ([]OpenInterestUpdate, error) {
	now := time.Now().UnixMilli()
	okxSymbols := map[string]string{
		"BTC-PERP": "BTC-USDT-SWAP",
		"ETH-PERP": "ETH-USDT-SWAP",
	}
	var updates []OpenInterestUpdate
	for canSym, exSym := range okxSymbols {
		url := "https://www.okx.com/api/v5/public/open-interest?instId=" + exSym
		resp, err := oiGetJSON[okxOIResp](ctx, p.client, url)
		if err != nil || len(resp.Data) == 0 {
			continue
		}
		oi, _ := strconv.ParseFloat(resp.Data[0].Oi, 64)
		if oi <= 0 {
			continue
		}
		updates = append(updates, OpenInterestUpdate{
			TenantID:    p.tenantID,
			Venue:       "okx",
			Symbol:      canSym,
			TsMs:        now,
			OiContracts: oi,
		})
	}
	return updates, nil
}

// ── Deribit ──────────────────────────────────────────────────────────────────

func (p *OIPoller) fetchDeribitOI(ctx context.Context) ([]OpenInterestUpdate, error) {
	now := time.Now().UnixMilli()
	deribitSymbols := map[string]string{
		"BTC-PERP": "BTC-PERPETUAL",
		"ETH-PERP": "ETH-PERPETUAL",
	}
	var updates []OpenInterestUpdate
	for canSym, exSym := range deribitSymbols {
		url := "https://www.deribit.com/api/v2/public/get_book_summary_by_instrument?instrument_name=" + exSym
		type resp struct {
			Result []struct {
				OpenInterest float64 `json:"open_interest"`
			} `json:"result"`
		}
		r, err := oiGetJSON[resp](ctx, p.client, url)
		if err != nil || len(r.Result) == 0 {
			continue
		}
		oi := r.Result[0].OpenInterest
		if oi <= 0 {
			continue
		}
		updates = append(updates, OpenInterestUpdate{
			TenantID:    p.tenantID,
			Venue:       "deribit",
			Symbol:      canSym,
			TsMs:        now,
			OiContracts: oi,
		})
	}
	return updates, nil
}

// ── HTTP helper ──────────────────────────────────────────────────────────────

func oiGetJSON[T any](ctx context.Context, client *http.Client, url string) (*T, error) {
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
