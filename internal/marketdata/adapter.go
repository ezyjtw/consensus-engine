package marketdata

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// connWriter serialises WebSocket writes behind a mutex. gorilla/websocket
// does not support concurrent writers, so any goroutine that writes (ping,
// heartbeat response) must go through this wrapper.
type connWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func newConnWriter(conn *websocket.Conn) *connWriter {
	return &connWriter{conn: conn}
}

func (cw *connWriter) WriteMessage(messageType int, data []byte) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.conn.WriteMessage(messageType, data)
}

func (cw *connWriter) WriteJSON(v interface{}) error {
	cw.mu.Lock()
	defer cw.mu.Unlock()
	return cw.conn.WriteJSON(v)
}

// quoteState is a mutex-guarded partial Quote that each adapter builds up
// incrementally as different WebSocket channels deliver their fields.
type quoteState struct {
	mu    sync.Mutex
	quote consensus.Quote
}

// update calls fn under the lock and returns a snapshot of the updated quote.
func (qs *quoteState) update(fn func(q *consensus.Quote)) consensus.Quote {
	qs.mu.Lock()
	defer qs.mu.Unlock()
	fn(&qs.quote)
	return qs.quote
}

// computeDepths sums notional (price × quantity) available within 1% of the
// best bid / ask price. Returns values in quote currency (e.g. USD).
func computeDepths(bids, asks [][2]float64, bestBid, bestAsk float64) (bidDepth1Pct, askDepth1Pct float64) {
	if bestBid > 0 {
		floor := bestBid * 0.99
		for _, lv := range bids {
			if lv[0] >= floor {
				bidDepth1Pct += lv[0] * lv[1]
			}
		}
	}
	if bestAsk > 0 {
		ceil := bestAsk * 1.01
		for _, lv := range asks {
			if lv[0] <= ceil {
				askDepth1Pct += lv[0] * lv[1]
			}
		}
	}
	return
}

// runWithBackoff calls fn in a loop, applying exponential backoff on failure.
// It returns only when ctx is cancelled.
func runWithBackoff(ctx context.Context, name string, backoffs []int, fn func(ctx context.Context) error) {
	attempt := 0
	for {
		err := fn(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("%s: disconnected: %v", name, err)
		}
		idx := attempt
		if idx >= len(backoffs) {
			idx = len(backoffs) - 1
		}
		delay := time.Duration(backoffs[idx]) * time.Millisecond
		log.Printf("%s: reconnecting in %v (attempt %d)", name, delay, attempt+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		attempt++
	}
}
