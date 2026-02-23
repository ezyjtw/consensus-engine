package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// posEntry is the value stored in PaperExecutor.positions.
// Defined at package scope so type assertions work correctly across methods.
type posEntry struct {
	mu         sync.Mutex
	qty        float64
	entryPrice float64
	notional   float64
}

// PriceCache holds the latest consensus mid price per symbol.
type PriceCache struct {
	mu    sync.RWMutex
	mids  map[string]float64
	bands map[string][2]float64
}

func NewPriceCache() *PriceCache {
	return &PriceCache{
		mids:  make(map[string]float64),
		bands: make(map[string][2]float64),
	}
}

func (pc *PriceCache) Update(u consensus.ConsensusUpdate) {
	pc.mu.Lock()
	defer pc.mu.Unlock()
	sym := string(u.Symbol)
	pc.mids[sym] = u.Consensus.Mid
	pc.bands[sym] = [2]float64{u.Consensus.BandLow, u.Consensus.BandHigh}
}

func (pc *PriceCache) Mid(symbol string) (float64, bool) {
	pc.mu.RLock()
	defer pc.mu.RUnlock()
	v, ok := pc.mids[symbol]
	return v, ok
}

// PaperExecutor simulates order fills without touching real exchange APIs.
type PaperExecutor struct {
	cfg        *Config
	priceCache *PriceCache
	// positions: "venue:symbol:market" → *posEntry
	positions sync.Map
	// orderTs tracks recent order timestamps for rate limiting.
	orderMu sync.Mutex
	orderTs []int64
}

func NewPaperExecutor(cfg *Config, cache *PriceCache) *PaperExecutor {
	return &PaperExecutor{cfg: cfg, priceCache: cache}
}

// Execute simulates the fill of all legs in an approved intent.
// Returns execution events for each leg and a single SimulatedFill summary.
func (e *PaperExecutor) Execute(ctx context.Context, intent arb.TradeIntent) ([]ExecutionEvent, *SimulatedFill) {
	now := time.Now().UnixMilli()

	// Rate limit: reject if order rate exceeds MaxOrdersPerMinute.
	if e.cfg.MaxOrdersPerMinute > 0 && !e.tryAcquireOrderSlot(now) {
		log.Printf("paper: rate limited intent %s (max %d orders/min)",
			intent.IntentID, e.cfg.MaxOrdersPerMinute)
		return []ExecutionEvent{{
			EventType: "ORDER_REJECTED",
			IntentID:  intent.IntentID,
			Strategy:  intent.Strategy,
			Symbol:    intent.Symbol,
			TsMs:      now,
			TenantID:  e.cfg.TenantID,
			Mode:      e.cfg.TradingMode,
		}}, nil
	}

	// Check intent has not expired.
	if now > intent.ExpiresMs {
		log.Printf("paper: intent %s expired (ttl=%dms)", intent.IntentID,
			intent.ExpiresMs-intent.TsMs)
		return nil, &SimulatedFill{
			IntentID:      intent.IntentID,
			Strategy:      intent.Strategy,
			Symbol:        intent.Symbol,
			TsSignalMs:    intent.TsMs,
			IntentExpired: true,
			Mode:          e.cfg.TradingMode,
			TenantID:      e.cfg.TenantID,
		}
	}

	mid, ok := e.priceCache.Mid(intent.Symbol)
	if !ok || mid == 0 {
		log.Printf("paper: no consensus price for %s — skipping", intent.Symbol)
		return nil, nil
	}

	latencyMs := e.cfg.SimLatencyMs
	fillTs := now + latencyMs

	var events []ExecutionEvent
	var buyPrice, sellPrice float64
	var totalFees float64
	adverseSelection := false

	slipBps := e.cfg.SimSlippageBps

	for i, leg := range intent.Legs {
		var fillPrice float64
		if leg.Action == "BUY" {
			fillPrice = mid * (1 + slipBps/10000)
			buyPrice = fillPrice
		} else {
			fillPrice = mid * (1 - slipBps/10000)
			sellPrice = fillPrice
		}

		// Enforce price limit.
		if leg.PriceLimit > 0 {
			if leg.Action == "BUY" && fillPrice > leg.PriceLimit {
				fillPrice = leg.PriceLimit
			}
			if leg.Action == "SELL" && fillPrice < leg.PriceLimit {
				fillPrice = leg.PriceLimit
			}
		}

		slippageActual := math.Abs(fillPrice-mid) / mid * 10000
		feesUSD := leg.NotionalUSD * 4 / 10000 // 4bps taker
		totalFees += feesUSD

		if slippageActual > e.cfg.AdverseSelBps {
			adverseSelection = true
		}

		market := leg.Market
		if market == "" {
			market = "PERP"
		}

		events = append(events, ExecutionEvent{
			EventType:             "ORDER_FILLED",
			IntentID:              intent.IntentID,
			LegIndex:              i,
			Venue:                 leg.Venue,
			Symbol:                intent.Symbol,
			Action:                leg.Action,
			Strategy:              intent.Strategy,
			Market:                market,
			RequestedNotionalUSD:  leg.NotionalUSD,
			FilledNotionalUSD:     leg.NotionalUSD,
			FilledPrice:           fillPrice,
			SlippageBpsActual:     slippageActual,
			SlippageBpsAllowed:    leg.MaxSlippageBps,
			FeesUSDActual:         feesUSD,
			TsMs:                  fillTs,
			LatencySignalToFillMs: latencyMs,
			TenantID:              e.cfg.TenantID,
			Mode:                  e.cfg.TradingMode,
		})

		// Update virtual position.
		posKey := fmt.Sprintf("%s:%s:%s", leg.Venue, intent.Symbol, market)
		qty := leg.NotionalUSD / fillPrice
		if leg.Action == "SELL" {
			qty = -qty
		}
		e.updatePosition(posKey, qty, fillPrice, leg.NotionalUSD)
	}

	netPnL := 0.0
	if buyPrice > 0 && sellPrice > 0 {
		notional := intent.Legs[0].NotionalUSD
		netPnL = (sellPrice-buyPrice)/mid*notional - totalFees
	}

	edgeAtSignal := intent.Expected.EdgeBpsNet
	edgeAtFill := edgeAtSignal
	if adverseSelection {
		edgeAtFill = edgeAtSignal * 0.8
	}

	// Build per-leg summary for notional accounting by the allocator.
	fillLegs := make([]FillLeg, len(events))
	for i, ev := range events {
		fillLegs[i] = FillLeg{
			Venue:             ev.Venue,
			Action:            ev.Action,
			FilledNotionalUSD: ev.FilledNotionalUSD,
			FilledPrice:       ev.FilledPrice,
		}
	}

	fill := &SimulatedFill{
		IntentID:                 intent.IntentID,
		Strategy:                 intent.Strategy,
		Symbol:                   intent.Symbol,
		Legs:                     fillLegs,
		TsSignalMs:               intent.TsMs,
		TsFillSimulatedMs:        fillTs,
		LatencyMs:                latencyMs,
		EdgeAtSignalBps:          edgeAtSignal,
		EdgeAtFillBps:            edgeAtFill,
		EdgeCapturedBps:          edgeAtFill,
		AdverseSelectionOccurred: adverseSelection,
		FillPriceBuy:             buyPrice,
		FillPriceSell:            sellPrice,
		FeesAssumedUSD:           totalFees,
		SlippageAssumedBps:       slipBps,
		NetPnLUSD:                netPnL,
		IntentExpired:            false,
		Mode:                     e.cfg.TradingMode,
		TenantID:                 e.cfg.TenantID,
	}

	return events, fill
}

func (e *PaperExecutor) updatePosition(key string, qty, price, notional float64) {
	actual, _ := e.positions.LoadOrStore(key, &posEntry{})
	pos := actual.(*posEntry) // safe: all stored values are *posEntry (package-level type)
	pos.mu.Lock()
	pos.qty += qty
	pos.notional += notional
	if pos.qty != 0 {
		pos.entryPrice = price
	}
	pos.mu.Unlock()
}

// PositionJSON returns all current virtual positions as a serialisable map.
func (e *PaperExecutor) PositionJSON() map[string]interface{} {
	out := make(map[string]interface{})
	e.positions.Range(func(k, v interface{}) bool {
		if p, ok := v.(*posEntry); ok { // same package-level type — assertion always succeeds
			p.mu.Lock()
			out[k.(string)] = map[string]float64{
				"qty":         p.qty,
				"entry_price": p.entryPrice,
				"notional":    p.notional,
			}
			p.mu.Unlock()
		}
		return true
	})
	return out
}

// tryAcquireOrderSlot checks whether another order is allowed under the
// per-minute rate limit. Returns true and records the timestamp on success.
func (e *PaperExecutor) tryAcquireOrderSlot(nowMs int64) bool {
	e.orderMu.Lock()
	defer e.orderMu.Unlock()
	cutoff := nowMs - 60_000
	// Prune entries older than 1 minute.
	i := 0
	for i < len(e.orderTs) && e.orderTs[i] < cutoff {
		i++
	}
	e.orderTs = e.orderTs[i:]
	if len(e.orderTs) >= e.cfg.MaxOrdersPerMinute {
		return false
	}
	e.orderTs = append(e.orderTs, nowMs)
	return true
}

// MarshalFill serialises a SimulatedFill to JSON bytes.
func MarshalFill(f *SimulatedFill) ([]byte, error) {
	return json.Marshal(f)
}
