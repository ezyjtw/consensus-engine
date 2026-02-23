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

// OKXAdapter streams quotes from the OKX V5 public WebSocket.
// Channels used per symbol: tickers, mark-price, index-tickers, funding-rate, books5.
type OKXAdapter struct {
	venueCfg VenueConfig
	pub      *Publisher
	tenantID string
	backoffs []int
}

func NewOKXAdapter(venueCfg VenueConfig, pub *Publisher, tenantID string, backoffs []int) *OKXAdapter {
	return &OKXAdapter{
		venueCfg: venueCfg,
		pub:      pub,
		tenantID: tenantID,
		backoffs: backoffs,
	}
}

func (a *OKXAdapter) Run(ctx context.Context) {
	runWithBackoff(ctx, "okx", a.backoffs, a.connect)
}

// ── OKX message types ─────────────────────────────────────────────────────

type okxSubscribeMsg struct {
	Op   string         `json:"op"`
	Args []okxSubscribeArg `json:"args"`
}

type okxSubscribeArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

type okxPushMsg struct {
	Arg  okxPushArg        `json:"arg"`
	Data []json.RawMessage `json:"data"`
}

type okxPushArg struct {
	Channel string `json:"channel"`
	InstID  string `json:"instId"`
}

type okxTicker struct {
	InstID  string `json:"instId"`
	BidPx   string `json:"bidPx"`
	AskPx   string `json:"askPx"`
	Ts      string `json:"ts"`
}

type okxMarkPrice struct {
	InstID string `json:"instId"`
	MarkPx string `json:"markPx"`
	Ts     string `json:"ts"`
}

type okxIndexTicker struct {
	InstID string `json:"instId"`
	IdxPx  string `json:"idxPx"`
	Ts     string `json:"ts"`
}

type okxFundingRate struct {
	InstID      string `json:"instId"`
	FundingRate string `json:"fundingRate"`
}

type okxBook5 struct {
	Bids [][]string `json:"bids"` // [price, qty, ?, orders]
	Asks [][]string `json:"asks"`
	Ts   string     `json:"ts"`
}

// okxIndexInstID derives the OKX index instId from a SWAP instId.
// "BTC-USDT-SWAP" → "BTC-USD", "ETH-USDT-SWAP" → "ETH-USD"
func okxIndexInstID(swapInstID string) string {
	base := strings.TrimSuffix(swapInstID, "-USDT-SWAP")
	base = strings.TrimSuffix(base, "-USDC-SWAP")
	return base + "-USD"
}

func (a *OKXAdapter) connect(ctx context.Context) error {
	log.Printf("okx: connecting to %s", a.venueCfg.WsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.venueCfg.WsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// Build subscription args for all symbols.
	var args []okxSubscribeArg
	for _, sym := range a.venueCfg.Symbols {
		args = append(args,
			okxSubscribeArg{Channel: "tickers", InstID: sym},
			okxSubscribeArg{Channel: "mark-price", InstID: sym},
			okxSubscribeArg{Channel: "index-tickers", InstID: okxIndexInstID(sym)},
			okxSubscribeArg{Channel: "funding-rate", InstID: sym},
			okxSubscribeArg{Channel: "books5", InstID: sym},
		)
	}
	subMsg := okxSubscribeMsg{Op: "subscribe", Args: args}
	if err := conn.WriteJSON(subMsg); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Build quote state per canonical symbol.
	// OKX tickers use SWAP instId; index-tickers use index instId.
	states := make(map[string]*quoteState) // canonical symbol → state
	// maps exchange instId → canonical symbol (covers both SWAP and index IDs)
	instToCanon := make(map[string]string)
	for exSym, canSym := range a.venueCfg.SymbolMap {
		states[canSym] = &quoteState{
			quote: consensus.Quote{
				TenantID:    a.tenantID,
				Venue:       "okx",
				Symbol:      consensus.Symbol(canSym),
				FeeBpsTaker: a.venueCfg.FeeBpsTaker,
				FeedHealth:  consensus.FeedHealth{WsConnected: true},
			},
		}
		instToCanon[exSym] = canSym
		instToCanon[okxIndexInstID(exSym)] = canSym
	}

	// Ping ticker: OKX requires a ping every 25 seconds.
	pingTicker := time.NewTicker(25 * time.Second)
	defer pingTicker.Stop()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pingTicker.C:
				if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
					return
				}
			}
		}
	}()

	log.Printf("okx: connected, tracking %d symbols", len(states))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			a.publishStale(ctx, states)
			return fmt.Errorf("read: %w", err)
		}

		// OKX server sends plain "pong" text frames.
		if string(raw) == "pong" {
			continue
		}

		var push okxPushMsg
		if err := json.Unmarshal(raw, &push); err != nil {
			continue
		}
		if len(push.Data) == 0 {
			continue
		}

		canSym, ok := instToCanon[push.Arg.InstID]
		if !ok {
			continue
		}
		qs, ok := states[canSym]
		if !ok {
			continue
		}

		now := time.Now().UnixMilli()
		var q consensus.Quote
		published := false

		switch push.Arg.Channel {
		case "tickers":
			var t okxTicker
			if err := json.Unmarshal(push.Data[0], &t); err != nil {
				continue
			}
			bid, _ := strconv.ParseFloat(t.BidPx, 64)
			ask, _ := strconv.ParseFloat(t.AskPx, 64)
			q = qs.update(func(q *consensus.Quote) {
				q.BestBid = bid
				q.BestAsk = ask
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			published = true

		case "mark-price":
			var m okxMarkPrice
			if err := json.Unmarshal(push.Data[0], &m); err != nil {
				continue
			}
			mark, _ := strconv.ParseFloat(m.MarkPx, 64)
			q = qs.update(func(q *consensus.Quote) {
				q.Mark = mark
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			published = true

		case "index-tickers":
			var idx okxIndexTicker
			if err := json.Unmarshal(push.Data[0], &idx); err != nil {
				continue
			}
			idxPx, _ := strconv.ParseFloat(idx.IdxPx, 64)
			q = qs.update(func(q *consensus.Quote) {
				q.Index = idxPx
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			// Don't publish on index-only update; wait for ticker.

		case "funding-rate":
			var fr okxFundingRate
			if err := json.Unmarshal(push.Data[0], &fr); err != nil {
				continue
			}
			rate, _ := strconv.ParseFloat(fr.FundingRate, 64)
			qs.update(func(q *consensus.Quote) {
				q.FundingRate = rate
			})
			// Funding rate updates are infrequent; no publish triggered.

		case "books5":
			var b okxBook5
			if err := json.Unmarshal(push.Data[0], &b); err != nil {
				continue
			}
			bids := parseOKXBookLevels(b.Bids)
			asks := parseOKXBookLevels(b.Asks)
			q = qs.update(func(q *consensus.Quote) {
				q.Orderbook = &consensus.Orderbook{Bids: bids, Asks: asks}
				bd, ad := computeDepths(bids, asks, q.BestBid, q.BestAsk)
				q.BidDepth1Pct = bd
				q.AskDepth1Pct = ad
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			// Don't publish on depth-only update.
		}

		if published && q.BestBid > 0 && q.BestAsk > 0 {
			if pubErr := a.pub.Publish(ctx, q); pubErr != nil && ctx.Err() == nil {
				log.Printf("okx: publish %s: %v", canSym, pubErr)
			}
		}
	}
}

// parseOKXBookLevels converts OKX book entries (4-element string arrays) to [][2]float64.
func parseOKXBookLevels(raw [][]string) [][2]float64 {
	out := make([][2]float64, 0, len(raw))
	for _, entry := range raw {
		if len(entry) < 2 {
			continue
		}
		p, _ := strconv.ParseFloat(entry[0], 64)
		q, _ := strconv.ParseFloat(entry[1], 64)
		out = append(out, [2]float64{p, q})
	}
	return out
}

func (a *OKXAdapter) publishStale(ctx context.Context, states map[string]*quoteState) {
	now := time.Now().UnixMilli()
	for _, qs := range states {
		q := qs.update(func(q *consensus.Quote) {
			q.TsMs = now
			q.FeedHealth.WsConnected = false
			q.FeedHealth.LastMsgTsMs = now
		})
		if q.BestBid > 0 {
			if err := a.pub.Publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("okx: publish stale: %v", err)
			}
		}
	}
}
