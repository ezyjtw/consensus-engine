package orderbook

import (
	"math"
	"sort"
	"sync"
	"time"
)

// PriceMove records a significant price change on one venue.
type PriceMove struct {
	Venue    string  `json:"venue"`
	Symbol   string  `json:"symbol"`
	TsMs     int64   `json:"ts_ms"`
	OldMid   float64 `json:"old_mid"`
	NewMid   float64 `json:"new_mid"`
	ChangeBps float64 `json:"change_bps"`
}

// LeaderStats describes how often a venue leads price discovery.
type LeaderStats struct {
	Venue           string  `json:"venue"`
	LeadCount       int     `json:"lead_count"`
	FollowCount     int     `json:"follow_count"`
	LeadPct         float64 `json:"lead_pct"`     // % of moves this venue led
	AvgLeadTimeMs   float64 `json:"avg_lead_ms"`  // average lead time over followers
	Reliability     float64 `json:"reliability"`   // consistency of leadership
}

// VenueLeaderWeight is the output used by the fair value engine.
type VenueLeaderWeight struct {
	Venue  string  `json:"venue"`
	Weight float64 `json:"weight"` // 0..1 dynamic fair value weight
}

// moveEvent is an internal record of correlated price moves across venues.
type moveEvent struct {
	symbol    string
	tsMs      int64
	leader    string
	followers map[string]int64 // venue → ts of follow move
	changeBps float64
}

// LeaderDetector identifies which venues lead price discovery by tracking
// the temporal ordering of significant price moves across venues.
type LeaderDetector struct {
	mu            sync.RWMutex
	lastMids      map[string]map[string]float64 // symbol → venue → last mid
	moves         map[string][]PriceMove        // symbol → recent moves
	events        map[string][]moveEvent         // symbol → correlated events
	minChangeBps  float64                        // minimum bps move to track
	correlWindowMs int64                         // max time between leader and follower
	windowMs      int64                          // analysis lookback window
	maxEvents     int
}

// NewLeaderDetector creates a leader/follower detector.
// minChangeBps: minimum price move to consider (e.g., 2.0 for 2 bps).
// correlWindowMs: max time (ms) for a follower to respond (e.g., 500).
func NewLeaderDetector(minChangeBps float64, correlWindowMs, windowMs int64) *LeaderDetector {
	return &LeaderDetector{
		lastMids:       make(map[string]map[string]float64),
		moves:          make(map[string][]PriceMove),
		events:         make(map[string][]moveEvent),
		minChangeBps:   minChangeBps,
		correlWindowMs: correlWindowMs,
		windowMs:       windowMs,
		maxEvents:      2000,
	}
}

// RecordPrice records a new mid price observation for a venue+symbol.
// Returns the detected move if significant, nil otherwise.
func (ld *LeaderDetector) RecordPrice(venue, symbol string, mid float64, tsMs int64) *PriceMove {
	ld.mu.Lock()
	defer ld.mu.Unlock()

	if ld.lastMids[symbol] == nil {
		ld.lastMids[symbol] = make(map[string]float64)
	}

	lastMid := ld.lastMids[symbol][venue]
	ld.lastMids[symbol][venue] = mid

	if lastMid == 0 {
		return nil
	}

	changeBps := (mid - lastMid) / lastMid * 10000
	if math.Abs(changeBps) < ld.minChangeBps {
		return nil
	}

	pm := &PriceMove{
		Venue:     venue,
		Symbol:    symbol,
		TsMs:      tsMs,
		OldMid:    lastMid,
		NewMid:    mid,
		ChangeBps: changeBps,
	}

	ld.moves[symbol] = append(ld.moves[symbol], *pm)

	// Try to correlate with recent moves from other venues
	ld.correlateMove(pm)

	// Trim old data
	cutoff := time.Now().UnixMilli() - ld.windowMs
	ld.trimOlderThan(symbol, cutoff)

	return pm
}

// correlateMove checks if this move was led by another venue.
func (ld *LeaderDetector) correlateMove(pm *PriceMove) {
	moves := ld.moves[pm.Symbol]
	sameDir := pm.ChangeBps > 0 // positive = up

	// Look backwards for a correlated leader move
	for i := len(moves) - 2; i >= 0; i-- {
		m := moves[i]
		if pm.TsMs-m.TsMs > ld.correlWindowMs {
			break
		}
		if m.Venue == pm.Venue {
			continue
		}
		mDir := m.ChangeBps > 0
		if mDir != sameDir {
			continue
		}

		// Found a correlated prior move from a different venue → that venue led
		event := moveEvent{
			symbol:    pm.Symbol,
			tsMs:      m.TsMs,
			leader:    m.Venue,
			followers: map[string]int64{pm.Venue: pm.TsMs},
			changeBps: m.ChangeBps,
		}
		ld.events[pm.Symbol] = append(ld.events[pm.Symbol], event)
		return
	}

	// No prior correlated move: this venue is the leader (for now)
	event := moveEvent{
		symbol:    pm.Symbol,
		tsMs:      pm.TsMs,
		leader:    pm.Venue,
		followers: make(map[string]int64),
		changeBps: pm.ChangeBps,
	}
	ld.events[pm.Symbol] = append(ld.events[pm.Symbol], event)
}

func (ld *LeaderDetector) trimOlderThan(symbol string, cutoffMs int64) {
	moves := ld.moves[symbol]
	start := 0
	for start < len(moves) && moves[start].TsMs < cutoffMs {
		start++
	}
	if start > 0 {
		ld.moves[symbol] = moves[start:]
	}

	events := ld.events[symbol]
	es := 0
	for es < len(events) && events[es].tsMs < cutoffMs {
		es++
	}
	if es > 0 {
		ld.events[symbol] = events[es:]
	}
	if len(ld.events[symbol]) > ld.maxEvents {
		ld.events[symbol] = ld.events[symbol][len(ld.events[symbol])-ld.maxEvents:]
	}
}

// Stats returns leader/follower statistics for all venues on a symbol.
func (ld *LeaderDetector) Stats(symbol string) []LeaderStats {
	ld.mu.RLock()
	defer ld.mu.RUnlock()

	events := ld.events[symbol]
	if len(events) == 0 {
		return nil
	}

	leadCount := make(map[string]int)
	followCount := make(map[string]int)
	leadTimes := make(map[string][]int64) // venue → lead times over followers

	for _, e := range events {
		leadCount[e.leader]++
		for fv, fts := range e.followers {
			followCount[fv]++
			leadTimes[e.leader] = append(leadTimes[e.leader], fts-e.tsMs)
		}
	}

	total := len(events)
	var stats []LeaderStats
	venues := make(map[string]bool)
	for v := range leadCount {
		venues[v] = true
	}
	for v := range followCount {
		venues[v] = true
	}

	for v := range venues {
		lc := leadCount[v]
		fc := followCount[v]
		pct := float64(lc) / float64(total) * 100

		var avgLead float64
		if lts := leadTimes[v]; len(lts) > 0 {
			var sum int64
			for _, lt := range lts {
				sum += lt
			}
			avgLead = float64(sum) / float64(len(lts))
		}

		// Reliability: low variance in lead time = high reliability
		reliability := 0.0
		if lts := leadTimes[v]; len(lts) > 1 {
			var sum float64
			for _, lt := range lts {
				sum += float64(lt)
			}
			mean := sum / float64(len(lts))
			var variance float64
			for _, lt := range lts {
				d := float64(lt) - mean
				variance += d * d
			}
			variance /= float64(len(lts))
			stddev := math.Sqrt(variance)
			if mean > 0 {
				cv := stddev / mean // coefficient of variation
				reliability = math.Max(0, 1-cv)
			}
		} else if lc > 0 {
			reliability = 0.5 // single observation
		}

		stats = append(stats, LeaderStats{
			Venue:         v,
			LeadCount:     lc,
			FollowCount:   fc,
			LeadPct:       pct,
			AvgLeadTimeMs: avgLead,
			Reliability:   reliability,
		})
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].LeadPct > stats[j].LeadPct })
	return stats
}

// DynamicWeights computes fair-value weights based on leadership, latency,
// depth, and reliability. These are used by the fair value engine.
func (ld *LeaderDetector) DynamicWeights(symbol string, depths map[string]DepthInfo) []VenueLeaderWeight {
	stats := ld.Stats(symbol)
	if len(stats) == 0 {
		return nil
	}

	// Raw score: leadership% * reliability * depth_factor * latency_factor
	raw := make(map[string]float64)
	var total float64
	for _, s := range stats {
		leaderScore := s.LeadPct / 100 * math.Max(0.1, s.Reliability)

		depthFactor := 1.0
		latencyFactor := 1.0
		if d, ok := depths[s.Venue]; ok {
			totalDepth := d.BidDepthUSD + d.AskDepthUSD
			depthFactor = math.Min(2, totalDepth/100000) // normalise to $100k
			if d.LatencyUs > 0 {
				latencyFactor = 1.0 / (1.0 + float64(d.LatencyUs)/1000000) // penalise high latency
			}
		}

		score := leaderScore * depthFactor * latencyFactor
		if score < 0.01 {
			score = 0.01 // floor
		}
		raw[s.Venue] = score
		total += score
	}

	// Normalise
	var weights []VenueLeaderWeight
	for venue, score := range raw {
		weights = append(weights, VenueLeaderWeight{
			Venue:  venue,
			Weight: score / total,
		})
	}
	sort.Slice(weights, func(i, j int) bool { return weights[i].Weight > weights[j].Weight })
	return weights
}
