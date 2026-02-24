package allocator

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// OISnapshot holds the latest open interest for a venue+symbol.
type OISnapshot struct {
	Venue       string  `json:"venue"`
	Symbol      string  `json:"symbol"`
	OiContracts float64 `json:"oi_contracts"`
	TsMs        int64   `json:"ts_ms"`
}

// OITracker consumes open interest data and provides liquidity multipliers
// for position sizing. Higher OI = deeper market = can trade bigger.
type OITracker struct {
	mu   sync.RWMutex
	data map[string]*OISnapshot // "venue:symbol" → latest OI
}

// NewOITracker creates an empty OI tracker.
func NewOITracker() *OITracker {
	return &OITracker{data: make(map[string]*OISnapshot)}
}

// Update ingests an OI update from the market:open_interest stream.
func (t *OITracker) Update(raw string) {
	var snap OISnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return
	}
	if snap.OiContracts <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := snap.Venue + ":" + snap.Symbol
	t.data[key] = &snap
}

// LiquidityMultiplier returns a scaling factor [0.25, 1.5] based on OI depth
// for a given venue+symbol. Returns 1.0 if no OI data is available.
//
// The multiplier is based on the OI relative to a baseline:
//   - OI < 50% of baseline → 0.25 (very thin, reduce size)
//   - OI 50-100% of baseline → linear 0.5-1.0
//   - OI 100-200% of baseline → linear 1.0-1.5 (deep market, can size up)
//   - OI > 200% of baseline → 1.5 cap
func (t *OITracker) LiquidityMultiplier(venue, symbol string, baselineOI float64) float64 {
	if baselineOI <= 0 {
		return 1.0
	}
	t.mu.RLock()
	snap, ok := t.data[venue+":"+symbol]
	t.mu.RUnlock()
	if !ok {
		return 1.0
	}
	// Skip stale data (> 5 minutes old).
	if time.Now().UnixMilli()-snap.TsMs > 300_000 {
		return 1.0
	}
	ratio := snap.OiContracts / baselineOI
	switch {
	case ratio < 0.5:
		return 0.25
	case ratio < 1.0:
		return 0.5 + (ratio-0.5)*1.0 // linear 0.5 → 1.0
	case ratio < 2.0:
		return 1.0 + (ratio-1.0)*0.5 // linear 1.0 → 1.5
	default:
		return 1.5
	}
}

// OIChange returns the percentage change in OI for a venue+symbol compared to
// a provided previous value. Used for cascade detection.
// Returns 0 if no data available.
func (t *OITracker) CurrentOI(venue, symbol string) float64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	snap, ok := t.data[venue+":"+symbol]
	if !ok {
		return 0
	}
	return snap.OiContracts
}

// AllSnapshots returns a copy of all current OI snapshots.
func (t *OITracker) AllSnapshots() []OISnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	result := make([]OISnapshot, 0, len(t.data))
	for _, s := range t.data {
		result = append(result, *s)
	}
	return result
}

// LogSummary logs a brief OI summary.
func (t *OITracker) LogSummary() {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for key, snap := range t.data {
		log.Printf("oi-tracker: %s oi=%.0f contracts", key, snap.OiContracts)
	}
}
