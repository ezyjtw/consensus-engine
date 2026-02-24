package allocator

import (
	"log"
	"sync"
	"time"
)

// StrategyPerformance tracks recent P&L and execution quality for a strategy.
type StrategyPerformance struct {
	Strategy    string
	RecentPnL   float64 // rolling sum of recent P&L (USD)
	FillCount   int     // total fills
	WinCount    int     // fills with positive P&L
	TotalPnL    float64 // lifetime P&L
	LastFillMs  int64
}

// DynamicAllocator adjusts per-strategy caps based on recent performance.
// Strategies that are winning get larger allocations; losing strategies get reduced.
type DynamicAllocator struct {
	mu          sync.Mutex
	performance map[string]*StrategyPerformance
	baseCaps    map[string]float64 // base caps from policy (never exceeded)
}

// NewDynamicAllocator creates a performance-based dynamic allocator.
// baseCaps are the maximum per-strategy caps from policy.
func NewDynamicAllocator(baseCaps map[string]float64) *DynamicAllocator {
	perf := make(map[string]*StrategyPerformance)
	for strat := range baseCaps {
		perf[strat] = &StrategyPerformance{Strategy: strat}
	}
	return &DynamicAllocator{
		performance: perf,
		baseCaps:    baseCaps,
	}
}

// RecordFill updates performance tracking when a fill completes.
func (da *DynamicAllocator) RecordFill(strategy string, pnlUSD float64) {
	da.mu.Lock()
	defer da.mu.Unlock()

	p, ok := da.performance[strategy]
	if !ok {
		p = &StrategyPerformance{Strategy: strategy}
		da.performance[strategy] = p
	}
	p.FillCount++
	p.TotalPnL += pnlUSD
	p.RecentPnL += pnlUSD
	if pnlUSD > 0 {
		p.WinCount++
	}
	p.LastFillMs = time.Now().UnixMilli()
}

// AdjustedCap returns the dynamic cap for a strategy based on performance.
// Range: [baseCap * 0.25, baseCap * 1.5]
//
// Logic:
//   - Win rate > 60% and positive P&L → scale up (1.0-1.5x)
//   - Win rate 40-60% → use base cap (1.0x)
//   - Win rate < 40% or negative P&L → scale down (0.25-0.75x)
//   - No fills yet → use base cap (1.0x, cold start)
func (da *DynamicAllocator) AdjustedCap(strategy string) float64 {
	da.mu.Lock()
	defer da.mu.Unlock()

	baseCap, ok := da.baseCaps[strategy]
	if !ok {
		return 0
	}

	p, ok := da.performance[strategy]
	if !ok || p.FillCount < 5 {
		return baseCap // cold start: use base cap until we have data
	}

	winRate := float64(p.WinCount) / float64(p.FillCount)

	var multiplier float64
	switch {
	case p.RecentPnL > 0 && winRate > 0.6:
		// Winning strategy: scale up proportionally to win rate
		multiplier = 1.0 + (winRate-0.6)*1.25 // 60% → 1.0x, 80% → 1.25x, 100% → 1.5x
		if multiplier > 1.5 {
			multiplier = 1.5
		}
	case winRate >= 0.4:
		// Neutral performance: hold steady
		multiplier = 1.0
	case winRate >= 0.2:
		// Underperforming: reduce allocation
		multiplier = 0.5 + (winRate-0.2)*1.25 // 20% → 0.5x, 40% → 0.75x
	default:
		// Severely underperforming: minimum allocation
		multiplier = 0.25
	}

	// Negative recent P&L always caps at 1.0x regardless of win rate.
	if p.RecentPnL < 0 && multiplier > 1.0 {
		multiplier = 0.75
	}

	adjusted := baseCap * multiplier
	log.Printf("dynamic-allocator: strategy=%s win_rate=%.1f%% recent_pnl=$%.2f multiplier=%.2f cap=$%.0f",
		strategy, winRate*100, p.RecentPnL, multiplier, adjusted)
	return adjusted
}

// DecayRecentPnL reduces the rolling P&L by a decay factor.
// Call periodically (e.g. every 5 minutes) to prevent stale performance from
// dominating allocation decisions.
func (da *DynamicAllocator) DecayRecentPnL(factor float64) {
	da.mu.Lock()
	defer da.mu.Unlock()
	for _, p := range da.performance {
		p.RecentPnL *= factor
	}
}

// Snapshot returns current performance data for all strategies.
func (da *DynamicAllocator) Snapshot() []StrategyPerformance {
	da.mu.Lock()
	defer da.mu.Unlock()
	result := make([]StrategyPerformance, 0, len(da.performance))
	for _, p := range da.performance {
		result = append(result, *p)
	}
	return result
}
