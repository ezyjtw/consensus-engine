package execution

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/exchange"
)

// LiveExecutor submits real orders to exchange REST APIs.
type LiveExecutor struct {
	cfg        *Config
	priceCache *PriceCache
	registry   *exchange.Registry
}

// NewLiveExecutor creates a live executor backed by real exchange adapters.
func NewLiveExecutor(cfg *Config, cache *PriceCache, registry *exchange.Registry) *LiveExecutor {
	return &LiveExecutor{cfg: cfg, priceCache: cache, registry: registry}
}

// Execute places real orders for all legs in an approved intent.
// Returns execution events for each leg and a fill summary.
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

	mid, ok := e.priceCache.Mid(intent.Symbol)
	if !ok || mid == 0 {
		log.Printf("live: no consensus price for %s — skipping", intent.Symbol)
		return nil, nil
	}

	var events []ExecutionEvent
	var buyPrice, sellPrice float64
	var totalFees float64

	for i, leg := range intent.Legs {
		ex, err := e.registry.Get(ctx, leg.Venue)
		if err != nil {
			log.Printf("live: no exchange client for %s: %v", leg.Venue, err)
			events = append(events, ExecutionEvent{
				EventType: "ORDER_REJECTED",
				IntentID:  intent.IntentID,
				LegIndex:  i,
				Venue:     leg.Venue,
				Symbol:    intent.Symbol,
				Action:    leg.Action,
				Strategy:  intent.Strategy,
				TsMs:      now,
				TenantID:  e.cfg.TenantID,
				Mode:      e.cfg.TradingMode,
			})
			continue
		}

		// Map our canonical symbol to exchange-native symbol.
		nativeSymbol := mapSymbol(leg.Venue, intent.Symbol)

		// Calculate quantity from notional and mid-price.
		qty := leg.NotionalUSD / mid

		// Calculate price limit with slippage buffer.
		var priceLimit float64
		if leg.Action == "BUY" {
			priceLimit = mid * (1 + leg.MaxSlippageBps/10000)
		} else {
			priceLimit = mid * (1 - leg.MaxSlippageBps/10000)
		}

		orderReq := exchange.OrderRequest{
			Symbol:         nativeSymbol,
			Side:           exchange.Side(leg.Action),
			Type:           exchange.OrderTypeIOC,
			Quantity:       qty,
			Price:          priceLimit,
			NotionalUSD:    leg.NotionalUSD,
			MaxSlippageBps: leg.MaxSlippageBps,
			ClientOrderID:  fmt.Sprintf("%s-leg%d", intent.IntentID, i),
		}

		orderResp, err := ex.PlaceOrder(ctx, orderReq)
		if err != nil {
			log.Printf("live: order failed on %s: %v", leg.Venue, err)
			events = append(events, ExecutionEvent{
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
			continue
		}

		// Poll for fill status (up to 5 seconds).
		var finalOrder *exchange.OrderResponse
		for attempt := 0; attempt < 10; attempt++ {
			time.Sleep(500 * time.Millisecond)
			resp, err := ex.GetOrder(ctx, nativeSymbol, orderResp.OrderID)
			if err != nil {
				continue
			}
			finalOrder = resp
			if resp.Status == exchange.OrderStatusFilled ||
				resp.Status == exchange.OrderStatusCancelled ||
				resp.Status == exchange.OrderStatusRejected ||
				resp.Status == exchange.OrderStatusExpired {
				break
			}
		}

		if finalOrder == nil {
			finalOrder = orderResp
		}

		fillTs := time.Now().UnixMilli()
		latency := fillTs - now

		filledNotional := finalOrder.FilledQty * finalOrder.AvgFillPrice
		if filledNotional == 0 {
			filledNotional = leg.NotionalUSD // fallback
		}

		fillPrice := finalOrder.AvgFillPrice
		if fillPrice == 0 {
			fillPrice = mid
		}

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
		if finalOrder.Status == exchange.OrderStatusRejected {
			eventType = "ORDER_REJECTED"
		} else if finalOrder.Status == exchange.OrderStatusCancelled || finalOrder.Status == exchange.OrderStatusExpired {
			if finalOrder.FilledQty > 0 {
				eventType = "LEG_PARTIAL"
			} else {
				eventType = "ORDER_REJECTED"
			}
		}

		events = append(events, ExecutionEvent{
			EventType:             eventType,
			IntentID:              intent.IntentID,
			LegIndex:              i,
			Venue:                 leg.Venue,
			Symbol:                intent.Symbol,
			Action:                leg.Action,
			Strategy:              intent.Strategy,
			Market:                market,
			RequestedNotionalUSD:  leg.NotionalUSD,
			FilledNotionalUSD:     filledNotional,
			FilledPrice:           fillPrice,
			SlippageBpsActual:     slipBps,
			SlippageBpsAllowed:    leg.MaxSlippageBps,
			FeesUSDActual:         finalOrder.FeesUSD,
			TsMs:                  fillTs,
			LatencySignalToFillMs: latency,
			TenantID:              e.cfg.TenantID,
			Mode:                  e.cfg.TradingMode,
		})
	}

	netPnL := 0.0
	if buyPrice > 0 && sellPrice > 0 {
		notional := intent.Legs[0].NotionalUSD
		netPnL = (sellPrice-buyPrice)/mid*notional - totalFees
	}

	fillLegs := make([]FillLeg, 0, len(events))
	for _, ev := range events {
		fillLegs = append(fillLegs, FillLeg{
			Venue:             ev.Venue,
			Action:            ev.Action,
			FilledNotionalUSD: ev.FilledNotionalUSD,
			FilledPrice:       ev.FilledPrice,
		})
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

	return events, fill
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
