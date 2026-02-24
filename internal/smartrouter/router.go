// Package smartrouter provides intelligent order routing that chooses the
// optimal execution venue, timing, and strategy for each trade leg based on
// real-time book depth, latency, reliability, and fill probability.
package smartrouter

import (
	"math"
	"sort"
	"sync"
	"time"
)

// VenueScore is the real-time execution quality score for one venue.
type VenueScore struct {
	Venue          string  `json:"venue"`
	FillProbability float64 `json:"fill_probability"` // 0..1
	SlippageBps    float64 `json:"slippage_bps"`
	LatencyMs      float64 `json:"latency_ms"`
	ReliabilityPct float64 `json:"reliability_pct"` // recent fill success rate
	DepthUSD       float64 `json:"depth_usd"`
	Score          float64 `json:"score"`
}

// RoutingDecision is the output of the smart router.
type RoutingDecision struct {
	Symbol         string            `json:"symbol"`
	Strategy       ExecutionStrategy `json:"strategy"`
	Legs           []RoutedLeg       `json:"legs"`
	EstPnLBps      float64           `json:"est_pnl_bps"`
	Confidence     float64           `json:"confidence"`
	TsMs           int64             `json:"ts_ms"`
}

// RoutedLeg is a single leg with venue chosen by the router.
type RoutedLeg struct {
	Venue       string          `json:"venue"`
	Action      string          `json:"action"`      // BUY or SELL
	OrderType   OrderType       `json:"order_type"`   // MARKET, LIMIT, IOC
	SizeUSD     float64         `json:"size_usd"`
	PriceLimit  float64         `json:"price_limit"`
	Priority    int             `json:"priority"`      // execution order (1 = first)
	Scores      VenueScore      `json:"scores"`
}

// ExecutionStrategy determines how legs are sequenced.
type ExecutionStrategy string

const (
	StrategySimultaneous ExecutionStrategy = "SIMULTANEOUS"
	StrategyHedgeFirst   ExecutionStrategy = "HEDGE_FIRST"
	StrategyAggrFirst    ExecutionStrategy = "AGGRESSIVE_FIRST"
	StrategyPassive      ExecutionStrategy = "PASSIVE"
)

// OrderType determines the order mechanics.
type OrderType string

const (
	OrderMarket OrderType = "MARKET"
	OrderLimit  OrderType = "LIMIT"
	OrderIOC    OrderType = "IOC"
)

// VenueStats tracks recent execution statistics for a venue.
type VenueStats struct {
	Venue          string
	FillCount      int
	RejectCount    int
	AvgSlippageBps float64
	AvgLatencyMs   float64
	ReliabilityPct float64
	LastUpdateMs   int64
}

// FillRecord tracks one historical fill for stats computation.
type FillRecord struct {
	Venue       string
	TsMs        int64
	SlippageBps float64
	LatencyMs   float64
	Filled      bool
}

// Router chooses optimal execution venues and strategies.
type Router struct {
	mu          sync.RWMutex
	fillHistory map[string][]FillRecord // venue → recent fills
	cfg         RouterConfig
	maxHistory  int
}

// RouterConfig controls router behaviour.
type RouterConfig struct {
	FillProbWeight     float64 `yaml:"fill_prob_weight"`
	SlippageWeight     float64 `yaml:"slippage_weight"`
	LatencyWeight      float64 `yaml:"latency_weight"`
	ReliabilityWeight  float64 `yaml:"reliability_weight"`
	DepthWeight        float64 `yaml:"depth_weight"`
	DepthRefUSD        float64 `yaml:"depth_ref_usd"`
	MaxSlippageBps     float64 `yaml:"max_slippage_bps"`
	PassiveSpreadMinBps float64 `yaml:"passive_spread_min_bps"` // min spread for passive strategy
	HistoryWindowMs    int64   `yaml:"history_window_ms"`
}

// DefaultRouterConfig returns sensible defaults.
func DefaultRouterConfig() RouterConfig {
	return RouterConfig{
		FillProbWeight:     0.30,
		SlippageWeight:     0.25,
		LatencyWeight:      0.20,
		ReliabilityWeight:  0.15,
		DepthWeight:        0.10,
		DepthRefUSD:        500000,
		MaxSlippageBps:     15,
		PassiveSpreadMinBps: 3.0,
		HistoryWindowMs:    300000, // 5 minutes
	}
}

// NewRouter creates a smart order router.
func NewRouter(cfg RouterConfig) *Router {
	return &Router{
		fillHistory: make(map[string][]FillRecord),
		cfg:         cfg,
		maxHistory:  1000,
	}
}

// LegRequest describes what needs to be executed.
type LegRequest struct {
	Action          string   // BUY or SELL
	SizeUSD         float64
	CandidateVenues []string // venues to consider
	MaxSlippageBps  float64
}

// VenueDepth provides real-time depth information for routing decisions.
type VenueDepth struct {
	Venue       string
	BidDepthUSD float64
	AskDepthUSD float64
	BestBid     float64
	BestAsk     float64
	SpreadBps   float64
	LatencyUs   int64
}

// Route produces a routing decision for a set of legs.
func (r *Router) Route(symbol string, legs []LegRequest, depths map[string]VenueDepth) *RoutingDecision {
	now := time.Now().UnixMilli()

	var routedLegs []RoutedLeg
	usedVenues := make(map[string]bool)

	for _, leg := range legs {
		best := r.selectVenue(leg, depths, usedVenues)
		if best == nil {
			continue
		}
		routedLegs = append(routedLegs, *best)
		usedVenues[best.Venue] = true
	}

	if len(routedLegs) == 0 {
		return nil
	}

	strategy := r.chooseStrategy(routedLegs, depths)
	r.assignPriorities(routedLegs, strategy, depths)

	var totalEdge float64
	for _, l := range routedLegs {
		totalEdge -= l.Scores.SlippageBps
	}

	return &RoutingDecision{
		Symbol:     symbol,
		Strategy:   strategy,
		Legs:       routedLegs,
		EstPnLBps:  totalEdge,
		Confidence: r.routeConfidence(routedLegs),
		TsMs:       now,
	}
}

func (r *Router) selectVenue(leg LegRequest, depths map[string]VenueDepth, used map[string]bool) *RoutedLeg {
	var candidates []VenueScore

	for _, v := range leg.CandidateVenues {
		if used[v] {
			continue
		}
		d, ok := depths[v]
		if !ok {
			continue
		}

		var relevantDepth float64
		if leg.Action == "BUY" {
			relevantDepth = d.AskDepthUSD
		} else {
			relevantDepth = d.BidDepthUSD
		}

		fillProb := r.estimateFillProbability(v, leg.SizeUSD, relevantDepth)
		slipEst := r.estimateSlippage(v, leg.SizeUSD, relevantDepth, d.SpreadBps)
		stats := r.venueStats(v)

		score := r.scoreVenue(fillProb, slipEst, float64(d.LatencyUs)/1000, stats.ReliabilityPct, relevantDepth)

		candidates = append(candidates, VenueScore{
			Venue:           v,
			FillProbability: fillProb,
			SlippageBps:     slipEst,
			LatencyMs:       float64(d.LatencyUs) / 1000,
			ReliabilityPct:  stats.ReliabilityPct,
			DepthUSD:        relevantDepth,
			Score:           score,
		})
	}

	if len(candidates) == 0 {
		return nil
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	best := candidates[0]

	if best.SlippageBps > leg.MaxSlippageBps && best.SlippageBps > r.cfg.MaxSlippageBps {
		return nil
	}

	orderType := OrderIOC
	d := depths[best.Venue]
	if d.SpreadBps >= r.cfg.PassiveSpreadMinBps && best.FillProbability > 0.8 {
		orderType = OrderLimit
	}

	var priceLimit float64
	if leg.Action == "BUY" {
		priceLimit = d.BestAsk * (1 + leg.MaxSlippageBps/10000)
	} else {
		priceLimit = d.BestBid * (1 - leg.MaxSlippageBps/10000)
	}

	return &RoutedLeg{
		Venue:      best.Venue,
		Action:     leg.Action,
		OrderType:  orderType,
		SizeUSD:    leg.SizeUSD,
		PriceLimit: priceLimit,
		Scores:     best,
	}
}

func (r *Router) scoreVenue(fillProb, slipBps, latencyMs, reliabilityPct, depthUSD float64) float64 {
	cfg := r.cfg

	fillScore := fillProb
	slipScore := math.Max(0, 1-slipBps/cfg.MaxSlippageBps)
	latScore := math.Max(0, 1-latencyMs/500) // 500ms = worst
	relScore := reliabilityPct / 100
	depthScore := math.Min(1, depthUSD/cfg.DepthRefUSD)

	return cfg.FillProbWeight*fillScore +
		cfg.SlippageWeight*slipScore +
		cfg.LatencyWeight*latScore +
		cfg.ReliabilityWeight*relScore +
		cfg.DepthWeight*depthScore
}

func (r *Router) estimateFillProbability(venue string, sizeUSD, depthUSD float64) float64 {
	if depthUSD == 0 {
		return 0
	}
	ratio := sizeUSD / depthUSD
	if ratio > 1 {
		return 0.1
	}

	// Historical fill rate
	stats := r.venueStats(venue)
	histProb := stats.ReliabilityPct / 100
	if histProb == 0 {
		histProb = 0.8
	}

	depthProb := math.Max(0.1, 1-ratio*ratio)
	return 0.5*depthProb + 0.5*histProb
}

func (r *Router) estimateSlippage(venue string, sizeUSD, depthUSD, spreadBps float64) float64 {
	if depthUSD == 0 {
		return 100
	}

	// Base: half-spread
	base := spreadBps / 2

	// Impact: proportional to size/depth
	impact := (sizeUSD / depthUSD) * 20 // 20 bps at full depth

	// Historical: use actual observed slippage if available
	stats := r.venueStats(venue)
	if stats.FillCount > 5 {
		return 0.4*stats.AvgSlippageBps + 0.6*(base+impact)
	}

	return base + impact
}

func (r *Router) venueStats(venue string) VenueStats {
	r.mu.RLock()
	defer r.mu.RUnlock()

	history := r.fillHistory[venue]
	cutoff := time.Now().UnixMilli() - r.cfg.HistoryWindowMs

	var fills, rejects int
	var totalSlip, totalLat float64

	for _, f := range history {
		if f.TsMs < cutoff {
			continue
		}
		if f.Filled {
			fills++
			totalSlip += f.SlippageBps
			totalLat += f.LatencyMs
		} else {
			rejects++
		}
	}

	total := fills + rejects
	reliability := 80.0 // default
	avgSlip := 3.0      // default
	avgLat := 50.0      // default

	if total > 0 {
		reliability = float64(fills) / float64(total) * 100
	}
	if fills > 0 {
		avgSlip = totalSlip / float64(fills)
		avgLat = totalLat / float64(fills)
	}

	return VenueStats{
		Venue:          venue,
		FillCount:      fills,
		RejectCount:    rejects,
		AvgSlippageBps: avgSlip,
		AvgLatencyMs:   avgLat,
		ReliabilityPct: reliability,
		LastUpdateMs:   time.Now().UnixMilli(),
	}
}

// RecordFill records an execution result for venue statistics.
func (r *Router) RecordFill(rec FillRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fillHistory[rec.Venue] = append(r.fillHistory[rec.Venue], rec)

	// Trim old
	cutoff := time.Now().UnixMilli() - r.cfg.HistoryWindowMs
	history := r.fillHistory[rec.Venue]
	start := 0
	for start < len(history) && history[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		r.fillHistory[rec.Venue] = history[start:]
	}
	if len(r.fillHistory[rec.Venue]) > r.maxHistory {
		r.fillHistory[rec.Venue] = r.fillHistory[rec.Venue][len(r.fillHistory[rec.Venue])-r.maxHistory:]
	}
}

func (r *Router) chooseStrategy(legs []RoutedLeg, depths map[string]VenueDepth) ExecutionStrategy {
	if len(legs) <= 1 {
		return StrategySimultaneous
	}

	// Check if passive (wide spread) strategy is viable
	allPassive := true
	for _, l := range legs {
		if l.OrderType != OrderLimit {
			allPassive = false
			break
		}
	}
	if allPassive {
		return StrategyPassive
	}

	// Check depth asymmetry — hedge the thinner side first
	var minDepth float64 = math.MaxFloat64
	var thinLeg int
	for i, l := range legs {
		d := depths[l.Venue]
		var relevantDepth float64
		if l.Action == "BUY" {
			relevantDepth = d.AskDepthUSD
		} else {
			relevantDepth = d.BidDepthUSD
		}
		if relevantDepth < minDepth {
			minDepth = relevantDepth
			thinLeg = i
		}
	}

	// If the thin leg is less than 50% of the thick leg, hedge first
	var maxDepth float64
	for _, l := range legs {
		d := depths[l.Venue]
		var relevantDepth float64
		if l.Action == "BUY" {
			relevantDepth = d.AskDepthUSD
		} else {
			relevantDepth = d.BidDepthUSD
		}
		if relevantDepth > maxDepth {
			maxDepth = relevantDepth
		}
	}

	if maxDepth > 0 && minDepth/maxDepth < 0.5 {
		_ = thinLeg // thin side executes first via priority assignment
		return StrategyHedgeFirst
	}

	return StrategySimultaneous
}

func (r *Router) assignPriorities(legs []RoutedLeg, strategy ExecutionStrategy, depths map[string]VenueDepth) {
	switch strategy {
	case StrategySimultaneous:
		for i := range legs {
			legs[i].Priority = 1
		}
	case StrategyHedgeFirst:
		// Thinner book side gets priority 1 (first)
		type depthIdx struct {
			depth float64
			idx   int
		}
		var di []depthIdx
		for i, l := range legs {
			d := depths[l.Venue]
			relevantDepth := d.BidDepthUSD
			if l.Action == "BUY" {
				relevantDepth = d.AskDepthUSD
			}
			di = append(di, depthIdx{depth: relevantDepth, idx: i})
		}
		sort.Slice(di, func(a, b int) bool { return di[a].depth < di[b].depth })
		for pri, d := range di {
			legs[d.idx].Priority = pri + 1
		}
	case StrategyAggrFirst:
		for i := range legs {
			if legs[i].Action == "BUY" {
				legs[i].Priority = 1
			} else {
				legs[i].Priority = 2
			}
		}
	case StrategyPassive:
		for i := range legs {
			legs[i].Priority = 1
		}
	}
}

func (r *Router) routeConfidence(legs []RoutedLeg) float64 {
	if len(legs) == 0 {
		return 0
	}
	var totalFP float64
	for _, l := range legs {
		totalFP += l.Scores.FillProbability
	}
	return totalFP / float64(len(legs))
}
