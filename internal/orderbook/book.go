// Package orderbook provides L2 order book aggregation, synthetic book
// construction, liquidity flow detection, and leader/follower venue analysis.
package orderbook

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Level represents a single price level in an order book.
type Level struct {
	Price float64 `json:"price"`
	Qty   float64 `json:"qty"` // in base asset
}

// VenueBook holds the full L2 order book for one venue+symbol.
type VenueBook struct {
	Venue     string  `json:"venue"`
	Symbol    string  `json:"symbol"`
	Bids      []Level `json:"bids"` // descending by price
	Asks      []Level `json:"asks"` // ascending by price
	TsMs      int64   `json:"ts_ms"`
	LatencyUs int64   `json:"latency_us"` // feed latency in microseconds
}

// SyntheticBook is a merged view of all venue books for a symbol.
type SyntheticBook struct {
	Symbol     string               `json:"symbol"`
	Bids       []Level              `json:"bids"` // merged, descending
	Asks       []Level              `json:"asks"` // merged, ascending
	VenueDepth map[string]DepthInfo `json:"venue_depth"`
	TsMs       int64                `json:"ts_ms"`
}

// DepthInfo summarises a single venue's book depth.
type DepthInfo struct {
	Venue          string  `json:"venue"`
	BidDepthUSD    float64 `json:"bid_depth_usd"`    // total bid-side notional
	AskDepthUSD    float64 `json:"ask_depth_usd"`    // total ask-side notional
	BestBid        float64 `json:"best_bid"`
	BestAsk        float64 `json:"best_ask"`
	Spread         float64 `json:"spread_bps"`
	Imbalance      float64 `json:"imbalance"`        // -1..+1 bid-heavy to ask-heavy
	Levels         int     `json:"levels"`
	LatencyUs      int64   `json:"latency_us"`
}

// SlippageCurve maps order sizes to expected slippage in bps.
type SlippageCurve struct {
	Venue  string          `json:"venue"`
	Symbol string          `json:"symbol"`
	Buy    []SlippagePoint `json:"buy"`
	Sell   []SlippagePoint `json:"sell"`
}

// SlippagePoint represents expected slippage for a given order size.
type SlippagePoint struct {
	SizeUSD     float64 `json:"size_usd"`
	SlippageBps float64 `json:"slippage_bps"`
	FillPct     float64 `json:"fill_pct"` // 0..100 how much can fill at this level
}

// Aggregator maintains L2 order books for all venue+symbol pairs and
// produces synthetic merged books on demand.
type Aggregator struct {
	mu    sync.RWMutex
	books map[string]map[string]*VenueBook // symbol → venue → book
}

// NewAggregator creates a new order book aggregator.
func NewAggregator() *Aggregator {
	return &Aggregator{
		books: make(map[string]map[string]*VenueBook),
	}
}

// Update replaces the order book for a venue+symbol pair.
func (a *Aggregator) Update(book VenueBook) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.books[book.Symbol] == nil {
		a.books[book.Symbol] = make(map[string]*VenueBook)
	}
	a.books[book.Symbol][book.Venue] = &book
}

// SyntheticFor returns a merged order book for the given symbol.
func (a *Aggregator) SyntheticFor(symbol string) *SyntheticBook {
	a.mu.RLock()
	defer a.mu.RUnlock()

	venueBooks := a.books[symbol]
	if len(venueBooks) == 0 {
		return nil
	}

	now := time.Now().UnixMilli()
	sb := &SyntheticBook{
		Symbol:     symbol,
		TsMs:       now,
		VenueDepth: make(map[string]DepthInfo, len(venueBooks)),
	}

	for venue, vb := range venueBooks {
		// Skip books older than 5 seconds
		if now-vb.TsMs > 5000 {
			continue
		}

		var bidUSD, askUSD float64
		for _, l := range vb.Bids {
			bidUSD += l.Price * l.Qty
			sb.Bids = append(sb.Bids, Level{Price: l.Price, Qty: l.Qty})
		}
		for _, l := range vb.Asks {
			askUSD += l.Price * l.Qty
			sb.Asks = append(sb.Asks, Level{Price: l.Price, Qty: l.Qty})
		}

		var bestBid, bestAsk, spreadBps float64
		if len(vb.Bids) > 0 {
			bestBid = vb.Bids[0].Price
		}
		if len(vb.Asks) > 0 {
			bestAsk = vb.Asks[0].Price
		}
		if bestBid > 0 && bestAsk > 0 {
			mid := (bestBid + bestAsk) / 2
			spreadBps = (bestAsk - bestBid) / mid * 10000
		}

		totalUSD := bidUSD + askUSD
		imbalance := 0.0
		if totalUSD > 0 {
			imbalance = (bidUSD - askUSD) / totalUSD // -1 ask-heavy, +1 bid-heavy
		}

		sb.VenueDepth[venue] = DepthInfo{
			Venue:       venue,
			BidDepthUSD: bidUSD,
			AskDepthUSD: askUSD,
			BestBid:     bestBid,
			BestAsk:     bestAsk,
			Spread:      spreadBps,
			Imbalance:   imbalance,
			Levels:      len(vb.Bids) + len(vb.Asks),
			LatencyUs:   vb.LatencyUs,
		}
	}

	// Sort merged levels
	sort.Slice(sb.Bids, func(i, j int) bool { return sb.Bids[i].Price > sb.Bids[j].Price })
	sort.Slice(sb.Asks, func(i, j int) bool { return sb.Asks[i].Price < sb.Asks[j].Price })

	return sb
}

// EstimateSlippage computes expected slippage for executing a given notional
// on a specific venue's book. Returns slippage in bps and fill percentage.
func (a *Aggregator) EstimateSlippage(symbol, venue string, sizeUSD float64, isBuy bool) (slippageBps float64, fillPct float64) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	vb := a.books[symbol][venue]
	if vb == nil {
		return 100, 0 // no book → max slippage, no fill
	}

	var levels []Level
	if isBuy {
		levels = vb.Asks
	} else {
		levels = vb.Bids
	}

	if len(levels) == 0 {
		return 100, 0
	}

	refPrice := levels[0].Price
	var filled, weightedPrice float64
	for _, l := range levels {
		levelUSD := l.Price * l.Qty
		take := math.Min(levelUSD, sizeUSD-filled)
		weightedPrice += l.Price * take
		filled += take
		if filled >= sizeUSD {
			break
		}
	}

	if filled == 0 {
		return 100, 0
	}

	avgPrice := weightedPrice / filled
	slippageBps = math.Abs(avgPrice-refPrice) / refPrice * 10000
	fillPct = filled / sizeUSD * 100
	if fillPct > 100 {
		fillPct = 100
	}
	return slippageBps, fillPct
}

// SlippageCurveFor computes a full slippage curve for a venue+symbol across
// standard order sizes.
func (a *Aggregator) SlippageCurveFor(symbol, venue string) SlippageCurve {
	sizes := []float64{1000, 5000, 10000, 25000, 50000, 100000, 250000, 500000}
	sc := SlippageCurve{Venue: venue, Symbol: symbol}
	for _, sz := range sizes {
		buySlip, buyFill := a.EstimateSlippage(symbol, venue, sz, true)
		sc.Buy = append(sc.Buy, SlippagePoint{SizeUSD: sz, SlippageBps: buySlip, FillPct: buyFill})
		sellSlip, sellFill := a.EstimateSlippage(symbol, venue, sz, false)
		sc.Sell = append(sc.Sell, SlippagePoint{SizeUSD: sz, SlippageBps: sellSlip, FillPct: sellFill})
	}
	return sc
}

// BestExecutionVenue returns the venue with lowest slippage for a given size.
func (a *Aggregator) BestExecutionVenue(symbol string, sizeUSD float64, isBuy bool) (bestVenue string, bestSlip float64) {
	a.mu.RLock()
	venues := make([]string, 0, len(a.books[symbol]))
	for v := range a.books[symbol] {
		venues = append(venues, v)
	}
	a.mu.RUnlock()

	bestSlip = math.MaxFloat64
	for _, v := range venues {
		slip, fill := a.EstimateSlippage(symbol, v, sizeUSD, isBuy)
		if fill >= 90 && slip < bestSlip {
			bestSlip = slip
			bestVenue = v
		}
	}
	return bestVenue, bestSlip
}
