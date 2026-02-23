package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/yourorg/arbsuite/internal/consensus"
)

// BybitAdapter streams quotes from the Bybit V5 linear public WebSocket.
// The tickers topic delivers bid/ask, mark, index, and funding rate in one
// message, so no additional channels are required for most fields.
type BybitAdapter struct {
	venueCfg VenueConfig
	pub      *Publisher
	tenantID string
	backoffs []int
	depth    int
}

func NewBybitAdapter(venueCfg VenueConfig, pub *Publisher, tenantID string, backoffs []int, depth int) *BybitAdapter {
	return &BybitAdapter{
		venueCfg: venueCfg,
		pub:      pub,
		tenantID: tenantID,
		backoffs: backoffs,
		depth:    depth,
	}
}

func (a *BybitAdapter) Run(ctx context.Context) {
	runWithBackoff(ctx, "bybit", a.backoffs, a.connect)
}

// ── Bybit message types ───────────────────────────────────────────────────

type bybitSubscribeMsg struct {
	Op   string   `json:"op"`
	Args []string `json:"args"`
}

type bybitEnvelope struct {
	Topic string          `json:"topic"`
	Type  string          `json:"type"`
	Data  json.RawMessage `json:"data"`
	Ts    int64           `json:"ts"`
	Op    string          `json:"op"` // used for pong responses
}

type bybitTickerData struct {
	Symbol        string `json:"symbol"`
	Bid1Price     string `json:"bid1Price"`
	Ask1Price     string `json:"ask1Price"`
	MarkPrice     string `json:"markPrice"`
	IndexPrice    string `json:"indexPrice"`
	FundingRate   string `json:"fundingRate"`
}

type bybitOrderbookData struct {
	S    string     `json:"s"`
	Bids [][]string `json:"b"`
	Asks [][]string `json:"a"`
}

func (a *BybitAdapter) connect(ctx context.Context) error {
	log.Printf("bybit: connecting to %s", a.venueCfg.WsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.venueCfg.WsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// Build subscription args: tickers + orderbook for each symbol.
	var args []string
	for _, sym := range a.venueCfg.Symbols {
		args = append(args,
			"tickers."+sym,
			fmt.Sprintf("orderbook.%d.%s", a.depth, sym),
		)
	}
	subMsg := bybitSubscribeMsg{Op: "subscribe", Args: args}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Build quote state per canonical symbol.
	states := make(map[string]*quoteState) // canonical symbol → state
	for exSym, canSym := range a.venueCfg.SymbolMap {
		_ = exSym
		states[canSym] = &quoteState{
			quote: consensus.Quote{
				TenantID:    a.tenantID,
				Venue:       "bybit",
				Symbol:      consensus.Symbol(canSym),
				FeeBpsTaker: a.venueCfg.FeeBpsTaker,
				FeedHealth:  consensus.FeedHealth{WsConnected: true},
			},
		}
	}

	// Send heartbeat every 20 seconds per Bybit docs.
	pingTicker := time.NewTicker(20 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				if err := conn.WriteJSON(map[string]string{"op": "ping"}); err != nil {
					return
				}
			}
		}
	}()

	log.Printf("bybit: connected, tracking %d symbols", len(states))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			a.publishStale(ctx, states)
			return fmt.Errorf("read: %w", err)
		}

		var env bybitEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}

		// Pong responses and subscription confirmations — skip.
		if env.Op == "pong" || env.Op == "subscribe" || env.Topic == "" {
			continue
		}

		now := time.Now().UnixMilli()

		// Determine exchange symbol from topic (e.g. "tickers.BTCUSDT").
		parts := strings.SplitN(env.Topic, ".", 2)
		if len(parts) < 2 {
			continue
		}
		topicType := parts[0]
		// For orderbook topics like "orderbook.10.BTCUSDT", symbol is last part.
		exSym := parts[1]
		if topicType == "orderbook" {
			subParts := strings.SplitN(exSym, ".", 2)
			if len(subParts) == 2 {
				exSym = subParts[1]
			}
		}

		canSym, ok := a.venueCfg.SymbolMap[exSym]
		if !ok {
			continue
		}
		qs, ok := states[canSym]
		if !ok {
			continue
		}

		var q consensus.Quote
		published := false

		switch topicType {
		case "tickers":
			var td bybitTickerData
			if err := json.Unmarshal(env.Data, &td); err != nil {
				continue
			}
			bid, _ := strconv.ParseFloat(td.Bid1Price, 64)
			ask, _ := strconv.ParseFloat(td.Ask1Price, 64)
			mark, _ := strconv.ParseFloat(td.MarkPrice, 64)
			index, _ := strconv.ParseFloat(td.IndexPrice, 64)
			fr, _ := strconv.ParseFloat(td.FundingRate, 64)
			q = qs.update(func(q *consensus.Quote) {
				if bid > 0 {
					q.BestBid = bid
				}
				if ask > 0 {
					q.BestAsk = ask
				}
				if mark > 0 {
					q.Mark = mark
				}
				if index > 0 {
					q.Index = index
				}
				if fr != 0 {
					q.FundingRate = fr
				}
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			published = true

		case "orderbook":
			var od bybitOrderbookData
			if err := json.Unmarshal(env.Data, &od); err != nil {
				continue
			}
			bids := parseStringPairs(od.Bids)
			asks := parseStringPairs(od.Asks)
			q = qs.update(func(q *consensus.Quote) {
				q.Orderbook = &consensus.Orderbook{Bids: bids, Asks: asks}
				bd, ad := computeDepths(bids, asks, q.BestBid, q.BestAsk)
				q.BidDepth1Pct = bd
				q.AskDepth1Pct = ad
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			// Don't publish on orderbook-only updates.
		}

		if published && q.BestBid > 0 && q.BestAsk > 0 {
			if pubErr := a.pub.Publish(ctx, q); pubErr != nil && ctx.Err() == nil {
				log.Printf("bybit: publish %s: %v", canSym, pubErr)
			}
		}
	}
}

func (a *BybitAdapter) publishStale(ctx context.Context, states map[string]*quoteState) {
	now := time.Now().UnixMilli()
	for _, qs := range states {
		q := qs.update(func(q *consensus.Quote) {
			q.TsMs = now
			q.FeedHealth.WsConnected = false
			q.FeedHealth.LastMsgTsMs = now
		})
		if q.BestBid > 0 {
			if err := a.pub.Publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("bybit: publish stale: %v", err)
			}
		}
	}
}
