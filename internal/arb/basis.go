package arb

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

const (
	StrategyBasisTrade = "BASIS_TRADE"
)

// BasisTracker monitors the spot-futures basis (premium/discount) across venues.
// When the basis is significantly positive (contango), it emits a trade intent
// to buy spot and sell futures, capturing the basis as it converges.
type BasisTracker struct {
	mu sync.Mutex
	// basisHistory[symbol][venue] → rolling basis observations
	basisHistory map[string]map[string]*basisWindow
	windowSize   int // number of observations to keep
}

type basisObs struct {
	basisBps float64 // (mark - index) / index * 10000
	markPx   float64
	indexPx  float64
	tsMs     int64
}

type basisWindow struct {
	obs  []basisObs
	idx  int
	full bool
	cap  int
}

// NewBasisTracker creates a basis tracker with the given window size.
func NewBasisTracker(windowSize int) *BasisTracker {
	if windowSize < 10 {
		windowSize = 200
	}
	return &BasisTracker{
		basisHistory: make(map[string]map[string]*basisWindow),
		windowSize:   windowSize,
	}
}

// RecordQuote updates the basis observation for a venue when both mark and index are available.
func (bt *BasisTracker) RecordQuote(q consensus.Quote) {
	if q.Mark <= 0 || q.Index <= 0 {
		return
	}
	bps := (q.Mark - q.Index) / q.Index * 10000

	bt.mu.Lock()
	defer bt.mu.Unlock()

	sym := string(q.Symbol)
	ven := string(q.Venue)
	if bt.basisHistory[sym] == nil {
		bt.basisHistory[sym] = make(map[string]*basisWindow)
	}
	w, ok := bt.basisHistory[sym][ven]
	if !ok {
		w = &basisWindow{obs: make([]basisObs, bt.windowSize), cap: bt.windowSize}
		bt.basisHistory[sym][ven] = w
	}
	w.obs[w.idx] = basisObs{basisBps: bps, markPx: q.Mark, indexPx: q.Index, tsMs: q.TsMs}
	w.idx++
	if w.idx >= w.cap {
		w.idx = 0
		w.full = true
	}
}

// BasisSnapshot holds the current basis state for a venue+symbol.
type BasisSnapshot struct {
	Venue      string
	Symbol     string
	CurrentBps float64 // current basis in bps
	MeanBps    float64 // rolling mean basis
	StdDevBps  float64 // rolling stddev
	ZScore     float64 // how many stddevs from mean
	MarkPx     float64
	IndexPx    float64
	TsMs       int64
}

// Snapshot returns the current basis snapshot for a venue+symbol.
// Returns nil if insufficient data.
func (bt *BasisTracker) Snapshot(symbol, venue string) *BasisSnapshot {
	bt.mu.Lock()
	defer bt.mu.Unlock()

	venMap, ok := bt.basisHistory[symbol]
	if !ok {
		return nil
	}
	w, ok := venMap[venue]
	if !ok {
		return nil
	}
	n := w.count()
	if n < 20 {
		return nil
	}

	obs := w.sorted()
	latest := obs[len(obs)-1]

	var sum, sumSq float64
	for _, o := range obs {
		sum += o.basisBps
		sumSq += o.basisBps * o.basisBps
	}
	mean := sum / float64(n)
	variance := sumSq/float64(n) - mean*mean
	stdDev := 0.0
	if variance > 0 {
		stdDev = math.Sqrt(variance)
	}
	zScore := 0.0
	if stdDev > 0 {
		zScore = (latest.basisBps - mean) / stdDev
	}

	return &BasisSnapshot{
		Venue:      venue,
		Symbol:     symbol,
		CurrentBps: latest.basisBps,
		MeanBps:    mean,
		StdDevBps:  stdDev,
		ZScore:     zScore,
		MarkPx:     latest.markPx,
		IndexPx:    latest.indexPx,
		TsMs:       latest.tsMs,
	}
}

// BasisConfig holds thresholds for basis trade generation.
type BasisConfig struct {
	MinBasisBps   float64 // minimum absolute basis to consider (e.g. 15 bps)
	MinZScore     float64 // minimum z-score (basis deviation from mean, e.g. 1.5)
	MaxNotionalUSD float64
	MaxSlippageBps float64
	IntentTTLMs   int64
	CooldownMs    int64
}

// EvaluateBasisTrades checks all venue+symbol pairs for basis trade opportunities.
// Returns intents for any qualifying basis convergence trades.
func (bt *BasisTracker) EvaluateBasisTrades(cfg BasisConfig, tenantID string, cooldown *Cooldown) []TradeIntent {
	bt.mu.Lock()
	symbols := make([]string, 0, len(bt.basisHistory))
	venuesBySymbol := make(map[string][]string)
	for sym, venMap := range bt.basisHistory {
		symbols = append(symbols, sym)
		for ven := range venMap {
			venuesBySymbol[sym] = append(venuesBySymbol[sym], ven)
		}
	}
	bt.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for _, sym := range symbols {
		for _, ven := range venuesBySymbol[sym] {
			snap := bt.Snapshot(sym, ven)
			if snap == nil {
				continue
			}

			// Only trade when basis is significantly positive (contango) and elevated.
			if snap.CurrentBps < cfg.MinBasisBps {
				continue
			}
			if snap.ZScore < cfg.MinZScore {
				continue
			}

			cooldownKey := fmt.Sprintf("basis:%s:%s", sym, ven)
			if cooldown != nil && cooldown.IsOnCooldown(cooldownKey, now) {
				continue
			}

			log.Printf("arb: BASIS opportunity sym=%s venue=%s basis=%.1fbps z=%.2f mean=%.1fbps",
				sym, ven, snap.CurrentBps, snap.ZScore, snap.MeanBps)

			// Basis convergence trade:
			// - Buy spot (at index price) — expects basis to narrow
			// - Sell futures (at mark price) — lock in the premium
			intent := TradeIntent{
				TenantID:  tenantID,
				IntentID:  newUUID(),
				Strategy:  StrategyBasisTrade,
				Symbol:    sym,
				TsMs:      now,
				ExpiresMs: now + cfg.IntentTTLMs,
				Legs: []TradeLeg{
					{
						Venue:          ven,
						Action:         "BUY",
						Market:         "SPOT",
						Type:           "MARKET_OR_IOC",
						NotionalUSD:    cfg.MaxNotionalUSD,
						MaxSlippageBps: cfg.MaxSlippageBps,
						PriceLimit:     snap.IndexPx * 1.003,
					},
					{
						Venue:          ven,
						Action:         "SELL",
						Market:         "PERP",
						Type:           "MARKET_OR_IOC",
						NotionalUSD:    cfg.MaxNotionalUSD,
						MaxSlippageBps: cfg.MaxSlippageBps,
						PriceLimit:     snap.MarkPx * 0.997,
					},
				},
				Expected: ExpectedMetrics{
					EdgeBpsGross: snap.CurrentBps,
					EdgeBpsNet:   snap.CurrentBps - 8, // ~8 bps round-trip fees
					ProfitUSDNet: cfg.MaxNotionalUSD * (snap.CurrentBps - 8) / 10000,
				},
				Constraints: IntentConstraints{
					MinQuality:      "MED",
					RequireVenueOK:  true,
					MaxAgeMs:        cfg.IntentTTLMs,
					HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
					CooldownKey:     cooldownKey,
				},
				Debug: IntentDebug{
					BuyOn:    ven + ":SPOT",
					SellOn:   ven + ":PERP",
					BuyExec:  snap.IndexPx,
					SellExec: snap.MarkPx,
				},
			}
			intents = append(intents, intent)
			if cooldown != nil {
				cooldown.Mark(cooldownKey, now)
			}
		}
	}
	return intents
}

func (w *basisWindow) count() int {
	if w.full {
		return w.cap
	}
	return w.idx
}

func (w *basisWindow) sorted() []basisObs {
	n := w.count()
	result := make([]basisObs, 0, n)
	if w.full {
		result = append(result, w.obs[w.idx:]...)
		result = append(result, w.obs[:w.idx]...)
	} else {
		result = append(result, w.obs[:w.idx]...)
	}
	return result
}
