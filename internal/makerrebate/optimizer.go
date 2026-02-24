// Package makerrebate provides a maker-rebate and liquidity programme
// optimizer that tracks fee tiers across venues, optimizes for maker
// qualification, and captures rebate alpha.
package makerrebate

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"
)

// FeeTier describes a venue's fee schedule tier.
type FeeTier struct {
	Tier              string  `json:"tier"`
	RequiredVolume30d float64 `json:"required_volume_30d"` // USD
	TakerFeeBps       float64 `json:"taker_fee_bps"`
	MakerFeeBps       float64 `json:"maker_fee_bps"` // negative = rebate
}

// VenueFeeSchedule is the full fee schedule for one venue.
type VenueFeeSchedule struct {
	Venue    string    `json:"venue"`
	Tiers    []FeeTier `json:"tiers"` // sorted by required volume ascending
	UpdateMs int64     `json:"update_ms"`
}

// VenueVolume tracks rolling volume on a venue.
type VenueVolume struct {
	Venue         string  `json:"venue"`
	Volume30dUSD  float64 `json:"volume_30d_usd"`
	MakerPct      float64 `json:"maker_pct"`      // % of volume that's maker
	CurrentTier   string  `json:"current_tier"`
	CurrentMaker  float64 `json:"current_maker_bps"`
	CurrentTaker  float64 `json:"current_taker_bps"`
	NextTier      string  `json:"next_tier"`
	VolumeToNext  float64 `json:"volume_to_next"`  // USD needed for next tier
}

// RebateOpportunity describes an optimization recommendation.
type RebateOpportunity struct {
	Venue           string  `json:"venue"`
	Type            string  `json:"type"` // UPGRADE_TIER, SHIFT_MAKER, PROGRAMME_QUALIFY
	CurrentCostBps  float64 `json:"current_cost_bps"`
	OptimisedCostBps float64 `json:"optimised_cost_bps"`
	SavingsPerDayUSD float64 `json:"savings_per_day_usd"`
	Action          string  `json:"action"`
	Confidence      float64 `json:"confidence"`
}

// FillSample records one execution for volume tracking.
type FillSample struct {
	Venue     string  `json:"venue"`
	SizeUSD   float64 `json:"size_usd"`
	IsMaker   bool    `json:"is_maker"`
	FeeBps    float64 `json:"fee_bps"`
	TsMs      int64   `json:"ts_ms"`
}

// Optimizer tracks fee schedules, rolling volumes, and recommends optimizations.
type Optimizer struct {
	mu         sync.RWMutex
	schedules  map[string]*VenueFeeSchedule
	fills      map[string][]FillSample // venue → recent fills
	windowMs   int64
	maxFills   int
}

// NewOptimizer creates a maker rebate optimizer.
func NewOptimizer(windowMs int64) *Optimizer {
	return &Optimizer{
		schedules: make(map[string]*VenueFeeSchedule),
		fills:     make(map[string][]FillSample),
		windowMs:  windowMs,
		maxFills:  50000,
	}
}

// SetSchedule configures the fee schedule for a venue.
func (o *Optimizer) SetSchedule(schedule VenueFeeSchedule) {
	o.mu.Lock()
	defer o.mu.Unlock()
	sort.Slice(schedule.Tiers, func(i, j int) bool {
		return schedule.Tiers[i].RequiredVolume30d < schedule.Tiers[j].RequiredVolume30d
	})
	o.schedules[schedule.Venue] = &schedule
}

// RecordFill records an execution for volume and fee tracking.
func (o *Optimizer) RecordFill(f FillSample) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.fills[f.Venue] = append(o.fills[f.Venue], f)
	o.trimFills(f.Venue)
}

// VolumeReport returns rolling volume stats per venue.
func (o *Optimizer) VolumeReport() []VenueVolume {
	o.mu.RLock()
	defer o.mu.RUnlock()

	var reports []VenueVolume
	now := time.Now().UnixMilli()
	cutoff30d := now - 30*24*3600*1000

	for venue, sched := range o.schedules {
		fills := o.fills[venue]
		var totalVol, makerVol float64
		for _, f := range fills {
			if f.TsMs >= cutoff30d {
				totalVol += f.SizeUSD
				if f.IsMaker {
					makerVol += f.SizeUSD
				}
			}
		}

		makerPct := 0.0
		if totalVol > 0 {
			makerPct = makerVol / totalVol * 100
		}

		currentTier, nextTier, volumeToNext := o.findTier(sched, totalVol)

		vv := VenueVolume{
			Venue:        venue,
			Volume30dUSD: totalVol,
			MakerPct:     makerPct,
			CurrentTier:  currentTier.Tier,
			CurrentMaker: currentTier.MakerFeeBps,
			CurrentTaker: currentTier.TakerFeeBps,
			VolumeToNext: volumeToNext,
		}
		if nextTier != nil {
			vv.NextTier = nextTier.Tier
		}
		reports = append(reports, vv)
	}

	return reports
}

// Optimize returns rebate optimization recommendations.
func (o *Optimizer) Optimize() []RebateOpportunity {
	o.mu.RLock()
	defer o.mu.RUnlock()

	now := time.Now().UnixMilli()
	var opps []RebateOpportunity

	for venue, sched := range o.schedules {
		fills := o.fills[venue]
		cutoff30d := now - 30*24*3600*1000

		var totalVol, makerVol, totalFees float64
		var fillCount int
		for _, f := range fills {
			if f.TsMs >= cutoff30d {
				totalVol += f.SizeUSD
				totalFees += f.SizeUSD * f.FeeBps / 10000
				if f.IsMaker {
					makerVol += f.SizeUSD
				}
				fillCount++
			}
		}

		if fillCount == 0 {
			continue
		}

		dailyVol := totalVol / 30
		currentTier, nextTier, volumeToNext := o.findTier(sched, totalVol)
		currentAvgCost := totalFees / totalVol * 10000 // bps

		// Opportunity 1: upgrade to next tier
		if nextTier != nil && volumeToNext > 0 {
			daysToQualify := volumeToNext / dailyVol
			if daysToQualify <= 30 { // achievable within a month
				newCostBps := nextTier.TakerFeeBps*0.5 + nextTier.MakerFeeBps*0.5
				savingsPerDay := dailyVol * (currentAvgCost - newCostBps) / 10000

				opps = append(opps, RebateOpportunity{
					Venue:            venue,
					Type:             "UPGRADE_TIER",
					CurrentCostBps:   currentAvgCost,
					OptimisedCostBps: newCostBps,
					SavingsPerDayUSD: savingsPerDay,
					Action:           fmt.Sprintf("Increase volume by $%.0f to reach %s tier", volumeToNext, nextTier.Tier),
					Confidence:       math.Max(0.3, 1-daysToQualify/30),
				})
			}
		}

		// Opportunity 2: shift more volume to maker
		makerPct := 0.0
		if totalVol > 0 {
			makerPct = makerVol / totalVol
		}
		if makerPct < 0.6 { // less than 60% maker
			targetMaker := 0.7
			newCostBps := currentTier.TakerFeeBps*(1-targetMaker) + currentTier.MakerFeeBps*targetMaker
			savingsPerDay := dailyVol * (currentAvgCost - newCostBps) / 10000

			if savingsPerDay > 0 {
				opps = append(opps, RebateOpportunity{
					Venue:            venue,
					Type:             "SHIFT_MAKER",
					CurrentCostBps:   currentAvgCost,
					OptimisedCostBps: newCostBps,
					SavingsPerDayUSD: savingsPerDay,
					Action:           "Increase maker order ratio to 70%+ using passive limit orders",
					Confidence:       0.7,
				})
			}
		}
	}

	sort.Slice(opps, func(i, j int) bool { return opps[i].SavingsPerDayUSD > opps[j].SavingsPerDayUSD })
	return opps
}

func (o *Optimizer) findTier(sched *VenueFeeSchedule, volume float64) (current FeeTier, next *FeeTier, volumeToNext float64) {
	if len(sched.Tiers) == 0 {
		return FeeTier{}, nil, 0
	}

	current = sched.Tiers[0]
	for i, tier := range sched.Tiers {
		if volume >= tier.RequiredVolume30d {
			current = tier
		} else {
			next = &sched.Tiers[i]
			volumeToNext = tier.RequiredVolume30d - volume
			break
		}
	}
	return current, next, volumeToNext
}

func (o *Optimizer) trimFills(venue string) {
	cutoff := time.Now().UnixMilli() - o.windowMs
	fills := o.fills[venue]
	start := 0
	for start < len(fills) && fills[start].TsMs < cutoff {
		start++
	}
	if start > 0 {
		o.fills[venue] = fills[start:]
	}
	if len(o.fills[venue]) > o.maxFills {
		o.fills[venue] = o.fills[venue][len(o.fills[venue])-o.maxFills:]
	}
}
