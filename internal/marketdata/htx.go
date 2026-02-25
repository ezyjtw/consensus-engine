package marketdata

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// HTXAdapter streams normalised quotes from the HTX (formerly Huobi) linear
// swap WebSocket. HTX sends gzip-compressed binary frames that must be
// decompressed before parsing.
type HTXAdapter struct {
	venueCfg VenueConfig
	pub      *Publisher
	tenantID string
	backoffs []int
	depth    int
}

func NewHTXAdapter(venueCfg VenueConfig, pub *Publisher, tenantID string, backoffs []int, depth int) *HTXAdapter {
	return &HTXAdapter{
		venueCfg: venueCfg,
		pub:      pub,
		tenantID: tenantID,
		backoffs: backoffs,
		depth:    depth,
	}
}

func (a *HTXAdapter) Run(ctx context.Context) {
	runWithBackoff(ctx, "htx", a.backoffs, a.connect)
}

// ── HTX message types ────────────────────────────────────────────────────

type htxSubMsg struct {
	Sub string `json:"sub"`
	ID  string `json:"id"`
}

type htxPong struct {
	Pong int64 `json:"pong"`
}

type htxEnvelope struct {
	Ping int64           `json:"ping,omitempty"`
	Ch   string          `json:"ch,omitempty"`
	Tick json.RawMessage `json:"tick,omitempty"`
}

type htxBBO struct {
	Bid  [2]json.Number `json:"bid"` // [price, qty]
	Ask  [2]json.Number `json:"ask"`
	MrID int64          `json:"mrid"`
	Ts   int64          `json:"ts"`
}

type htxDepth struct {
	Bids [][]json.Number `json:"bids"` // [[price, qty], ...]
	Asks [][]json.Number `json:"asks"`
	Ts   int64           `json:"ts"`
}

// decompressGzip decompresses a gzip-encoded payload.
func decompressGzip(data []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func (a *HTXAdapter) connect(ctx context.Context) error {
	log.Printf("htx: connecting to %s", a.venueCfg.WsURL)
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

	// Subscribe to BBO (best bid/offer) and depth for each symbol.
	for _, sym := range a.venueCfg.Symbols {
		s := strings.ToLower(strings.ReplaceAll(sym, "-", "_"))

		bboSub := htxSubMsg{Sub: fmt.Sprintf("market.%s.bbo", s), ID: "bbo_" + s}
		if err := cw.WriteJSON(bboSub); err != nil {
			return fmt.Errorf("subscribe bbo %s: %w", sym, err)
		}

		depthSub := htxSubMsg{Sub: fmt.Sprintf("market.%s.depth.step0", s), ID: "depth_" + s}
		if err := cw.WriteJSON(depthSub); err != nil {
			return fmt.Errorf("subscribe depth %s: %w", sym, err)
		}
	}

	// Build quote state per canonical symbol.
	states := make(map[string]*quoteState)
	symLookup := make(map[string]string) // lowercase exchange sym (btc_usdt) → canonical
	for exSym, canSym := range a.venueCfg.SymbolMap {
		states[canSym] = &quoteState{
			quote: consensus.Quote{
				TenantID:    a.tenantID,
				Venue:       "htx",
				Symbol:      consensus.Symbol(canSym),
				FeeBpsTaker: a.venueCfg.FeeBpsTaker,
				FeedHealth:  consensus.FeedHealth{WsConnected: true},
			},
		}
		lower := strings.ToLower(strings.ReplaceAll(exSym, "-", "_"))
		symLookup[lower] = canSym
	}

	log.Printf("htx: connected, tracking %d symbols", len(states))

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			a.publishStale(ctx, states)
			return fmt.Errorf("read: %w", err)
		}

		// HTX sends gzip-compressed binary frames.
		decoded, err := decompressGzip(raw)
		if err != nil {
			continue
		}

		var env htxEnvelope
		if err := json.Unmarshal(decoded, &env); err != nil {
			continue
		}

		// Respond to server heartbeat pings.
		if env.Ping > 0 {
			pong := htxPong{Pong: env.Ping}
			if err := cw.WriteJSON(pong); err != nil {
				return fmt.Errorf("pong: %w", err)
			}
			continue
		}

		if env.Ch == "" || env.Tick == nil {
			continue
		}

		// Parse channel: "market.btc_usdt.bbo" or "market.btc_usdt.depth.step0"
		parts := strings.Split(env.Ch, ".")
		if len(parts) < 3 || parts[0] != "market" {
			continue
		}
		exSym := parts[1]
		chType := parts[2]

		canSym, ok := symLookup[exSym]
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

		switch chType {
		case "bbo":
			var bbo htxBBO
			if err := json.Unmarshal(env.Tick, &bbo); err != nil {
				continue
			}
			bid, _ := bbo.Bid[0].Float64()
			ask, _ := bbo.Ask[0].Float64()
			q = qs.update(func(q *consensus.Quote) {
				q.BestBid = bid
				q.BestAsk = ask
				q.TsMs = now
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
			published = true

		case "depth":
			var d htxDepth
			if err := json.Unmarshal(env.Tick, &d); err != nil {
				continue
			}
			bids := parseJSONNumberPairs(d.Bids)
			asks := parseJSONNumberPairs(d.Asks)
			q = qs.update(func(q *consensus.Quote) {
				q.Orderbook = &consensus.Orderbook{Bids: bids, Asks: asks}
				bd, ad := computeDepths(bids, asks, q.BestBid, q.BestAsk)
				q.BidDepth1Pct = bd
				q.AskDepth1Pct = ad
				q.FeedHealth.WsConnected = true
				q.FeedHealth.LastMsgTsMs = now
			})
		}

		if published && q.BestBid > 0 && q.BestAsk > 0 {
			if pubErr := a.pub.Publish(ctx, q); pubErr != nil && ctx.Err() == nil {
				log.Printf("htx: publish %s: %v", canSym, pubErr)
			}
		}
	}
}

// parseJSONNumberPairs converts [][]json.Number to [][2]float64.
func parseJSONNumberPairs(raw [][]json.Number) [][2]float64 {
	out := make([][2]float64, 0, len(raw))
	for _, pair := range raw {
		if len(pair) < 2 {
			continue
		}
		p, _ := strconv.ParseFloat(pair[0].String(), 64)
		q, _ := strconv.ParseFloat(pair[1].String(), 64)
		out = append(out, [2]float64{p, q})
	}
	return out
}

func (a *HTXAdapter) publishStale(ctx context.Context, states map[string]*quoteState) {
	now := time.Now().UnixMilli()
	for _, qs := range states {
		q := qs.update(func(q *consensus.Quote) {
			q.TsMs = now
			q.FeedHealth.WsConnected = false
			q.FeedHealth.LastMsgTsMs = now
		})
		if q.BestBid > 0 {
			if err := a.pub.Publish(ctx, q); err != nil && ctx.Err() == nil {
				log.Printf("htx: publish stale: %v", err)
			}
		}
	}
}
