// Package triangular implements single-venue triangular arbitrage detection.
// It exploits cross-rate inconsistencies among three trading pairs on the same
// exchange (e.g., BTC/USDT, ETH/BTC, ETH/USDT).
package triangular

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Triangle defines a three-pair arbitrage cycle.
type Triangle struct {
	Name    string `yaml:"name"`    // e.g. "BTC_ETH_USDT"
	PairAB  string `yaml:"pair_ab"` // e.g. "BTC/USDT"
	PairBC  string `yaml:"pair_bc"` // e.g. "ETH/BTC"
	PairCA  string `yaml:"pair_ca"` // e.g. "ETH/USDT"
	Venue   string `yaml:"venue"`
}

// PairQuote holds the top-of-book for one pair.
type PairQuote struct {
	Symbol  string  `json:"symbol"`
	Venue   string  `json:"venue"`
	Bid     float64 `json:"bid"`
	Ask     float64 `json:"ask"`
	BidQty  float64 `json:"bid_qty"` // size at best bid
	AskQty  float64 `json:"ask_qty"` // size at best ask
	TsMs    int64   `json:"ts_ms"`
}

// Opportunity describes a detected triangular arbitrage.
type Opportunity struct {
	Triangle    string  `json:"triangle"`
	Venue       string  `json:"venue"`
	Direction   string  `json:"direction"` // "FORWARD" or "REVERSE"
	GrossEdgeBps float64 `json:"gross_edge_bps"`
	NetEdgeBps  float64 `json:"net_edge_bps"` // after fees
	NotionalUSD float64 `json:"notional_usd"`
	Steps       [3]Step `json:"steps"`
	TsMs        int64   `json:"ts_ms"`
}

// Step is one leg of the triangular trade.
type Step struct {
	Pair   string  `json:"pair"`
	Action string  `json:"action"` // BUY or SELL
	Price  float64 `json:"price"`
	Qty    float64 `json:"qty"`
}

// Config controls triangular arbitrage detection.
type Config struct {
	Triangles      []Triangle `yaml:"triangles"`
	MinEdgeBps     float64    `yaml:"min_edge_bps"`      // minimum net edge
	MaxSlippageBps float64    `yaml:"max_slippage_bps"`
	FeeBpsTaker    float64    `yaml:"fee_bps_taker"`     // per-leg taker fee
	MaxNotionalUSD float64    `yaml:"max_notional_usd"`
	CooldownMs     int64      `yaml:"cooldown_ms"`
	StaleMs        int64      `yaml:"stale_ms"`
}

// Engine detects triangular arbitrage opportunities.
type Engine struct {
	mu        sync.RWMutex
	cfg       Config
	quotes    map[string]*PairQuote // "venue:symbol" → quote
	lastEmit  map[string]int64      // triangle name → last emit ts
}

// NewEngine creates a triangular arbitrage engine.
func NewEngine(cfg Config) *Engine {
	return &Engine{
		cfg:      cfg,
		quotes:   make(map[string]*PairQuote),
		lastEmit: make(map[string]int64),
	}
}

// UpdateQuote records the latest quote for a pair.
func (e *Engine) UpdateQuote(q PairQuote) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := q.Venue + ":" + q.Symbol
	e.quotes[key] = &q
}

// Scan checks all configured triangles for opportunities.
func (e *Engine) Scan() []Opportunity {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UnixMilli()
	var opps []Opportunity

	for _, tri := range e.cfg.Triangles {
		if now-e.lastEmit[tri.Name] < e.cfg.CooldownMs {
			continue
		}

		qAB := e.quotes[tri.Venue+":"+tri.PairAB]
		qBC := e.quotes[tri.Venue+":"+tri.PairBC]
		qCA := e.quotes[tri.Venue+":"+tri.PairCA]

		if qAB == nil || qBC == nil || qCA == nil {
			continue
		}
		if now-qAB.TsMs > e.cfg.StaleMs || now-qBC.TsMs > e.cfg.StaleMs || now-qCA.TsMs > e.cfg.StaleMs {
			continue
		}

		// Forward: buy AB, buy BC, sell CA
		// Start with 1 unit of quote currency (e.g., USDT)
		// Step 1: buy A with USDT → get A
		// Step 2: buy C with A (sell A for C via BC pair)
		// Step 3: sell C for USDT
		fwdRate := (1.0 / qAB.Ask) * (1.0 / qBC.Ask) * qCA.Bid
		fwdEdgeBps := (fwdRate - 1) * 10000
		fwdFees := e.cfg.FeeBpsTaker * 3 // 3 legs
		fwdNetBps := fwdEdgeBps - fwdFees

		// Reverse: sell AB, sell BC, buy CA
		revRate := qAB.Bid * qBC.Bid * (1.0 / qCA.Ask)
		revEdgeBps := (revRate - 1) * 10000
		revFees := e.cfg.FeeBpsTaker * 3
		revNetBps := revEdgeBps - revFees

		if fwdNetBps >= e.cfg.MinEdgeBps {
			// Size by smallest available quantity across the 3 legs
			maxQty := e.triangleSize(qAB, qBC, qCA, true)
			notional := math.Min(maxQty*qAB.Ask, e.cfg.MaxNotionalUSD)

			opps = append(opps, Opportunity{
				Triangle:     tri.Name,
				Venue:        tri.Venue,
				Direction:    "FORWARD",
				GrossEdgeBps: fwdEdgeBps,
				NetEdgeBps:   fwdNetBps,
				NotionalUSD:  notional,
				Steps: [3]Step{
					{Pair: tri.PairAB, Action: "BUY", Price: qAB.Ask, Qty: notional / qAB.Ask},
					{Pair: tri.PairBC, Action: "BUY", Price: qBC.Ask},
					{Pair: tri.PairCA, Action: "SELL", Price: qCA.Bid},
				},
				TsMs: now,
			})
			e.lastEmit[tri.Name] = now
		}

		if revNetBps >= e.cfg.MinEdgeBps {
			maxQty := e.triangleSize(qAB, qBC, qCA, false)
			notional := math.Min(maxQty*qAB.Bid, e.cfg.MaxNotionalUSD)

			opps = append(opps, Opportunity{
				Triangle:     tri.Name,
				Venue:        tri.Venue,
				Direction:    "REVERSE",
				GrossEdgeBps: revEdgeBps,
				NetEdgeBps:   revNetBps,
				NotionalUSD:  notional,
				Steps: [3]Step{
					{Pair: tri.PairAB, Action: "SELL", Price: qAB.Bid, Qty: notional / qAB.Bid},
					{Pair: tri.PairBC, Action: "SELL", Price: qBC.Bid},
					{Pair: tri.PairCA, Action: "BUY", Price: qCA.Ask},
				},
				TsMs: now,
			})
			e.lastEmit[tri.Name] = now
		}
	}

	return opps
}

func (e *Engine) triangleSize(qAB, qBC, qCA *PairQuote, forward bool) float64 {
	if forward {
		return math.Min(qAB.AskQty, math.Min(qBC.AskQty, qCA.BidQty))
	}
	return math.Min(qAB.BidQty, math.Min(qBC.BidQty, qCA.AskQty))
}

// Stats returns detection statistics.
func (e *Engine) Stats() map[string]int64 {
	e.mu.RLock()
	defer e.mu.RUnlock()
	result := make(map[string]int64, len(e.lastEmit))
	for k, v := range e.lastEmit {
		result[k] = v
	}
	return result
}

// ToIntentID generates a unique intent ID for a triangular opportunity.
func (o *Opportunity) ToIntentID() string {
	return fmt.Sprintf("tri-%s-%s-%d", o.Triangle, o.Direction, o.TsMs)
}
