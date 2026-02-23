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
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// BinanceAdapter streams normalised quotes from Binance Futures WebSocket.
// It uses the combined-stream endpoint so one connection serves all symbols.
type BinanceAdapter struct {
	venueCfg VenueConfig
	pub      *Publisher
	tenantID string
	backoffs []int
	depth    int
}

func NewBinanceAdapter(venueCfg VenueConfig, pub *Publisher, tenantID string, backoffs []int, depth int) *BinanceAdapter {
	return &BinanceAdapter{
		venueCfg: venueCfg,
		pub:      pub,
		tenantID: tenantID,
		backoffs: backoffs,
		depth:    depth,
	}
}

// Run blocks until ctx is cancelled, reconnecting on failure.
func (a *BinanceAdapter) Run(ctx context.Context) {
	runWithBackoff(ctx, "binance", a.backoffs, a.connect)
}

// buildStreamURL constructs the Binance combined-stream URL for all configured
// symbols. Each symbol gets bookTicker, markPrice, and depthN streams.
func (a *BinanceAdapter) buildStreamURL() string {
	var streams []string
	for _, sym := range a.venueCfg.Symbols {
		s := strings.ToLower(sym)
		streams = append(streams,
			s+"@bookTicker",
			s+"@markPrice@1s",
			fmt.Sprintf("%s@depth%d@500ms", s, a.depth),
		)
	}
	base := a.venueCfg.WsURL
	// Normalise base URL: ensure it ends with /stream not /ws.
	if strings.HasSuffix(base, "/ws") {
		base = strings.TrimSuffix(base, "/ws") + "/stream"
	} else if !strings.HasSuffix(base, "/stream") {
		base += "/stream"
	}
	return base + "?streams=" + strings.Join(streams, "/")
}

// ── Binance message types ──────────────────────────────────────────────────

type binanceCombinedMsg struct {
	Stream string          `json:"stream"`
	Data   json.RawMessage `json:"data"`
}

type binanceBookTicker struct {
	Symbol  string `json:"s"`
	BestBid string `json:"b"`
	BestAsk string `json:"a"`
}

type binanceMarkPrice struct {
	Symbol      string `json:"s"`
	MarkPrice   string `json:"p"`
	IndexPrice  string `json:"i"`
	FundingRate string `json:"r"`
	EventTime   int64  `json:"E"`
}

type binanceDepth struct {
	Symbol string     `json:"s"`
	Bids   [][]string `json:"b"`
	Asks   [][]string `json:"a"`
}

// parseStringPairs converts [][]string price/qty pairs to [][2]float64.
func parseStringPairs(raw [][]string) [][2]float64 {
	out := make([][2]float64, 0, len(raw))
	for _, pair := range raw {
		if len(pair) < 2 {
			continue
		}
		p, _ := strconv.ParseFloat(pair[0], 64)
		q, _ := strconv.ParseFloat(pair[1], 64)
		out = append(out, [2]float64{p, q})
	}
	return out
}

func (a *BinanceAdapter) connect(ctx context.Context) error {
	wsURL := a.buildStreamURL()
	log.Printf("binance: connecting to %s", wsURL)

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	// Close conn when ctx is cancelled so ReadMessage unblocks.
	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// Build per-canonical-symbol state.
	states := make(map[string]*quoteState) // canonical symbol → state
	for exSym, canSym := range a.venueCfg.SymbolMap {
		_ = exSym
		states[canSym] = &quoteState{
			quote: consensus.Quote{
				TenantID:    a.tenantID,
				Venue:       "binance",
				Symbol:      consensus.Symbol(canSym),
				FeeBpsTaker: a.venueCfg.FeeBpsTaker,
				FeedHealth:  consensus.FeedHealth{WsConnected: true},
			},
		}
	}

	log.Printf("binance: connected, tracking %d symbols", len(states))

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			// Publish stale quotes to let downstream staleness filters fire.
			a.publishStale(ctx, states)
			return fmt.Errorf("read: %w", err)
		}

		var combined binanceCombinedMsg
		if err := json.Unmarshal(msg, &combined); err != nil {
			continue
		}

		// stream name: "btcusdt@bookTicker", "btcusdt@markPrice@1s", etc.
		parts := strings.SplitN(combined.Stream, "@", 2)
		if len(parts) < 2 {
			continue
		}
		exSym := strings.ToUpper(parts[0])
		streamType := parts[1]

		canSym, ok := a.venueCfg.SymbolMap[exSym]
		if !ok {
			continue
		}
		qs, ok := states[canSym]
		if !ok {
			continue
		}

		now := time.Now().UnixMilli()
		var q consensus.Quote

		switch {
		case streamType == "bookTicker":
			var bt binanceBookTicker
			if err := json.Unmarshal(combined.Data, &bt); err != nil {
				continue
			}
			bid, _ := strconv.ParseFloat(bt.BestBid, 64)
			ask, _ := strconv.ParseFloat(bt.BestAsk, 64)
			q = qs.update(func(q *consensus.Quote) {
				q.BestBid = bid
				q.BestAsk = ask
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})

		case strings.HasPrefix(streamType, "markPrice"):
			var mp binanceMarkPrice
			if err := json.Unmarshal(combined.Data, &mp); err != nil {
				continue
			}
			mark, _ := strconv.ParseFloat(mp.MarkPrice, 64)
			index, _ := strconv.ParseFloat(mp.IndexPrice, 64)
			fr, _ := strconv.ParseFloat(mp.FundingRate, 64)
			q = qs.update(func(q *consensus.Quote) {
				q.Mark = mark
				q.Index = index
				q.FundingRate = fr
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})

		case strings.HasPrefix(streamType, "depth"):
			var d binanceDepth
			if err := json.Unmarshal(combined.Data, &d); err != nil {
				continue
			}
			bids := parseStringPairs(d.Bids)
			asks := parseStringPairs(d.Asks)
			q = qs.update(func(q *consensus.Quote) {
				q.Orderbook = &consensus.Orderbook{Bids: bids, Asks: asks}
				bd, ad := computeDepths(bids, asks, q.BestBid, q.BestAsk)
				q.BidDepth1Pct = bd
				q.AskDepth1Pct = ad
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})

		default:
			continue
		}

		if q.BestBid > 0 && q.BestAsk > 0 {
			if pubErr := a.pub.Publish(ctx, q); pubErr != nil && ctx.Err() == nil {
				log.Printf("binance: publish %s: %v", canSym, pubErr)
			}
		}
	}
}

func (a *BinanceAdapter) publishStale(ctx context.Context, states map[string]*quoteState) {
	now := time.Now().UnixMilli()
	for _, qs := range states {
		q := qs.update(func(q *consensus.Quote) {
			q.TsMs = now
			q.FeedHealth.WsConnected = false
			q.FeedHealth.LastMsgTsMs = now
		})
		if q.BestBid > 0 {
			if err := a.pub.Publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("binance: publish stale: %v", err)
			}
		}
	}
}
