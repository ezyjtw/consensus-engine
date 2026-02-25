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

// GateAdapter streams normalised quotes from the Gate.io futures V4 WebSocket.
type GateAdapter struct {
	venueCfg VenueConfig
	pub      *Publisher
	tenantID string
	backoffs []int
	depth    int
}

func NewGateAdapter(venueCfg VenueConfig, pub *Publisher, tenantID string, backoffs []int, depth int) *GateAdapter {
	return &GateAdapter{
		venueCfg: venueCfg,
		pub:      pub,
		tenantID: tenantID,
		backoffs: backoffs,
		depth:    depth,
	}
}

func (a *GateAdapter) Run(ctx context.Context) {
	runWithBackoff(ctx, "gate", a.backoffs, a.connect)
}

// ── Gate message types ───────────────────────────────────────────────────

type gateSubMsg struct {
	Time    int64    `json:"time"`
	Channel string   `json:"channel"`
	Event   string   `json:"event"`
	Payload []string `json:"payload"`
}

type gateEnvelope struct {
	Time    int64           `json:"time"`
	Channel string          `json:"channel"`
	Event   string          `json:"event"`
	Result  json.RawMessage `json:"result"`
}

type gateBookTicker struct {
	Contract string `json:"s"`
	BidPrice string `json:"b"` // best bid price
	BidSize  int64  `json:"B"` // best bid size
	AskPrice string `json:"a"` // best ask price
	AskSize  int64  `json:"A"` // best ask size
	T        int64  `json:"t"` // timestamp ms
}

type gateOrderBook struct {
	Contract string          `json:"contract"`
	Bids     []gateBookLevel `json:"bids"`
	Asks     []gateBookLevel `json:"asks"`
	T        int64           `json:"t"`
}

type gateBookLevel struct {
	Price string `json:"p"`
	Size  int64  `json:"s"`
}

func (a *GateAdapter) connect(ctx context.Context) error {
	log.Printf("gate: connecting to %s", a.venueCfg.WsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.venueCfg.WsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	cw := newConnWriter(conn)

	now := time.Now().Unix()

	// Subscribe to book_ticker and order_book for each symbol.
	tickerPayload := make([]string, 0, len(a.venueCfg.Symbols))
	depthPayload := make([]string, 0, len(a.venueCfg.Symbols))
	for _, sym := range a.venueCfg.Symbols {
		tickerPayload = append(tickerPayload, sym)
		depthPayload = append(depthPayload, sym)
	}

	tickerSub := gateSubMsg{Time: now, Channel: "futures.book_ticker", Event: "subscribe", Payload: tickerPayload}
	if err := cw.WriteJSON(tickerSub); err != nil {
		return fmt.Errorf("subscribe book_ticker: %w", err)
	}

	depthSub := gateSubMsg{Time: now, Channel: "futures.order_book", Event: "subscribe", Payload: depthPayload}
	if err := cw.WriteJSON(depthSub); err != nil {
		return fmt.Errorf("subscribe order_book: %w", err)
	}

	// Build quote state per canonical symbol.
	states := make(map[string]*quoteState)
	symLookup := make(map[string]string) // exchange sym → canonical
	for exSym, canSym := range a.venueCfg.SymbolMap {
		states[canSym] = &quoteState{
			quote: consensus.Quote{
				TenantID:    a.tenantID,
				Venue:       "gate",
				Symbol:      consensus.Symbol(canSym),
				FeeBpsTaker: a.venueCfg.FeeBpsTaker,
				FeedHealth:  consensus.FeedHealth{WsConnected: true},
			},
		}
		symLookup[exSym] = canSym
	}

	// Ping every 15 seconds per Gate docs.
	pingTicker := time.NewTicker(15 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				ping := gateSubMsg{Time: time.Now().Unix(), Channel: "futures.ping"}
				if err := cw.WriteJSON(ping); err != nil {
					return
				}
			}
		}
	}()

	log.Printf("gate: connected, tracking %d symbols", len(states))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			a.publishStale(ctx, states)
			return fmt.Errorf("read: %w", err)
		}

		var env gateEnvelope
		if err := json.Unmarshal(raw, &env); err != nil {
			continue
		}

		// Skip subscription confirmations and pong responses.
		if env.Event == "subscribe" || env.Channel == "futures.pong" || env.Result == nil {
			continue
		}

		nowMs := time.Now().UnixMilli()

		switch env.Channel {
		case "futures.book_ticker":
			var bt gateBookTicker
			if err := json.Unmarshal(env.Result, &bt); err != nil {
				continue
			}
			canSym, ok := symLookup[bt.Contract]
			if !ok {
				// Try looking up by the 's' field.
				canSym, ok = symLookup[strings.ReplaceAll(bt.Contract, "_", "-")]
				if !ok {
					continue
				}
			}
			qs, ok := states[canSym]
			if !ok {
				continue
			}
			bid, _ := strconv.ParseFloat(bt.BidPrice, 64)
			ask, _ := strconv.ParseFloat(bt.AskPrice, 64)
			q := qs.update(func(q *consensus.Quote) {
				q.BestBid = bid
				q.BestAsk = ask
				q.TsMs = nowMs
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = nowMs
			})
			if q.BestBid > 0 && q.BestAsk > 0 {
				if pubErr := a.pub.Publish(ctx, q); pubErr != nil && ctx.Err() == nil {
					log.Printf("gate: publish %s: %v", canSym, pubErr)
				}
			}

		case "futures.order_book":
			var ob gateOrderBook
			if err := json.Unmarshal(env.Result, &ob); err != nil {
				continue
			}
			contract := ob.Contract
			canSym, ok := symLookup[contract]
			if !ok {
				continue
			}
			qs, ok := states[canSym]
			if !ok {
				continue
			}
			bids := make([][2]float64, 0, len(ob.Bids))
			for _, lv := range ob.Bids {
				p, _ := strconv.ParseFloat(lv.Price, 64)
				bids = append(bids, [2]float64{p, float64(lv.Size)})
			}
			asks := make([][2]float64, 0, len(ob.Asks))
			for _, lv := range ob.Asks {
				p, _ := strconv.ParseFloat(lv.Price, 64)
				asks = append(asks, [2]float64{p, float64(lv.Size)})
			}
			qs.update(func(q *consensus.Quote) {
				q.Orderbook = &consensus.Orderbook{Bids: bids, Asks: asks}
				bd, ad := computeDepths(bids, asks, q.BestBid, q.BestAsk)
				q.BidDepth1Pct = bd
				q.AskDepth1Pct = ad
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = nowMs
			})
		}
	}
}

func (a *GateAdapter) publishStale(ctx context.Context, states map[string]*quoteState) {
	now := time.Now().UnixMilli()
	for _, qs := range states {
		q := qs.update(func(q *consensus.Quote) {
			q.TsMs = now
			q.FeedHealth.WsConnected = false
			q.FeedHealth.LastMsgTsMs = now
		})
		if q.BestBid > 0 {
			if err := a.pub.Publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("gate: publish stale: %v", err)
			}
		}
	}
}
