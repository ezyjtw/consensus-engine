package funding

import (
	"math"
	"sync"
)

// VolTracker tracks rolling realized volatility from mark price observations.
// It computes annualized volatility from log-returns over a configurable window.
type VolTracker struct {
	mu       sync.Mutex
	windows  map[string]*volWindow // key: "venue:symbol"
	capacity int                   // max observations per window
}

type priceObs struct {
	price float64
	tsMs  int64
}

type volWindow struct {
	obs  []priceObs
	idx  int
	full bool
	cap  int
}

// NewVolTracker creates a volatility tracker.
// capacity controls how many price observations to retain (e.g. 1000).
func NewVolTracker(capacity int) *VolTracker {
	if capacity < 10 {
		capacity = 500
	}
	return &VolTracker{
		windows:  make(map[string]*volWindow),
		capacity: capacity,
	}
}

// Record adds a mark price observation for a venue+symbol pair.
func (vt *VolTracker) Record(venue, symbol string, price float64, tsMs int64) {
	if price <= 0 {
		return
	}
	vt.mu.Lock()
	defer vt.mu.Unlock()

	key := venue + ":" + symbol
	w, ok := vt.windows[key]
	if !ok {
		w = &volWindow{
			obs: make([]priceObs, vt.capacity),
			cap: vt.capacity,
		}
		vt.windows[key] = w
	}
	w.obs[w.idx] = priceObs{price: price, tsMs: tsMs}
	w.idx++
	if w.idx >= w.cap {
		w.idx = 0
		w.full = true
	}
}

// RealizedVol returns the annualized realized volatility (as a percentage, e.g. 45.0 for 45%)
// for the given venue+symbol. Returns 0 if insufficient data.
// It computes stdev of log-returns, then annualizes assuming 24h/365d continuous trading.
func (vt *VolTracker) RealizedVol(venue, symbol string) float64 {
	vt.mu.Lock()
	defer vt.mu.Unlock()

	key := venue + ":" + symbol
	w, ok := vt.windows[key]
	if !ok {
		return 0
	}

	n := w.count()
	if n < 10 {
		return 0
	}

	// Collect observations in chronological order.
	sorted := w.sorted()

	// Compute log-returns.
	var sumR, sumR2 float64
	returns := 0
	for i := 1; i < len(sorted); i++ {
		if sorted[i].price <= 0 || sorted[i-1].price <= 0 {
			continue
		}
		r := math.Log(sorted[i].price / sorted[i-1].price)
		sumR += r
		sumR2 += r * r
		returns++
	}
	if returns < 5 {
		return 0
	}

	mean := sumR / float64(returns)
	variance := sumR2/float64(returns) - mean*mean
	if variance <= 0 {
		return 0
	}
	stdDev := math.Sqrt(variance)

	// Annualize: estimate the average interval between observations.
	totalTimeMs := float64(sorted[len(sorted)-1].tsMs - sorted[0].tsMs)
	if totalTimeMs <= 0 {
		return 0
	}
	avgIntervalS := totalTimeMs / float64(returns) / 1000
	if avgIntervalS <= 0 {
		return 0
	}
	periodsPerYear := 365.25 * 24 * 3600 / avgIntervalS
	annualizedVol := stdDev * math.Sqrt(periodsPerYear) * 100

	return annualizedVol
}

func (w *volWindow) count() int {
	if w.full {
		return w.cap
	}
	return w.idx
}

// sorted returns observations in chronological order.
func (w *volWindow) sorted() []priceObs {
	n := w.count()
	result := make([]priceObs, 0, n)
	if w.full {
		result = append(result, w.obs[w.idx:]...)
		result = append(result, w.obs[:w.idx]...)
	} else {
		result = append(result, w.obs[:w.idx]...)
	}
	return result
}
