package taxuk

import (
	"sort"
	"time"
)

// matchResult tracks a portion of a disposal matched to an acquisition source.
type matchResult struct {
	quantity  float64
	costGBP   float64
	matchType string // "same-day", "30-day", "section-104"
	matchID   string // acquisition trade ID (empty for pool matches)
}

// MatchSpotTrades applies HMRC's three-tier matching rules to a set of spot
// trades for a single symbol and returns the resulting disposals.
//
// The pool parameter carries forward any Section 104 pool state from prior
// periods; it is updated in place so callers can persist it across tax years.
//
// Matching order per HMRC TCGA 1992 rules:
//  1. Same-day rule — match disposals with acquisitions on the same day.
//  2. 30-day bed-and-breakfasting rule — match with acquisitions in the
//     following 30 calendar days (FIFO within the window).
//  3. Section 104 pool — draw from the weighted-average cost pool.
func MatchSpotTrades(trades []TradeRecord, pool *Section104Pool) []Disposal {
	if pool == nil {
		pool = &Section104Pool{}
	}
	if len(trades) == 0 {
		return nil
	}

	// Sort chronologically.
	sorted := make([]TradeRecord, len(trades))
	copy(sorted, trades)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	n := len(sorted)
	remaining := make([]float64, n)
	for i := range sorted {
		remaining[i] = sorted[i].Quantity
	}

	// Index disposals for efficient lookup in phase 3.
	type disposalWork struct {
		idx     int
		matches []matchResult
	}
	var disposals []disposalWork
	disposalByIdx := make(map[int]int) // sorted-index → disposals-slice-index
	for i, t := range sorted {
		if t.Action == "SELL" {
			disposalByIdx[i] = len(disposals)
			disposals = append(disposals, disposalWork{idx: i})
		}
	}

	// ── Phase 1: same-day matching ──────────────────────────────────────
	for di := range disposals {
		dIdx := disposals[di].idx
		if remaining[dIdx] <= 0 {
			continue
		}
		dispDate := truncateDay(sorted[dIdx].Timestamp)
		for j := range sorted {
			if remaining[dIdx] <= 0 {
				break
			}
			if sorted[j].Action != "BUY" || remaining[j] <= 0 {
				continue
			}
			if !truncateDay(sorted[j].Timestamp).Equal(dispDate) {
				continue
			}
			matched := minf(remaining[dIdx], remaining[j])
			costPerUnit := acqCostPerUnit(sorted[j])
			disposals[di].matches = append(disposals[di].matches, matchResult{
				quantity:  matched,
				costGBP:   matched * costPerUnit,
				matchType: "same-day",
				matchID:   sorted[j].ID,
			})
			remaining[dIdx] -= matched
			remaining[j] -= matched
		}
	}

	// ── Phase 2: 30-day bed-and-breakfasting ────────────────────────────
	for di := range disposals {
		dIdx := disposals[di].idx
		if remaining[dIdx] <= 0 {
			continue
		}
		dispDate := truncateDay(sorted[dIdx].Timestamp)
		deadline := dispDate.AddDate(0, 0, 30)
		for j := range sorted {
			if remaining[dIdx] <= 0 {
				break
			}
			if sorted[j].Action != "BUY" || remaining[j] <= 0 {
				continue
			}
			acqDate := truncateDay(sorted[j].Timestamp)
			if !acqDate.After(dispDate) || acqDate.After(deadline) {
				continue
			}
			matched := minf(remaining[dIdx], remaining[j])
			costPerUnit := acqCostPerUnit(sorted[j])
			disposals[di].matches = append(disposals[di].matches, matchResult{
				quantity:  matched,
				costGBP:   matched * costPerUnit,
				matchType: "30-day",
				matchID:   sorted[j].ID,
			})
			remaining[dIdx] -= matched
			remaining[j] -= matched
		}
	}

	// ── Phase 3: Section 104 pool ───────────────────────────────────────
	// Process chronologically: add remaining acquisitions to the pool as we
	// encounter them, then draw from the pool for remaining disposals.
	for i := range sorted {
		if remaining[i] <= 0 {
			continue
		}
		if sorted[i].Action == "BUY" {
			costPerUnit := acqCostPerUnit(sorted[i])
			pool.TotalQuantity += remaining[i]
			pool.TotalCostGBP += remaining[i] * costPerUnit
			remaining[i] = 0
		} else if sorted[i].Action == "SELL" {
			di, ok := disposalByIdx[i]
			if !ok || pool.TotalQuantity <= 0 {
				continue
			}
			matched := minf(remaining[i], pool.TotalQuantity)
			cpu := pool.CostPerUnit()
			disposals[di].matches = append(disposals[di].matches, matchResult{
				quantity:  matched,
				costGBP:   matched * cpu,
				matchType: "section-104",
			})
			pool.TotalQuantity -= matched
			pool.TotalCostGBP -= matched * cpu
			remaining[i] -= matched
		}
	}

	// ── Build disposal results ──────────────────────────────────────────
	results := make([]Disposal, 0, len(disposals))
	for _, dw := range disposals {
		d := sorted[dw.idx]
		var totalCost, totalQty float64
		var matchedIDs []string
		primaryMatch := "unmatched"

		for _, mr := range dw.matches {
			totalCost += mr.costGBP
			totalQty += mr.quantity
			if mr.matchID != "" {
				matchedIDs = append(matchedIDs, mr.matchID)
			}
			if primaryMatch == "unmatched" {
				primaryMatch = mr.matchType
			}
		}

		// Proceeds proportional to matched quantity.
		proceeds := 0.0
		if d.Quantity > 0 && totalQty > 0 {
			proceeds = (totalQty / d.Quantity) * (d.NotionalGBP - d.FeesGBP)
		}
		gain := proceeds - totalCost

		results = append(results, Disposal{
			Date:        d.Timestamp,
			Symbol:      d.Symbol,
			Quantity:    totalQty,
			ProceedsGBP: proceeds,
			CostGBP:     totalCost,
			GainGBP:     gain,
			MatchType:   primaryMatch,
			MatchedWith: matchedIDs,
		})
	}

	return results
}

// acqCostPerUnit returns the total acquisition cost per unit in GBP,
// including fees (which are an allowable deduction under HMRC rules).
func acqCostPerUnit(t TradeRecord) float64 {
	if t.Quantity <= 0 {
		return 0
	}
	return (t.NotionalGBP + t.FeesGBP) / t.Quantity
}

func truncateDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
