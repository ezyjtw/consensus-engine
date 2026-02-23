package execution

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/yourorg/arbsuite/internal/arb"
	"github.com/yourorg/arbsuite/internal/consensus"
)

// PriceCache holds the latest consensus mid price per symbol.
type PriceCache struct {
	mu    sync.RWMutex
	mids  map[string]float64 // canonical symbol → mid
	bands map[string][2]float64 // low, high
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
	// positions: "venue:symbol:market" → notional (positive = long, negative = short)
	positions  sync.Map
}

func NewPaperExecutor(cfg *Config, cache *PriceCache) *PaperExecutor {
	return &PaperExecutor{cfg: cfg, priceCache: cache}
}

// Execute simulates the fill of all legs in an approved intent.
// Returns execution events for each leg and a single SimulatedFill summary.
func (e *PaperExecutor) Execute(ctx context.Context, intent arb.TradeIntent) ([]ExecutionEvent, *SimulatedFill) {
	now := time.Now().UnixMilli()

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

		// Check price limit.
		if leg.PriceLimit > 0 {
			if leg.Action == "BUY" && fillPrice > leg.PriceLimit {
				log.Printf("paper: leg %d BUY fill=%.2f > limit=%.2f — price limit breached",
					i, fillPrice, leg.PriceLimit)
				fillPrice = leg.PriceLimit // still fill but at limit
			}
			if leg.Action == "SELL" && fillPrice < leg.PriceLimit {
				log.Printf("paper: leg %d SELL fill=%.2f < limit=%.2f — price limit breached",
					i, fillPrice, leg.PriceLimit)
				fillPrice = leg.PriceLimit
			}
		}

		slippageActual := math.Abs(fillPrice-mid) / mid * 10000
		feesUSD := leg.NotionalUSD * leg.MaxSlippageBps / 10000 / 2 // approximate
		if feesUSD == 0 {
			feesUSD = leg.NotionalUSD * 4 / 10000 // default 4bps
		}
		totalFees += feesUSD

		// Check for adverse selection: did mid move against us by more than threshold?
		if slippageActual > e.cfg.AdverseSelBps {
			adverseSelection = true
		}

		market := leg.Market
		if market == "" {
			market = "PERP"
		}

		events = append(events, ExecutionEvent{
			EventType:            "ORDER_FILLED",
			IntentID:             intent.IntentID,
			LegIndex:             i,
			Venue:                leg.Venue,
			Symbol:               intent.Symbol,
			Action:               leg.Action,
			Strategy:             intent.Strategy,
			Market:               market,
			RequestedNotionalUSD: leg.NotionalUSD,
			FilledNotionalUSD:    leg.NotionalUSD,
			FilledPrice:          fillPrice,
			SlippageBpsActual:    slippageActual,
			SlippageBpsAllowed:   leg.MaxSlippageBps,
			FeesUSDActual:        feesUSD,
			TsMs:                 fillTs,
			LatencySignalToFillMs: latencyMs,
			TenantID:             e.cfg.TenantID,
			Mode:                 e.cfg.TradingMode,
		})

		// Update virtual position.
		posKey := fmt.Sprintf("%s:%s:%s", leg.Venue, intent.Symbol, market)
		qty := leg.NotionalUSD / fillPrice
		if leg.Action == "SELL" {
			qty = -qty
		}
		e.updatePosition(posKey, qty, fillPrice, leg.NotionalUSD)
	}

	// Compute fill PnL (sell value − buy cost − fees).
	netPnL := 0.0
	if buyPrice > 0 && sellPrice > 0 {
		// Two-leg: PnL = (sell_price - buy_price) × notional / mid − fees
		notional := intent.Legs[0].NotionalUSD
		netPnL = (sellPrice-buyPrice)/mid*notional - totalFees
	}

	edgeAtSignal := intent.Expected.EdgeBpsNet
	edgeAtFill := edgeAtSignal // simplified: no price movement model in V1
	if adverseSelection {
		edgeAtFill = edgeAtSignal * 0.8 // degraded edge
	}

	fill := &SimulatedFill{
		IntentID:              intent.IntentID,
		Strategy:              intent.Strategy,
		Symbol:                intent.Symbol,
		TsSignalMs:            intent.TsMs,
		TsFillSimulatedMs:     fillTs,
		LatencyMs:             latencyMs,
		EdgeAtSignalBps:       edgeAtSignal,
		EdgeAtFillBps:         edgeAtFill,
		EdgeCapturedBps:       edgeAtFill,
		AdverseSelectionOccurred: adverseSelection,
		FillPriceBuy:          buyPrice,
		FillPriceSell:         sellPrice,
		FeesAssumedUSD:        totalFees,
		SlippageAssumedBps:    slipBps,
		NetPnLUSD:             netPnL,
		IntentExpired:         false,
		Mode:                  e.cfg.TradingMode,
		TenantID:              e.cfg.TenantID,
	}

	return events, fill
}

func (e *PaperExecutor) updatePosition(key string, qty, price, notional float64) {
	type posEntry struct {
		qty        float64
		entryPrice float64
		notional   float64
	}
	actual, _ := e.positions.LoadOrStore(key, &posEntry{})
	pos := actual.(*posEntry)
	pos.qty += qty
	pos.notional += notional
	if pos.qty != 0 {
		pos.entryPrice = price
	}
}

// PositionJSON returns all current virtual positions as a JSON-serialisable map.
func (e *PaperExecutor) PositionJSON() map[string]interface{} {
	out := make(map[string]interface{})
	e.positions.Range(func(k, v interface{}) bool {
		type posEntry struct {
			qty        float64
			entryPrice float64
			notional   float64
		}
		if p, ok := v.(*posEntry); ok {
			out[k.(string)] = map[string]float64{
				"qty":         p.qty,
				"entry_price": p.entryPrice,
				"notional":    p.notional,
			}
		}
		return true
	})
	return out
}

// MarshalFill serialises a SimulatedFill to JSON bytes.
func MarshalFill(f *SimulatedFill) ([]byte, error) {
	return json.Marshal(f)
}
