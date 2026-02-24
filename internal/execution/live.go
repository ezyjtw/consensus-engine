package execution

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

// legResult captures the outcome of executing a single leg.
type legResult struct {
	filled      bool
	filledQty   float64
	filledUSD   float64
	fillPrice   float64
	feesUSD     float64
	orderID     string
	venue       string
	nativeSym   string
	partialFill bool
	fillTsMs    int64 // timestamp when fill was confirmed
}

// LiveExecutor submits real orders to exchange REST APIs with institutional
// safety primitives: partial fill handling, hedge drift enforcement, emergency
// unwind, and post-fill reconciliation.
type LiveExecutor struct {
	cfg        *Config
	priceCache *PriceCache
	registry   *exchange.Registry

	// Micro-live graduation: rolling daily notional tracking.
	dailyMu    sync.Mutex
	dailyFills []dailyFillEntry
}

// dailyFillEntry tracks a fill timestamp and notional for rolling daily cap.
type dailyFillEntry struct {
	tsMs       int64
	notionalUSD float64
}

// NewLiveExecutor creates a live executor backed by real exchange adapters.
func NewLiveExecutor(cfg *Config, cache *PriceCache, registry *exchange.Registry) *LiveExecutor {
	return &LiveExecutor{cfg: cfg, priceCache: cache, registry: registry}
}

// rollingDailyNotional returns the total notional filled in the last 24 hours.
func (e *LiveExecutor) rollingDailyNotional(nowMs int64) float64 {
	cutoff := nowMs - 86_400_000 // 24 hours
	var total float64
	// Prune old entries while summing.
	kept := e.dailyFills[:0]
	for _, f := range e.dailyFills {
		if f.tsMs >= cutoff {
			total += f.notionalUSD
			kept = append(kept, f)
		}
	}
	e.dailyFills = kept
	return total
}

// recordDailyFill adds a fill to the rolling daily tracker.
func (e *LiveExecutor) recordDailyFill(tsMs int64, notionalUSD float64) {
	e.dailyMu.Lock()
	defer e.dailyMu.Unlock()
	e.dailyFills = append(e.dailyFills, dailyFillEntry{tsMs: tsMs, notionalUSD: notionalUSD})
}

// Execute places real orders for all legs in an approved intent, sequentially.
// If leg A fills (or partially fills), leg B is adjusted to match. If leg B
// fails, an emergency unwind of leg A is attempted. Hedge drift is tracked
// between legs. After completion, fills are reconciled against exchange state.
func (e *LiveExecutor) Execute(ctx context.Context, intent arb.TradeIntent) ([]ExecutionEvent, *SimulatedFill) {
	now := time.Now().UnixMilli()

	if now > intent.ExpiresMs {
		log.Printf("live: intent %s expired", intent.IntentID)
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

	// Micro-live graduation caps: enforce per-order and daily notional limits.
	totalIntentNotional := 0.0
	for _, leg := range intent.Legs {
		totalIntentNotional += leg.NotionalUSD
	}
	if e.cfg.LiveMaxOrderNotionalUSD > 0 {
		for _, leg := range intent.Legs {
			if leg.NotionalUSD > e.cfg.LiveMaxOrderNotionalUSD {
				log.Printf("live: leg notional $%.0f exceeds micro-live cap $%.0f — rejecting",
					leg.NotionalUSD, e.cfg.LiveMaxOrderNotionalUSD)
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
		}
	}
	if e.cfg.LiveMaxDailyNotionalUSD > 0 {
		e.dailyMu.Lock()
		rollingTotal := e.rollingDailyNotional(now)
		e.dailyMu.Unlock()
		if rollingTotal+totalIntentNotional > e.cfg.LiveMaxDailyNotionalUSD {
			log.Printf("live: daily notional $%.0f + $%.0f would exceed micro-live cap $%.0f — rejecting",
				rollingTotal, totalIntentNotional, e.cfg.LiveMaxDailyNotionalUSD)
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
	}

	mid, ok := e.priceCache.Mid(intent.Symbol)
	if !ok || mid == 0 {
		log.Printf("live: no consensus price for %s — skipping", intent.Symbol)
		return nil, nil
	}

	if len(intent.Legs) == 0 {
		return nil, nil
	}

	var allEvents []ExecutionEvent
	var buyPrice, sellPrice float64
	var totalFees float64

	results := make([]legResult, len(intent.Legs))

	// Execute legs sequentially so we can adjust leg B based on leg A's fill.
	for i, leg := range intent.Legs {
		// Check expiry before each leg.
		if time.Now().UnixMilli() > intent.ExpiresMs {
			log.Printf("live: intent %s expired before leg %d", intent.IntentID, i)
			allEvents = append(allEvents, ExecutionEvent{
				EventType: "ORDER_REJECTED",
				IntentID:  intent.IntentID,
				LegIndex:  i,
				Venue:     leg.Venue,
				Symbol:    intent.Symbol,
				Action:    leg.Action,
				Strategy:  intent.Strategy,
				TsMs:      time.Now().UnixMilli(),
				TenantID:  e.cfg.TenantID,
				Mode:      e.cfg.TradingMode,
			})
			// If a previous leg filled, we have unhedged exposure — attempt unwind.
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], mid, &allEvents)
			}
			break
		}

		// Check hedge drift: if this is leg B and leg A is filled, enforce max drift.
		if i > 0 && results[i-1].filled {
			driftMs := time.Now().UnixMilli() - results[i-1].fillTsMs
			if driftMs > e.cfg.HedgeDriftMaxMs {
				log.Printf("live: hedge drift %dms exceeds max %dms — aborting leg %d",
					driftMs, e.cfg.HedgeDriftMaxMs, i)
				allEvents = append(allEvents, ExecutionEvent{
					EventType: "HEDGE_DRIFT",
					IntentID:  intent.IntentID,
					LegIndex:  i,
					Venue:     leg.Venue,
					Symbol:    intent.Symbol,
					Action:    leg.Action,
					Strategy:  intent.Strategy,
					TsMs:      time.Now().UnixMilli(),
					TenantID:  e.cfg.TenantID,
					Mode:      e.cfg.TradingMode,
				})
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], mid, &allEvents)
				break
			}
		}

		ex, err := e.registry.Get(ctx, leg.Venue)
		if err != nil {
			log.Printf("live: no exchange client for %s: %v", leg.Venue, err)
			allEvents = append(allEvents, ExecutionEvent{
				EventType: "ORDER_REJECTED",
				IntentID:  intent.IntentID,
				LegIndex:  i,
				Venue:     leg.Venue,
				Symbol:    intent.Symbol,
				Action:    leg.Action,
				Strategy:  intent.Strategy,
				TsMs:      time.Now().UnixMilli(),
				TenantID:  e.cfg.TenantID,
				Mode:      e.cfg.TradingMode,
			})
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], mid, &allEvents)
			}
			break
		}

		nativeSymbol := mapSymbol(leg.Venue, intent.Symbol)

		// Fetch venue constraints for price/qty rounding.
		constraints, _ := ex.GetConstraints(ctx, nativeSymbol)
		if constraints == nil {
			constraints = &exchange.VenueConstraints{Symbol: nativeSymbol}
		}

		// Adjust notional if previous leg was a partial fill.
		targetNotional := leg.NotionalUSD
		if i > 0 && results[i-1].partialFill {
			targetNotional = results[i-1].filledUSD
			log.Printf("live: adjusting leg %d notional to $%.2f (matching partial fill on leg %d)",
				i, targetNotional, i-1)
		}

		// Check minimum notional.
		if constraints.MinNotional > 0 && targetNotional < constraints.MinNotional {
			log.Printf("live: notional $%.2f below venue min $%.2f — skipping leg %d",
				targetNotional, constraints.MinNotional, i)
			break
		}

		qty := constraints.RoundQty(targetNotional / mid)
		if constraints.MinQty > 0 && qty < constraints.MinQty {
			qty = constraints.MinQty
		}

		var worstPrice float64
		if leg.Action == "BUY" {
			worstPrice = constraints.RoundPrice(mid * (1 + leg.MaxSlippageBps/10000))
		} else {
			worstPrice = constraints.RoundPrice(mid * (1 - leg.MaxSlippageBps/10000))
		}

		// Order strategy: attempt LIMIT first (saves taker fees), then IOC on retry.
		// First attempt uses a tight limit price (mid ± 1bps) for maker rebate.
		// If it doesn't fill, cancel/replace at progressively wider prices.
		// Final attempt uses IOC at worst price to guarantee fill.
		var finalOrder *exchange.OrderResponse
		var orderErr error
		for attempt := 0; attempt <= e.cfg.MaxRetriesPerLeg; attempt++ {
			if attempt > 0 {
				log.Printf("live: retrying leg %d attempt %d/%d", i, attempt, e.cfg.MaxRetriesPerLeg)
				time.Sleep(time.Duration(attempt*500) * time.Millisecond)

				// Re-check expiry before retry.
				if time.Now().UnixMilli() > intent.ExpiresMs {
					orderErr = fmt.Errorf("intent expired during retry")
					break
				}
			}

			// Choose order type and price based on attempt number.
			orderType := exchange.OrderTypeLimit
			var orderPrice float64
			if attempt < e.cfg.MaxRetriesPerLeg {
				// Early attempts: use tighter limit price for maker rebate.
				frac := float64(attempt+1) / float64(e.cfg.MaxRetriesPerLeg+1)
				if leg.Action == "BUY" {
					orderPrice = constraints.RoundPrice(mid * (1 + frac*leg.MaxSlippageBps/10000))
				} else {
					orderPrice = constraints.RoundPrice(mid * (1 - frac*leg.MaxSlippageBps/10000))
				}
			} else {
				// Final attempt: IOC at worst price to guarantee fill.
				orderType = exchange.OrderTypeIOC
				orderPrice = worstPrice
			}

			orderReq := exchange.OrderRequest{
				Symbol:         nativeSymbol,
				Side:           exchange.Side(leg.Action),
				Type:           orderType,
				Quantity:       qty,
				Price:          orderPrice,
				NotionalUSD:    targetNotional,
				MaxSlippageBps: leg.MaxSlippageBps,
				ClientOrderID:  fmt.Sprintf("%s-leg%d-r%d", intent.IntentID, i, attempt),
			}

			orderResp, err := ex.PlaceOrder(ctx, orderReq)
			if err != nil {
				orderErr = err
				log.Printf("live: order failed on %s (attempt %d): %v", leg.Venue, attempt, err)
				continue
			}

			// Poll for terminal status.
			final := e.pollOrderStatus(ctx, ex, nativeSymbol, orderResp.OrderID)
			if final == nil {
				final = orderResp
			}

			if final.Status == exchange.OrderStatusFilled ||
				(final.Status == exchange.OrderStatusPartiallyFilled && final.FilledQty > 0) {
				finalOrder = final
				orderErr = nil
				break
			}

			// Not filled — cancel the limit order before retrying at wider price.
			if orderType == exchange.OrderTypeLimit {
				_ = ex.CancelOrder(ctx, nativeSymbol, final.OrderID)
			}

			// Partial fill below minimum threshold — cancel remainder.
			if final.FilledQty > 0 {
				fillPct := final.FilledQty / qty
				if fillPct < e.cfg.MinPartialFillPct {
					log.Printf("live: partial fill %.1f%% below min %.1f%% — retrying wider",
						fillPct*100, e.cfg.MinPartialFillPct*100)
					orderErr = fmt.Errorf("partial fill below threshold")
					continue
				}
				finalOrder = final
				orderErr = nil
				break
			}

			orderErr = fmt.Errorf("order status: %s", final.Status)
		}

		if orderErr != nil || finalOrder == nil {
			log.Printf("live: leg %d failed after retries: %v", i, orderErr)
			allEvents = append(allEvents, ExecutionEvent{
				EventType: "ORDER_REJECTED",
				IntentID:  intent.IntentID,
				LegIndex:  i,
				Venue:     leg.Venue,
				Symbol:    intent.Symbol,
				Action:    leg.Action,
				Strategy:  intent.Strategy,
				TsMs:      time.Now().UnixMilli(),
				TenantID:  e.cfg.TenantID,
				Mode:      e.cfg.TradingMode,
			})
			if i > 0 && results[i-1].filled {
				e.emergencyUnwind(ctx, intent, i-1, results[i-1], mid, &allEvents)
			}
			break
		}

		// Record result.
		fillPrice := finalOrder.AvgFillPrice
		if fillPrice == 0 {
			fillPrice = mid
		}
		filledNotional := finalOrder.FilledQty * fillPrice
		if filledNotional == 0 {
			filledNotional = targetNotional
		}

		isPartial := finalOrder.Status == exchange.OrderStatusPartiallyFilled ||
			(finalOrder.FilledQty > 0 && finalOrder.FilledQty < qty*0.99)

		fillTsMs := time.Now().UnixMilli()
		results[i] = legResult{
			filled:      true,
			filledQty:   finalOrder.FilledQty,
			filledUSD:   filledNotional,
			fillPrice:   fillPrice,
			feesUSD:     finalOrder.FeesUSD,
			orderID:     finalOrder.OrderID,
			venue:       leg.Venue,
			nativeSym:   nativeSymbol,
			partialFill: isPartial,
			fillTsMs:    fillTsMs,
		}

		// Track notional for micro-live daily cap.
		e.recordDailyFill(fillTsMs, filledNotional)

		if leg.Action == "BUY" {
			buyPrice = fillPrice
		} else {
			sellPrice = fillPrice
		}
		totalFees += finalOrder.FeesUSD

		slipBps := math.Abs(fillPrice-mid) / mid * 10000

		market := leg.Market
		if market == "" {
			market = "PERP"
		}

		eventType := "ORDER_FILLED"
		if isPartial {
			eventType = "LEG_PARTIAL"
		}

		fillTs := time.Now().UnixMilli()
		allEvents = append(allEvents, ExecutionEvent{
			EventType:             eventType,
			IntentID:              intent.IntentID,
			LegIndex:              i,
			Venue:                 leg.Venue,
			Symbol:                intent.Symbol,
			Action:                leg.Action,
			Strategy:              intent.Strategy,
			Market:                market,
			RequestedNotionalUSD:  targetNotional,
			FilledNotionalUSD:     filledNotional,
			FilledPrice:           fillPrice,
			SlippageBpsActual:     slipBps,
			SlippageBpsAllowed:    leg.MaxSlippageBps,
			FeesUSDActual:         finalOrder.FeesUSD,
			TsMs:                  fillTs,
			LatencySignalToFillMs: fillTs - now,
			TenantID:              e.cfg.TenantID,
			Mode:                  e.cfg.TradingMode,
		})
	}

	// Schedule async reconciliation if we had fills.
	anyFilled := false
	for _, r := range results {
		if r.filled {
			anyFilled = true
			break
		}
	}
	if anyFilled {
		go e.reconcileFills(ctx, intent, results)
	}

	// Build fill summary.
	netPnL := 0.0
	if buyPrice > 0 && sellPrice > 0 {
		notional := results[0].filledUSD
		if notional == 0 {
			notional = intent.Legs[0].NotionalUSD
		}
		netPnL = (sellPrice-buyPrice)/mid*notional - totalFees
	}

	fillLegs := make([]FillLeg, 0, len(allEvents))
	for _, ev := range allEvents {
		if ev.EventType == "ORDER_FILLED" || ev.EventType == "LEG_PARTIAL" {
			fillLegs = append(fillLegs, FillLeg{
				Venue:             ev.Venue,
				Action:            ev.Action,
				FilledNotionalUSD: ev.FilledNotionalUSD,
				FilledPrice:       ev.FilledPrice,
			})
		}
	}

	fill := &SimulatedFill{
		IntentID:          intent.IntentID,
		Strategy:          intent.Strategy,
		Symbol:            intent.Symbol,
		Legs:              fillLegs,
		TsSignalMs:        intent.TsMs,
		TsFillSimulatedMs: time.Now().UnixMilli(),
		LatencyMs:         time.Now().UnixMilli() - now,
		EdgeAtSignalBps:   intent.Expected.EdgeBpsNet,
		EdgeAtFillBps:     intent.Expected.EdgeBpsNet,
		EdgeCapturedBps:   intent.Expected.EdgeBpsNet,
		FillPriceBuy:      buyPrice,
		FillPriceSell:     sellPrice,
		FeesAssumedUSD:    totalFees,
		NetPnLUSD:         netPnL,
		IntentExpired:     false,
		Mode:              e.cfg.TradingMode,
		TenantID:          e.cfg.TenantID,
	}

	return allEvents, fill
}

// pollOrderStatus polls the exchange for terminal order status.
func (e *LiveExecutor) pollOrderStatus(ctx context.Context, ex exchange.Exchange, symbol, orderID string) *exchange.OrderResponse {
	for attempt := 0; attempt < 10; attempt++ {
		time.Sleep(500 * time.Millisecond)
		resp, err := ex.GetOrder(ctx, symbol, orderID)
		if err != nil {
			continue
		}
		switch resp.Status {
		case exchange.OrderStatusFilled,
			exchange.OrderStatusCancelled,
			exchange.OrderStatusRejected,
			exchange.OrderStatusExpired,
			exchange.OrderStatusPartiallyFilled:
			return resp
		}
	}
	return nil
}

// emergencyUnwind attempts to close the position from a filled leg that has
// no matching hedge. It places a market order in the opposite direction.
func (e *LiveExecutor) emergencyUnwind(
	ctx context.Context,
	intent arb.TradeIntent,
	legIdx int,
	result legResult,
	mid float64,
	events *[]ExecutionEvent,
) {
	log.Printf("live: EMERGENCY UNWIND — reversing leg %d on %s (qty=%.6f)",
		legIdx, result.venue, result.filledQty)

	ex, err := e.registry.Get(ctx, result.venue)
	if err != nil {
		log.Printf("live: emergency unwind failed — no exchange client for %s: %v", result.venue, err)
		*events = append(*events, ExecutionEvent{
			EventType: "HEDGE_FAILED",
			IntentID:  intent.IntentID,
			LegIndex:  legIdx,
			Venue:     result.venue,
			Symbol:    intent.Symbol,
			Strategy:  intent.Strategy,
			TsMs:      time.Now().UnixMilli(),
			TenantID:  e.cfg.TenantID,
			Mode:      e.cfg.TradingMode,
		})
		return
	}

	// Reverse the action.
	originalAction := intent.Legs[legIdx].Action
	reverseAction := "SELL"
	if originalAction == "SELL" {
		reverseAction = "BUY"
	}

	unwindReq := exchange.OrderRequest{
		Symbol:        result.nativeSym,
		Side:          exchange.Side(reverseAction),
		Type:          exchange.OrderTypeMarket,
		Quantity:      result.filledQty,
		NotionalUSD:   result.filledUSD,
		ReduceOnly:    true,
		ClientOrderID: fmt.Sprintf("%s-unwind-leg%d", intent.IntentID, legIdx),
	}

	resp, err := ex.PlaceOrder(ctx, unwindReq)
	if err != nil {
		log.Printf("live: EMERGENCY UNWIND FAILED on %s: %v", result.venue, err)
		*events = append(*events, ExecutionEvent{
			EventType: "HEDGE_FAILED",
			IntentID:  intent.IntentID,
			LegIndex:  legIdx,
			Venue:     result.venue,
			Symbol:    intent.Symbol,
			Action:    reverseAction,
			Strategy:  intent.Strategy,
			TsMs:      time.Now().UnixMilli(),
			TenantID:  e.cfg.TenantID,
			Mode:      e.cfg.TradingMode,
		})
		return
	}

	// Wait for unwind fill.
	final := e.pollOrderStatus(ctx, ex, result.nativeSym, resp.OrderID)
	if final == nil {
		final = resp
	}

	unwindPrice := final.AvgFillPrice
	if unwindPrice == 0 {
		unwindPrice = mid
	}

	eventType := "ORDER_FILLED"
	if final.Status != exchange.OrderStatusFilled {
		eventType = "HEDGE_FAILED"
		log.Printf("live: emergency unwind did not fully fill (status=%s)", final.Status)
	} else {
		log.Printf("live: emergency unwind filled at %.2f on %s", unwindPrice, result.venue)
	}

	*events = append(*events, ExecutionEvent{
		EventType:            eventType,
		IntentID:             intent.IntentID,
		LegIndex:             legIdx,
		Venue:                result.venue,
		Symbol:               intent.Symbol,
		Action:               reverseAction,
		Strategy:             intent.Strategy,
		Market:               "PERP",
		RequestedNotionalUSD: result.filledUSD,
		FilledNotionalUSD:    final.FilledQty * unwindPrice,
		FilledPrice:          unwindPrice,
		FeesUSDActual:        final.FeesUSD,
		TsMs:                 time.Now().UnixMilli(),
		TenantID:             e.cfg.TenantID,
		Mode:                 e.cfg.TradingMode,
	})
}

// reconcileFills verifies that our internal fill records match the exchange's
// view of the orders. Logs discrepancies for investigation.
func (e *LiveExecutor) reconcileFills(
	ctx context.Context,
	intent arb.TradeIntent,
	results []legResult,
) {
	time.Sleep(time.Duration(e.cfg.ReconDelayMs) * time.Millisecond)

	for i, r := range results {
		if !r.filled || r.orderID == "" {
			continue
		}

		ex, err := e.registry.Get(ctx, r.venue)
		if err != nil {
			log.Printf("live-recon: cannot get exchange %s for recon: %v", r.venue, err)
			continue
		}

		order, err := ex.GetOrder(ctx, r.nativeSym, r.orderID)
		if err != nil {
			log.Printf("live-recon: cannot fetch order %s on %s: %v", r.orderID, r.venue, err)
			continue
		}

		// Check quantity divergence.
		if order.FilledQty > 0 {
			qtyDiff := math.Abs(r.filledQty-order.FilledQty) / order.FilledQty
			if qtyDiff > 0.01 { // >1% divergence
				log.Printf("live-recon: QUANTITY DIVERGENCE intent=%s leg=%d venue=%s "+
					"internal=%.6f exchange=%.6f diff=%.2f%%",
					intent.IntentID, i, r.venue,
					r.filledQty, order.FilledQty, qtyDiff*100)
			}
		}

		// Check price divergence.
		if order.AvgFillPrice > 0 {
			priceDiff := math.Abs(r.fillPrice-order.AvgFillPrice) / order.AvgFillPrice
			if priceDiff > 0.001 { // >0.1% = 10bps divergence
				log.Printf("live-recon: PRICE DIVERGENCE intent=%s leg=%d venue=%s "+
					"internal=%.2f exchange=%.2f diff=%.1fbps",
					intent.IntentID, i, r.venue,
					r.fillPrice, order.AvgFillPrice, priceDiff*10000)
			}
		}

		// Check fee divergence.
		if order.FeesUSD > 0 {
			feeDiff := math.Abs(r.feesUSD - order.FeesUSD)
			if feeDiff > 1.0 { // >$1 fee difference
				log.Printf("live-recon: FEE DIVERGENCE intent=%s leg=%d venue=%s "+
					"internal=$%.2f exchange=$%.2f",
					intent.IntentID, i, r.venue, r.feesUSD, order.FeesUSD)
			}
		}
	}
}

// ── Periodic position reconciliation ──────────────────────────────────────

// ReconcilePositions performs a position truth pull across all venues.
// Fetches real positions from each exchange and logs discrepancies.
func (e *LiveExecutor) ReconcilePositions(ctx context.Context, venues []string) []ExecutionEvent {
	var events []ExecutionEvent
	now := time.Now().UnixMilli()

	for _, venue := range venues {
		ex, err := e.registry.Get(ctx, venue)
		if err != nil {
			log.Printf("live-recon: cannot get exchange %s: %v", venue, err)
			continue
		}

		positions, err := ex.GetPositions(ctx)
		if err != nil {
			log.Printf("live-recon: GetPositions failed on %s: %v", venue, err)
			events = append(events, ExecutionEvent{
				EventType: "RECON_FAILED",
				Venue:     venue,
				TsMs:      now,
				TenantID:  e.cfg.TenantID,
				Mode:      e.cfg.TradingMode,
			})
			continue
		}

		balances, err := ex.GetBalances(ctx)
		if err != nil {
			log.Printf("live-recon: GetBalances failed on %s: %v", venue, err)
		}

		for _, pos := range positions {
			if pos.Quantity == 0 {
				continue
			}
			log.Printf("live-recon: POSITION TRUTH venue=%s sym=%s side=%s qty=%.6f notional=$%.2f pnl=$%.2f",
				venue, pos.Symbol, pos.Side, pos.Quantity, pos.NotionalUSD, pos.UnrealizedPnL)

			events = append(events, ExecutionEvent{
				EventType:         "POSITION_TRUTH",
				Venue:             venue,
				Symbol:            pos.Symbol,
				Action:            pos.Side,
				FilledNotionalUSD: pos.NotionalUSD,
				FilledPrice:       pos.MarkPrice,
				TsMs:              now,
				TenantID:          e.cfg.TenantID,
				Mode:              e.cfg.TradingMode,
			})
		}

		totalUSD := 0.0
		for _, bal := range balances {
			if bal.USDValue > 0 {
				totalUSD += bal.USDValue
			}
		}
		if totalUSD > 0 {
			log.Printf("live-recon: BALANCE TRUTH venue=%s total_usd=$%.2f", venue, totalUSD)
		}
	}

	return events
}

// StartPeriodicReconciliation launches a background goroutine that polls
// exchange positions every intervalMs milliseconds.
func (e *LiveExecutor) StartPeriodicReconciliation(ctx context.Context, venues []string, intervalMs int64) {
	if intervalMs <= 0 {
		intervalMs = 30000
	}
	go func() {
		ticker := time.NewTicker(time.Duration(intervalMs) * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				events := e.ReconcilePositions(ctx, venues)
				if len(events) > 0 {
					log.Printf("live-recon: periodic recon produced %d events", len(events))
				}
			}
		}
	}()
	log.Printf("live-recon: periodic reconciliation started (interval=%dms, venues=%v)", intervalMs, venues)
}

// mapSymbol converts canonical symbols (e.g. "BTC-PERP") to exchange-native format.
func mapSymbol(venue, symbol string) string {
	// Remove the -PERP suffix and map to venue conventions.
	base := symbol
	if len(symbol) > 5 && symbol[len(symbol)-5:] == "-PERP" {
		base = symbol[:len(symbol)-5]
	}

	switch venue {
	case "binance":
		return base + "USDT" // BTCUSDT
	case "okx":
		return base + "-USDT-SWAP" // BTC-USDT-SWAP
	case "bybit":
		return base + "USDT" // BTCUSDT
	case "deribit":
		return base + "-PERPETUAL" // BTC-PERPETUAL
	default:
		return symbol
	}
}
