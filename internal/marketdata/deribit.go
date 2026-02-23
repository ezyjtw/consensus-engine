package marketdata

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// DeribitAdapter streams quotes from the Deribit JSON-RPC WebSocket API.
// It subscribes to ticker and book channels for each configured instrument.
type DeribitAdapter struct {
	venueCfg VenueConfig
	pub      *Publisher
	tenantID string
	backoffs []int
	depth    int
}

func NewDeribitAdapter(venueCfg VenueConfig, pub *Publisher, tenantID string, backoffs []int, depth int) *DeribitAdapter {
	return &DeribitAdapter{
		venueCfg: venueCfg,
		pub:      pub,
		tenantID: tenantID,
		backoffs: backoffs,
		depth:    depth,
	}
}

func (a *DeribitAdapter) Run(ctx context.Context) {
	runWithBackoff(ctx, "deribit", a.backoffs, a.connect)
}

// ── Deribit message types ─────────────────────────────────────────────────

type deribitRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type deribitSubscribeParams struct {
	Channels []string `json:"channels"`
}

type deribitHeartbeatParams struct {
	Interval int `json:"interval"`
}

type deribitMessage struct {
	Jsonrpc string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      *int            `json:"id"`
	Result  json.RawMessage `json:"result"`
}

type deribitSubscriptionParams struct {
	Channel string          `json:"channel"`
	Data    json.RawMessage `json:"data"`
}

type deribitHeartbeatPushParams struct {
	Type string `json:"type"`
}

type deribitTickerData struct {
	BestBidPrice   float64 `json:"best_bid_price"`
	BestAskPrice   float64 `json:"best_ask_price"`
	MarkPrice      float64 `json:"mark_price"`
	IndexPrice     float64 `json:"index_price"`
	CurrentFunding float64 `json:"current_funding"`
	Funding8h      float64 `json:"funding_8h"`
	Timestamp      int64   `json:"timestamp"`
}

type deribitBookEntry = [3]interface{} // ["new"/"change"/"delete", price, amount]

type deribitBookData struct {
	Timestamp      int64             `json:"timestamp"`
	InstrumentName string            `json:"instrument_name"`
	Bids           []json.RawMessage `json:"bids"`
	Asks           []json.RawMessage `json:"asks"`
}

func (a *DeribitAdapter) connect(ctx context.Context) error {
	log.Printf("deribit: connecting to %s", a.venueCfg.WsURL)
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.venueCfg.WsURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		conn.Close()
	}()

	// Enable server-side heartbeats so disconnects are detected quickly.
	setHeartbeat := deribitRequest{
		Jsonrpc: "2.0",
		ID:      1,
		Method:  "public/set_heartbeat",
		Params:  deribitHeartbeatParams{Interval: 30},
	}
	if err := conn.WriteJSON(setHeartbeat); err != nil {
		return fmt.Errorf("set_heartbeat: %w", err)
	}

	// Build channel list: ticker.{INSTR}.raw and book.{INSTR}.{N}.none.20ms
	var channels []string
	for _, sym := range a.venueCfg.Symbols {
		channels = append(channels,
			fmt.Sprintf("ticker.%s.raw", sym),
			fmt.Sprintf("book.%s.%d.none.20ms", sym, a.depth),
		)
	}
	subReq := deribitRequest{
		Jsonrpc: "2.0",
		ID:      2,
		Method:  "public/subscribe",
		Params:  deribitSubscribeParams{Channels: channels},
	}
	if err := conn.WriteJSON(subReq); err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}

	// Build quote state per canonical symbol (keyed by Deribit instrument name).
	states := make(map[string]*quoteState)
	for exSym, canSym := range a.venueCfg.SymbolMap {
		_ = exSym
		states[canSym] = &quoteState{
			quote: consensus.Quote{
				TenantID:    a.tenantID,
				Venue:       "deribit",
				Symbol:      consensus.Symbol(canSym),
				FeeBpsTaker: a.venueCfg.FeeBpsTaker,
				FeedHealth:  consensus.FeedHealth{WsConnected: true},
			},
		}
	}

	log.Printf("deribit: connected, tracking %d symbols", len(states))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			a.publishStale(ctx, states)
			return fmt.Errorf("read: %w", err)
		}

		var msg deribitMessage
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		// Handle heartbeat test requests from the server.
		if msg.Method == "heartbeat" {
			var hp deribitHeartbeatPushParams
			if err := json.Unmarshal(msg.Params, &hp); err == nil && hp.Type == "test_request" {
				resp := deribitRequest{
					Jsonrpc: "2.0",
					ID:      9929,
					Method:  "public/test",
					Params:  map[string]interface{}{},
				}
				if err := conn.WriteJSON(resp); err != nil {
					return fmt.Errorf("heartbeat response: %w", err)
				}
			}
			continue
		}

		if msg.Method != "subscription" {
			continue
		}

		var subParams deribitSubscriptionParams
		if err := json.Unmarshal(msg.Params, &subParams); err != nil {
			continue
		}

		now := time.Now().UnixMilli()

		// Channel format: "ticker.BTC-PERPETUAL.raw" or "book.BTC-PERPETUAL.10.none.20ms"
		channelParts := strings.Split(subParams.Channel, ".")
		if len(channelParts) < 2 {
			continue
		}
		channelType := channelParts[0]

		var instrName string
		switch channelType {
		case "ticker":
			// ticker.{INSTR}.raw → index 1
			instrName = channelParts[1]
		case "book":
			// book.{INSTR}.{depth}.none.{interval} → index 1
			instrName = channelParts[1]
		default:
			continue
		}

		canSym, ok := a.venueCfg.SymbolMap[instrName]
		if !ok {
			continue
		}
		qs, ok := states[canSym]
		if !ok {
			continue
		}

		var q consensus.Quote
		published := false

		switch channelType {
		case "ticker":
			var td deribitTickerData
			if err := json.Unmarshal(subParams.Data, &td); err != nil {
				continue
			}
			q = qs.update(func(q *consensus.Quote) {
				q.BestBid = td.BestBidPrice
				q.BestAsk = td.BestAskPrice
				q.Mark = td.MarkPrice
				q.Index = td.IndexPrice
				q.FundingRate = td.CurrentFunding
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			published = true

		case "book":
			var bd deribitBookData
			if err := json.Unmarshal(subParams.Data, &bd); err != nil {
				continue
			}
			bids := parseDeribitBookLevels(bd.Bids)
			asks := parseDeribitBookLevels(bd.Asks)
			q = qs.update(func(q *consensus.Quote) {
				q.Orderbook = &consensus.Orderbook{Bids: bids, Asks: asks}
				bidD, askD := computeDepths(bids, asks, q.BestBid, q.BestAsk)
				q.BidDepth1Pct = bidD
				q.AskDepth1Pct = askD
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			// Don't publish on book-only update.
		}

		if published && q.BestBid > 0 && q.BestAsk > 0 {
			if pubErr := a.pub.Publish(ctx, q); pubErr != nil && ctx.Err() == nil {
				log.Printf("deribit: publish %s: %v", canSym, pubErr)
			}
		}
	}
}

// parseDeribitBookLevels converts Deribit's ["action", price, amount] triples.
// For snapshot messages every entry has action "new". For incremental updates
// "change" and "delete" are also sent; we use this primarily with the limited
// depth snapshot channel so we replace the whole book each time.
func parseDeribitBookLevels(raw []json.RawMessage) [][2]float64 {
	out := make([][2]float64, 0, len(raw))
	for _, entry := range raw {
		var triple [3]json.RawMessage
		if err := json.Unmarshal(entry, &triple); err != nil {
			continue
		}
		var action string
		if err := json.Unmarshal(triple[0], &action); err != nil {
			continue
		}
		if action == "delete" {
			continue
		}
		var price, amount float64
		if err := json.Unmarshal(triple[1], &price); err != nil {
			continue
		}
		if err := json.Unmarshal(triple[2], &amount); err != nil {
			continue
		}
		out = append(out, [2]float64{price, amount})
	}
	return out
}

func (a *DeribitAdapter) publishStale(ctx context.Context, states map[string]*quoteState) {
	now := time.Now().UnixMilli()
	for _, qs := range states {
		q := qs.update(func(q *consensus.Quote) {
			q.TsMs = now
			q.FeedHealth.WsConnected = false
			q.FeedHealth.LastMsgTsMs = now
		})
		if q.BestBid > 0 {
			if err := a.pub.Publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("deribit: publish stale: %v", err)
			}
		}
	}
}
