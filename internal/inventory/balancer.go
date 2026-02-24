// Package inventory provides cross-exchange inventory balancing and margin
// efficiency optimisation to minimise idle capital and maximise utilisation.
package inventory

import (
	"math"
	"sort"
	"sync"
	"time"
)

// VenueBalance represents the current balance state at one exchange.
type VenueBalance struct {
	Venue          string  `json:"venue"`
	EquityUSD      float64 `json:"equity_usd"`
	FreeMarginUSD  float64 `json:"free_margin_usd"`
	UsedMarginUSD  float64 `json:"used_margin_usd"`
	UnrealizedPnL  float64 `json:"unrealized_pnl"`
	UtilizationPct float64 `json:"utilization_pct"` // used / total margin
	TsMs           int64   `json:"ts_ms"`
}

// RebalanceAction is a recommended capital movement.
type RebalanceAction struct {
	FromVenue   string  `json:"from_venue"`
	ToVenue     string  `json:"to_venue"`
	AmountUSD   float64 `json:"amount_usd"`
	Reason      string  `json:"reason"`
	Priority    int     `json:"priority"` // 1 = urgent, 3 = routine
	EstTimeSec  int     `json:"est_time_sec"`
}

// InventorySnapshot is the full cross-exchange state.
type InventorySnapshot struct {
	Balances       []VenueBalance    `json:"balances"`
	TotalEquityUSD float64           `json:"total_equity_usd"`
	TargetAlloc    map[string]float64 `json:"target_alloc"`    // venue → target %
	ActualAlloc    map[string]float64 `json:"actual_alloc"`    // venue → actual %
	Actions        []RebalanceAction  `json:"actions"`
	TsMs           int64              `json:"ts_ms"`
}

// BalancerConfig controls the inventory balancer.
type BalancerConfig struct {
	TargetAllocPct    map[string]float64 `yaml:"target_alloc_pct"`    // venue → target % (sum=100)
	RebalanceThreshPct float64           `yaml:"rebalance_thresh_pct"` // trigger rebalance if off by this %
	MinTransferUSD    float64            `yaml:"min_transfer_usd"`     // minimum transfer amount
	MaxTransferUSD    float64            `yaml:"max_transfer_usd"`     // maximum single transfer
	MaxUtilizationPct float64            `yaml:"max_utilization_pct"`  // above this = urgent top-up
	MinFreePct        float64            `yaml:"min_free_pct"`         // minimum free margin %
	TransferTimeSec   map[string]int     `yaml:"transfer_time_sec"`    // venue pair → est time
}

// DefaultBalancerConfig returns sensible defaults.
func DefaultBalancerConfig() BalancerConfig {
	return BalancerConfig{
		TargetAllocPct: map[string]float64{
			"binance": 40,
			"okx":     25,
			"bybit":   20,
			"deribit": 15,
		},
		RebalanceThreshPct: 10,
		MinTransferUSD:     100,
		MaxTransferUSD:     50000,
		MaxUtilizationPct:  85,
		MinFreePct:         20,
		TransferTimeSec: map[string]int{
			"binance-okx":     600,
			"binance-bybit":   600,
			"binance-deribit":  900,
			"okx-bybit":       600,
		},
	}
}

// Balancer manages cross-exchange inventory.
type Balancer struct {
	mu  sync.RWMutex
	cfg BalancerConfig
}

// NewBalancer creates an inventory balancer.
func NewBalancer(cfg BalancerConfig) *Balancer {
	return &Balancer{cfg: cfg}
}

// Analyse produces an inventory snapshot with rebalancing recommendations.
func (b *Balancer) Analyse(balances []VenueBalance) *InventorySnapshot {
	b.mu.RLock()
	cfg := b.cfg
	b.mu.RUnlock()

	now := time.Now().UnixMilli()

	var totalEquity float64
	for _, bal := range balances {
		totalEquity += bal.EquityUSD
	}

	if totalEquity == 0 {
		return &InventorySnapshot{Balances: balances, TsMs: now}
	}

	actualAlloc := make(map[string]float64, len(balances))
	for _, bal := range balances {
		actualAlloc[bal.Venue] = bal.EquityUSD / totalEquity * 100
	}

	var actions []RebalanceAction

	// Urgency: check for venues near margin limits
	for _, bal := range balances {
		if bal.UtilizationPct > cfg.MaxUtilizationPct {
			// Find venue with most free margin to donate
			best := b.findDonorVenue(balances, bal.Venue)
			if best != "" {
				amount := math.Min(cfg.MaxTransferUSD, bal.UsedMarginUSD*0.2) // top up 20%
				amount = math.Max(cfg.MinTransferUSD, amount)
				actions = append(actions, RebalanceAction{
					FromVenue:  best,
					ToVenue:    bal.Venue,
					AmountUSD:  amount,
					Reason:     "MARGIN_CRITICAL",
					Priority:   1,
					EstTimeSec: b.transferTime(best, bal.Venue),
				})
			}
		}
	}

	// Routine: rebalance toward target allocation
	type deviation struct {
		venue string
		diff  float64 // actual - target (positive = over-allocated)
	}
	var devs []deviation
	for venue, target := range cfg.TargetAllocPct {
		actual := actualAlloc[venue]
		devs = append(devs, deviation{venue: venue, diff: actual - target})
	}

	sort.Slice(devs, func(i, j int) bool { return devs[i].diff > devs[j].diff })

	// Pair over-allocated venues with under-allocated ones
	i, j := 0, len(devs)-1
	for i < j {
		if devs[i].diff < cfg.RebalanceThreshPct {
			break // no more over-allocated
		}
		if devs[j].diff > -cfg.RebalanceThreshPct {
			break // no more under-allocated
		}

		transferPct := math.Min(devs[i].diff, -devs[j].diff) / 2
		amount := transferPct / 100 * totalEquity
		amount = math.Min(cfg.MaxTransferUSD, amount)
		amount = math.Max(cfg.MinTransferUSD, amount)

		actions = append(actions, RebalanceAction{
			FromVenue:  devs[i].venue,
			ToVenue:    devs[j].venue,
			AmountUSD:  amount,
			Reason:     "REBALANCE",
			Priority:   3,
			EstTimeSec: b.transferTime(devs[i].venue, devs[j].venue),
		})

		devs[i].diff -= transferPct
		devs[j].diff += transferPct
		if devs[i].diff < cfg.RebalanceThreshPct {
			i++
		}
		if devs[j].diff > -cfg.RebalanceThreshPct {
			j--
		}
	}

	return &InventorySnapshot{
		Balances:       balances,
		TotalEquityUSD: totalEquity,
		TargetAlloc:    cfg.TargetAllocPct,
		ActualAlloc:    actualAlloc,
		Actions:        actions,
		TsMs:           now,
	}
}

func (b *Balancer) findDonorVenue(balances []VenueBalance, exclude string) string {
	var best string
	var bestFree float64
	for _, bal := range balances {
		if bal.Venue == exclude {
			continue
		}
		if bal.FreeMarginUSD > bestFree && bal.UtilizationPct < b.cfg.MaxUtilizationPct {
			bestFree = bal.FreeMarginUSD
			best = bal.Venue
		}
	}
	return best
}

func (b *Balancer) transferTime(from, to string) int {
	key1 := from + "-" + to
	key2 := to + "-" + from
	if t, ok := b.cfg.TransferTimeSec[key1]; ok {
		return t
	}
	if t, ok := b.cfg.TransferTimeSec[key2]; ok {
		return t
	}
	return 900 // default 15 minutes
}

// MarginEfficiency computes margin utilisation statistics.
func (b *Balancer) MarginEfficiency(balances []VenueBalance) MarginReport {
	var totalEquity, totalUsed, totalFree, totalPnL float64

	for _, bal := range balances {
		totalEquity += bal.EquityUSD
		totalUsed += bal.UsedMarginUSD
		totalFree += bal.FreeMarginUSD
		totalPnL += bal.UnrealizedPnL
	}

	utilizationPct := 0.0
	if totalEquity > 0 {
		utilizationPct = totalUsed / totalEquity * 100
	}

	idleCapitalUSD := 0.0
	for _, bal := range balances {
		target := b.cfg.TargetAllocPct[bal.Venue]
		if target == 0 {
			continue
		}
		idealFree := bal.EquityUSD * b.cfg.MinFreePct / 100
		excess := bal.FreeMarginUSD - idealFree
		if excess > 0 {
			idleCapitalUSD += excess
		}
	}

	return MarginReport{
		TotalEquityUSD:   totalEquity,
		UsedMarginUSD:    totalUsed,
		FreeMarginUSD:    totalFree,
		UtilizationPct:   utilizationPct,
		IdleCapitalUSD:   idleCapitalUSD,
		UnrealizedPnLUSD: totalPnL,
		VenueCount:       len(balances),
	}
}

// MarginReport summarises cross-exchange margin efficiency.
type MarginReport struct {
	TotalEquityUSD   float64 `json:"total_equity_usd"`
	UsedMarginUSD    float64 `json:"used_margin_usd"`
	FreeMarginUSD    float64 `json:"free_margin_usd"`
	UtilizationPct   float64 `json:"utilization_pct"`
	IdleCapitalUSD   float64 `json:"idle_capital_usd"`
	UnrealizedPnLUSD float64 `json:"unrealized_pnl_usd"`
	VenueCount       int     `json:"venue_count"`
}
