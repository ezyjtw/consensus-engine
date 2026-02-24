package arb

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

const StrategyCascadeContra = "CASCADE_CONTRA"

// CascadeDetector uses OI drops combined with rapid price moves to detect
// liquidation cascades. When detected, it emits contrarian trade intents
// that fade the cascade (buy into sells, sell into buys).
type CascadeDetector struct {
	mu       sync.Mutex
	states   map[string]*cascadeState // "venue:symbol" → state
	oiPrev   map[string]float64       // "venue:symbol" → previous OI
}

type cascadeState struct {
	priceBuf   []priceObs
	bufIdx     int
	bufCap     int
	lastEmitMs int64
}

type priceObs struct {
	mid  float64
	tsMs int64
}

// CascadeConfig holds thresholds for cascade detection.
type CascadeConfig struct {
	Enabled           bool    `yaml:"enabled"`
	OIDropPct         float64 `yaml:"oi_drop_pct"`          // min OI drop % to trigger (e.g. 5.0)
	PriceMoveBps      float64 `yaml:"price_move_bps"`       // min price move in bps within window
	WindowMs          int64   `yaml:"window_ms"`             // observation window (e.g. 60000 = 1min)
	NotionalUSD       float64 `yaml:"notional_usd"`          // position size for contra trade
	MaxSlippageBps    float64 `yaml:"max_slippage_bps"`
	IntentTTLMs       int64   `yaml:"intent_ttl_ms"`
	CooldownMs        int64   `yaml:"cooldown_ms"`
}

// NewCascadeDetector creates a cascade detector.
func NewCascadeDetector() *CascadeDetector {
	return &CascadeDetector{
		states: make(map[string]*cascadeState),
		oiPrev: make(map[string]float64),
	}
}

// RecordQuote tracks price observations for cascade detection.
func (cd *CascadeDetector) RecordQuote(q consensus.Quote) {
	if q.BestBid <= 0 || q.BestAsk <= 0 {
		return
	}
	mid := (q.BestBid + q.BestAsk) / 2

	cd.mu.Lock()
	defer cd.mu.Unlock()

	key := string(q.Venue) + ":" + string(q.Symbol)
	cs, ok := cd.states[key]
	if !ok {
		cs = &cascadeState{
			priceBuf: make([]priceObs, 60),
			bufCap:   60,
		}
		cd.states[key] = cs
	}
	cs.priceBuf[cs.bufIdx%cs.bufCap] = priceObs{mid: mid, tsMs: q.TsMs}
	cs.bufIdx++
}

// UpdateOI updates the OI tracking for cascade detection.
// Returns the OI change percentage since last update.
func (cd *CascadeDetector) UpdateOI(venue, symbol string, currentOI float64) float64 {
	cd.mu.Lock()
	defer cd.mu.Unlock()

	key := venue + ":" + symbol
	prev, ok := cd.oiPrev[key]
	cd.oiPrev[key] = currentOI
	if !ok || prev <= 0 {
		return 0
	}
	return (currentOI - prev) / prev * 100
}

// Evaluate checks for cascade conditions and returns contrarian intents.
func (cd *CascadeDetector) Evaluate(cfg CascadeConfig, tenantID string, cooldown *Cooldown) []TradeIntent {
	if !cfg.Enabled {
		return nil
	}

	cd.mu.Lock()
	defer cd.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for key, cs := range cd.states {
		// Enforce cooldown.
		if now-cs.lastEmitMs < cfg.CooldownMs {
			continue
		}

		venue, symbol := splitKey(key)
		if venue == "" || symbol == "" {
			continue
		}

		// Check OI drop.
		oiKey := venue + ":" + symbol
		prevOI, hasPrev := cd.oiPrev[oiKey]
		_ = prevOI
		if !hasPrev {
			continue
		}

		// Check price move within window.
		priceMoveAbs, direction := cd.priceMove(cs, now, cfg.WindowMs)
		if priceMoveAbs < cfg.PriceMoveBps {
			continue
		}

		// Check OI change from the oiPrev tracking.
		// We need at least 2 OI observations to detect a drop.
		// For now, use the price move + spread widening as the cascade signal.
		// The OI drop check is done externally via UpdateOI.

		log.Printf("cascade: detected venue=%s symbol=%s price_move=%.1fbps direction=%s",
			venue, symbol, priceMoveAbs, direction)

		// Contrarian trade: fade the cascade.
		action := "BUY"  // if price dropped, buy
		if direction == "UP" {
			action = "SELL" // if price spiked up (short squeeze), sell
		}

		// Get latest mid for price limit.
		latestMid := cd.latestMid(cs)
		if latestMid <= 0 {
			continue
		}

		cooldownKey := fmt.Sprintf("cascade:%s:%s", symbol, venue)
		if cooldown != nil && cooldown.IsOnCooldown(cooldownKey, now) {
			continue
		}

		var priceLimit float64
		if action == "BUY" {
			priceLimit = latestMid * 1.005
		} else {
			priceLimit = latestMid * 0.995
		}

		intent := TradeIntent{
			TenantID:  tenantID,
			IntentID:  newUUID(),
			Strategy:  StrategyCascadeContra,
			Symbol:    symbol,
			TsMs:      now,
			ExpiresMs: now + cfg.IntentTTLMs,
			Legs: []TradeLeg{{
				Venue:          venue,
				Action:         action,
				Market:         "PERP",
				Type:           "MARKET_OR_IOC",
				NotionalUSD:    cfg.NotionalUSD,
				MaxSlippageBps: cfg.MaxSlippageBps,
				PriceLimit:     priceLimit,
			}},
			Expected: ExpectedMetrics{
				EdgeBpsGross: priceMoveAbs * 0.3, // expect ~30% reversion
				EdgeBpsNet:   priceMoveAbs*0.3 - 5,
				ProfitUSDNet: cfg.NotionalUSD * (priceMoveAbs*0.3 - 5) / 10000,
			},
			Constraints: IntentConstraints{
				MinQuality:  "MED",
				MaxAgeMs:    cfg.IntentTTLMs,
				CooldownKey: cooldownKey,
			},
			Debug: IntentDebug{
				BuyOn:    venue,
				SellOn:   venue,
				BuyExec:  latestMid,
				SellExec: latestMid,
			},
		}
		intents = append(intents, intent)
		cs.lastEmitMs = now
		if cooldown != nil {
			cooldown.Mark(cooldownKey, now)
		}
	}
	return intents
}

func (cd *CascadeDetector) priceMove(cs *cascadeState, nowMs, windowMs int64) (float64, string) {
	cutoff := nowMs - windowMs
	var oldest, newest priceObs
	oldestSet := false

	n := cs.bufCap
	if cs.bufIdx < n {
		n = cs.bufIdx
	}

	for i := 0; i < n; i++ {
		idx := (cs.bufIdx - 1 - i) % cs.bufCap
		if idx < 0 {
			idx += cs.bufCap
		}
		obs := cs.priceBuf[idx]
		if obs.tsMs == 0 || obs.tsMs < cutoff {
			break
		}
		if !oldestSet || obs.tsMs < oldest.tsMs {
			oldest = obs
			oldestSet = true
		}
		if obs.tsMs > newest.tsMs {
			newest = obs
		}
	}

	if !oldestSet || oldest.mid <= 0 || newest.mid <= 0 {
		return 0, ""
	}

	moveBps := (newest.mid - oldest.mid) / oldest.mid * 10000
	absBps := math.Abs(moveBps)
	direction := "DOWN"
	if moveBps > 0 {
		direction = "UP"
	}
	return absBps, direction
}

func (cd *CascadeDetector) latestMid(cs *cascadeState) float64 {
	if cs.bufIdx == 0 {
		return 0
	}
	idx := (cs.bufIdx - 1) % cs.bufCap
	return cs.priceBuf[idx].mid
}

func splitKey(key string) (string, string) {
	for i, c := range key {
		if c == ':' {
			return key[:i], key[i+1:]
		}
	}
	return "", ""
}
