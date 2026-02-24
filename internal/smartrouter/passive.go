package smartrouter

import (
	"math"
	"sync"
	"time"
)

// PassiveOrder represents a maker order placed to capture spread.
type PassiveOrder struct {
	OrderID    string    `json:"order_id"`
	Venue      string    `json:"venue"`
	Symbol     string    `json:"symbol"`
	Side       string    `json:"side"` // BUY or SELL
	Price      float64   `json:"price"`
	SizeUSD    float64   `json:"size_usd"`
	Status     string    `json:"status"` // PENDING, OPEN, FILLED, CANCELLED
	PlacedMs   int64     `json:"placed_ms"`
	FilledMs   int64     `json:"filled_ms"`
	FilledPct  float64   `json:"filled_pct"`
}

// PassiveOpportunity describes when passive liquidity provision is profitable.
type PassiveOpportunity struct {
	Symbol       string  `json:"symbol"`
	Venue        string  `json:"venue"`
	Side         string  `json:"side"`
	Price        float64 `json:"price"`
	SpreadBps    float64 `json:"spread_bps"`
	EdgeBps      float64 `json:"edge_bps"`     // spread/2 minus fees
	DepthUSD     float64 `json:"depth_usd"`
	FillProbPct  float64 `json:"fill_prob_pct"` // estimated fill probability
	SizeUSD      float64 `json:"size_usd"`
	Confidence   float64 `json:"confidence"`
}

// PassiveConfig controls passive liquidity provision.
type PassiveConfig struct {
	MinSpreadBps       float64 `yaml:"min_spread_bps"`        // minimum spread to provide liquidity
	MinEdgeBps         float64 `yaml:"min_edge_bps"`          // minimum edge after fees
	MaxOrderSizeUSD    float64 `yaml:"max_order_size_usd"`    // cap per passive order
	MaxOutstandingUSD  float64 `yaml:"max_outstanding_usd"`   // total passive exposure cap
	FeeBpsMaker        float64 `yaml:"fee_bps_maker"`         // maker fee (often negative = rebate)
	StaleMs            int64   `yaml:"stale_ms"`              // how long to keep passive orders
	MaxAdverseMoveBps  float64 `yaml:"max_adverse_move_bps"`  // cancel if price moves against us
}

// DefaultPassiveConfig returns sensible defaults.
func DefaultPassiveConfig() PassiveConfig {
	return PassiveConfig{
		MinSpreadBps:      3.0,
		MinEdgeBps:        1.0,
		MaxOrderSizeUSD:   10000,
		MaxOutstandingUSD: 50000,
		FeeBpsMaker:       -0.5, // rebate
		StaleMs:           5000,
		MaxAdverseMoveBps: 5.0,
	}
}

// PassiveEngine manages passive liquidity provision across venues.
type PassiveEngine struct {
	mu              sync.RWMutex
	cfg             PassiveConfig
	activeOrders    map[string]*PassiveOrder // orderID → order
	outstandingUSD  float64
}

// NewPassiveEngine creates a passive liquidity engine.
func NewPassiveEngine(cfg PassiveConfig) *PassiveEngine {
	return &PassiveEngine{
		cfg:          cfg,
		activeOrders: make(map[string]*PassiveOrder),
	}
}

// FindOpportunities scans venue data for passive liquidity opportunities.
func (pe *PassiveEngine) FindOpportunities(symbol string, depths map[string]VenueDepth, flowScore float64) []PassiveOpportunity {
	pe.mu.RLock()
	headroom := pe.cfg.MaxOutstandingUSD - pe.outstandingUSD
	pe.mu.RUnlock()

	if headroom <= 0 {
		return nil
	}

	var opps []PassiveOpportunity

	for venue, d := range depths {
		if d.SpreadBps < pe.cfg.MinSpreadBps {
			continue
		}

		// Edge = half-spread minus maker fee (fee is often negative = rebate)
		edgeBps := d.SpreadBps/2 - pe.cfg.FeeBpsMaker
		if edgeBps < pe.cfg.MinEdgeBps {
			continue
		}

		// Determine which side to provide based on flow
		side := "BUY"
		price := d.BestBid
		depthUSD := d.BidDepthUSD
		if flowScore > 0.3 {
			// Strong buy flow: provide on ask side (sell)
			side = "SELL"
			price = d.BestAsk
			depthUSD = d.AskDepthUSD
		} else if flowScore < -0.3 {
			// Strong sell flow: provide on bid side (buy)
			side = "BUY"
			price = d.BestBid
			depthUSD = d.BidDepthUSD
		} else {
			// Neutral: provide both sides
			// Add bid side
			opps = append(opps, pe.makeOpp(symbol, venue, "BUY", d.BestBid, d.SpreadBps, edgeBps, d.BidDepthUSD, headroom))
			// Add ask side
			opps = append(opps, pe.makeOpp(symbol, venue, "SELL", d.BestAsk, d.SpreadBps, edgeBps, d.AskDepthUSD, headroom))
			continue
		}

		opps = append(opps, pe.makeOpp(symbol, venue, side, price, d.SpreadBps, edgeBps, depthUSD, headroom))
	}

	return opps
}

func (pe *PassiveEngine) makeOpp(symbol, venue, side string, price, spreadBps, edgeBps, depthUSD, headroom float64) PassiveOpportunity {
	sizeUSD := math.Min(pe.cfg.MaxOrderSizeUSD, headroom)
	sizeUSD = math.Min(sizeUSD, depthUSD*0.1) // don't exceed 10% of depth

	fillProb := math.Max(0.1, math.Min(0.95, 1-spreadBps/20))

	confidence := fillProb * math.Min(1, depthUSD/100000)

	return PassiveOpportunity{
		Symbol:      symbol,
		Venue:       venue,
		Side:        side,
		Price:       price,
		SpreadBps:   spreadBps,
		EdgeBps:     edgeBps,
		DepthUSD:    depthUSD,
		FillProbPct: fillProb * 100,
		SizeUSD:     sizeUSD,
		Confidence:  confidence,
	}
}

// RegisterOrder tracks a placed passive order.
func (pe *PassiveEngine) RegisterOrder(order PassiveOrder) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	pe.activeOrders[order.OrderID] = &order
	pe.outstandingUSD += order.SizeUSD
}

// MarkFilled marks a passive order as filled.
func (pe *PassiveEngine) MarkFilled(orderID string) {
	pe.mu.Lock()
	defer pe.mu.Unlock()
	if o, ok := pe.activeOrders[orderID]; ok {
		o.Status = "FILLED"
		o.FilledMs = time.Now().UnixMilli()
		o.FilledPct = 100
		pe.outstandingUSD -= o.SizeUSD
		delete(pe.activeOrders, orderID)
	}
}

// CancelStale cancels orders that have been open too long.
func (pe *PassiveEngine) CancelStale() []string {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	now := time.Now().UnixMilli()
	var cancelled []string

	for id, o := range pe.activeOrders {
		if now-o.PlacedMs > pe.cfg.StaleMs {
			o.Status = "CANCELLED"
			pe.outstandingUSD -= o.SizeUSD
			cancelled = append(cancelled, id)
			delete(pe.activeOrders, id)
		}
	}

	return cancelled
}

// ActiveOrders returns all currently open passive orders.
func (pe *PassiveEngine) ActiveOrders() []PassiveOrder {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	var orders []PassiveOrder
	for _, o := range pe.activeOrders {
		orders = append(orders, *o)
	}
	return orders
}

// OutstandingUSD returns total outstanding passive exposure.
func (pe *PassiveEngine) OutstandingUSD() float64 {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return pe.outstandingUSD
}
