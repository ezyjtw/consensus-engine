package funding

import (
	"sync"

	"github.com/ezyjtw/consensus-engine/internal/execution"
)

// PositionTracker reconstructs open funding positions from execution events.
// It tracks net notional per strategy+symbol+venue(s) combination.
type PositionTracker struct {
	mu        sync.Mutex
	positions map[string]*trackedPosition // key → position
}

type trackedPosition struct {
	pos       OpenPosition
	netBuyUSD float64 // cumulative buy notional
	netSellUSD float64 // cumulative sell notional (exit legs)
}

// NewPositionTracker creates an empty position tracker.
func NewPositionTracker() *PositionTracker {
	return &PositionTracker{
		positions: make(map[string]*trackedPosition),
	}
}

// ProcessEvent updates position state from an execution event.
// Entry events (FUNDING_CARRY, FUNDING_DIFFERENTIAL) open or increase positions.
// Exit events (FUNDING_CARRY_EXIT, FUNDING_DIFFERENTIAL_EXIT) close or reduce them.
func (pt *PositionTracker) ProcessEvent(ev execution.ExecutionEvent) {
	if ev.EventType != "ORDER_FILLED" {
		return
	}

	switch ev.Strategy {
	case StrategyFundingCarry:
		pt.processCarryEntry(ev)
	case StrategyFundingCarryExit:
		pt.processCarryExit(ev)
	case StrategyFundingDifferential:
		pt.processDiffEntry(ev)
	case StrategyFundingDiffExit:
		pt.processDiffExit(ev)
	}
}

func (pt *PositionTracker) processCarryEntry(ev execution.ExecutionEvent) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	key := "carry:" + ev.Symbol + ":" + ev.Venue
	tp, ok := pt.positions[key]
	if !ok {
		tp = &trackedPosition{
			pos: OpenPosition{
				Strategy: StrategyFundingCarry,
				Symbol:   ev.Symbol,
				Venue:    ev.Venue,
			},
		}
		pt.positions[key] = tp
	}
	tp.pos.NotionalUSD += ev.FilledNotionalUSD
	if tp.pos.EntryRateBps == 0 {
		// First fill — record entry metadata.
		tp.pos.EntryTsMs = ev.TsMs
	}
}

func (pt *PositionTracker) processCarryExit(ev execution.ExecutionEvent) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	key := "carry:" + ev.Symbol + ":" + ev.Venue
	tp, ok := pt.positions[key]
	if !ok {
		return
	}
	tp.pos.NotionalUSD -= ev.FilledNotionalUSD
	if tp.pos.NotionalUSD <= 0 {
		delete(pt.positions, key)
	}
}

func (pt *PositionTracker) processDiffEntry(ev execution.ExecutionEvent) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	// For differential, we key by intent since it spans two venues.
	// The first fill tells us one venue; the second fill tells us the other.
	// Use intent ID as a temporary grouping key.
	intentKey := "diff-intent:" + ev.IntentID
	tp, ok := pt.positions[intentKey]
	if !ok {
		tp = &trackedPosition{
			pos: OpenPosition{
				Strategy:    StrategyFundingDifferential,
				Symbol:      ev.Symbol,
				NotionalUSD: ev.FilledNotionalUSD,
				EntryTsMs:   ev.TsMs,
			},
		}
		pt.positions[intentKey] = tp
	}

	// Track which venue is long vs short based on action.
	if ev.Action == "BUY" {
		tp.pos.LongVenue = ev.Venue
	} else {
		tp.pos.ShortVenue = ev.Venue
	}
	tp.pos.NotionalUSD = ev.FilledNotionalUSD

	// Once we have both venues, promote to a stable key.
	if tp.pos.LongVenue != "" && tp.pos.ShortVenue != "" {
		stableKey := "diff:" + ev.Symbol + ":" + tp.pos.LongVenue + ":" + tp.pos.ShortVenue
		pt.positions[stableKey] = tp
		delete(pt.positions, intentKey)
	}
}

func (pt *PositionTracker) processDiffExit(ev execution.ExecutionEvent) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	// Find and reduce any differential position matching this symbol+venue.
	for key, tp := range pt.positions {
		if tp.pos.Strategy != StrategyFundingDifferential {
			continue
		}
		if tp.pos.Symbol != ev.Symbol {
			continue
		}
		if tp.pos.LongVenue != ev.Venue && tp.pos.ShortVenue != ev.Venue {
			continue
		}
		tp.pos.NotionalUSD -= ev.FilledNotionalUSD
		if tp.pos.NotionalUSD <= 0 {
			delete(pt.positions, key)
		}
		return
	}
}

// OpenPositions returns a snapshot of all currently open funding positions.
func (pt *PositionTracker) OpenPositions() []OpenPosition {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	var result []OpenPosition
	for _, tp := range pt.positions {
		// Skip partially-formed differential positions (missing a venue).
		if tp.pos.Strategy == StrategyFundingDifferential &&
			(tp.pos.LongVenue == "" || tp.pos.ShortVenue == "") {
			continue
		}
		if tp.pos.NotionalUSD > 0 {
			result = append(result, tp.pos)
		}
	}
	return result
}

// UpdateEntryRate sets the funding rate at entry for an open position.
// Called when the funding engine has rate data at the time of position creation.
func (pt *PositionTracker) UpdateEntryRate(strategy, symbol, venue string, rateBps float64) {
	pt.mu.Lock()
	defer pt.mu.Unlock()

	var key string
	switch strategy {
	case StrategyFundingCarry:
		key = "carry:" + symbol + ":" + venue
	default:
		return
	}
	if tp, ok := pt.positions[key]; ok {
		tp.pos.EntryRateBps = rateBps
	}
}
