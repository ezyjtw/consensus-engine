// Package treasuryyield provides a treasury yield router that allocates idle
// capital across lending pools, tokenized T-bills, and staking protocols
// to maximise risk-adjusted yield.
package treasuryyield

import (
	"math"
	"sort"
	"sync"
	"time"
)

// YieldSource describes an available yield opportunity.
type YieldSource struct {
	ID            string  `json:"id"`
	Protocol      string  `json:"protocol"`
	ChainID       uint64  `json:"chain_id"`
	Asset         string  `json:"asset"`
	Type          string  `json:"type"` // LENDING, STAKING, TBILL, LP, VAULT
	APYPct        float64 `json:"apy_pct"`
	TVLMillions   float64 `json:"tvl_millions"`
	RiskScore     float64 `json:"risk_score"` // 0-100
	MinDepositUSD float64 `json:"min_deposit_usd"`
	MaxCapacityUSD float64 `json:"max_capacity_usd"`
	LockDays      int     `json:"lock_days"` // 0 = instant withdrawal
	WithdrawTimeS int     `json:"withdraw_time_s"`
	LastUpdateMs  int64   `json:"last_update_ms"`
}

// Allocation describes a capital allocation to a yield source.
type Allocation struct {
	SourceID    string  `json:"source_id"`
	Protocol    string  `json:"protocol"`
	Asset       string  `json:"asset"`
	AmountUSD   float64 `json:"amount_usd"`
	AllocPct    float64 `json:"alloc_pct"`
	ExpAPYPct   float64 `json:"exp_apy_pct"`
	RiskScore   float64 `json:"risk_score"`
	RiskAdjYield float64 `json:"risk_adj_yield"` // APY / risk
}

// RouterConfig configures the treasury yield router.
type RouterConfig struct {
	MaxRiskScore      float64 `yaml:"max_risk_score"`
	MaxSingleAllocPct float64 `yaml:"max_single_alloc_pct"` // max % in one source
	MaxLockDays       int     `yaml:"max_lock_days"`
	MinAPYPct         float64 `yaml:"min_apy_pct"`
	ReservesPct       float64 `yaml:"reserves_pct"`  // keep liquid for ops
	RebalanceThreshPct float64 `yaml:"rebalance_thresh_pct"`
	PreferredChains   []uint64 `yaml:"preferred_chains"`
}

// Portfolio represents the current yield portfolio.
type Portfolio struct {
	TotalCapitalUSD float64      `json:"total_capital_usd"`
	AllocatedUSD    float64      `json:"allocated_usd"`
	ReservesUSD     float64      `json:"reserves_usd"`
	WeightedAPY     float64      `json:"weighted_apy"`
	WeightedRisk    float64      `json:"weighted_risk"`
	Allocations     []Allocation `json:"allocations"`
	YieldPerDayUSD  float64      `json:"yield_per_day_usd"`
	YieldPerYearUSD float64      `json:"yield_per_year_usd"`
}

// RebalanceAction describes a required portfolio change.
type RebalanceAction struct {
	Type      string  `json:"type"` // DEPOSIT, WITHDRAW, SHIFT
	SourceID  string  `json:"source_id"`
	AmountUSD float64 `json:"amount_usd"`
	Reason    string  `json:"reason"`
}

// Router allocates idle capital across yield sources.
type Router struct {
	mu      sync.RWMutex
	sources map[string]*YieldSource
	config  RouterConfig
	current []Allocation // current live allocations
}

// NewRouter creates a treasury yield router.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.MaxRiskScore == 0 {
		cfg.MaxRiskScore = 40
	}
	if cfg.MaxSingleAllocPct == 0 {
		cfg.MaxSingleAllocPct = 30
	}
	if cfg.MaxLockDays == 0 {
		cfg.MaxLockDays = 7
	}
	if cfg.ReservesPct == 0 {
		cfg.ReservesPct = 20
	}
	return &Router{
		sources: make(map[string]*YieldSource),
		config:  cfg,
	}
}

// UpdateSource registers or updates a yield source.
func (r *Router) UpdateSource(src YieldSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	src.LastUpdateMs = time.Now().UnixMilli()
	r.sources[src.ID] = &src
}

// RemoveSource removes a yield source.
func (r *Router) RemoveSource(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sources, id)
}

// SetCurrentAllocations records the current live allocations.
func (r *Router) SetCurrentAllocations(allocs []Allocation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.current = allocs
}

// Optimize computes the optimal allocation for given capital.
func (r *Router) Optimize(capitalUSD float64) Portfolio {
	r.mu.RLock()
	defer r.mu.RUnlock()

	reservesUSD := capitalUSD * r.config.ReservesPct / 100
	deployable := capitalUSD - reservesUSD

	// Filter eligible sources
	var eligible []*YieldSource
	for _, src := range r.sources {
		if src.RiskScore > r.config.MaxRiskScore {
			continue
		}
		if src.LockDays > r.config.MaxLockDays {
			continue
		}
		if r.config.MinAPYPct > 0 && src.APYPct < r.config.MinAPYPct {
			continue
		}
		eligible = append(eligible, src)
	}

	// Sort by risk-adjusted yield (APY / sqrt(risk))
	sort.Slice(eligible, func(i, j int) bool {
		riskI := math.Max(1, eligible[i].RiskScore)
		riskJ := math.Max(1, eligible[j].RiskScore)
		return eligible[i].APYPct/math.Sqrt(riskI) > eligible[j].APYPct/math.Sqrt(riskJ)
	})

	maxPerSource := deployable * r.config.MaxSingleAllocPct / 100
	var allocations []Allocation
	remaining := deployable

	for _, src := range eligible {
		if remaining <= 0 {
			break
		}

		// Size allocation
		alloc := math.Min(remaining, maxPerSource)
		if src.MaxCapacityUSD > 0 {
			alloc = math.Min(alloc, src.MaxCapacityUSD)
		}
		if src.MinDepositUSD > 0 && alloc < src.MinDepositUSD {
			continue
		}

		riskAdj := src.APYPct / math.Max(1, math.Sqrt(src.RiskScore))

		allocations = append(allocations, Allocation{
			SourceID:     src.ID,
			Protocol:     src.Protocol,
			Asset:        src.Asset,
			AmountUSD:    alloc,
			AllocPct:     alloc / capitalUSD * 100,
			ExpAPYPct:    src.APYPct,
			RiskScore:    src.RiskScore,
			RiskAdjYield: riskAdj,
		})

		remaining -= alloc
	}

	// Compute portfolio metrics
	portfolio := Portfolio{
		TotalCapitalUSD: capitalUSD,
		ReservesUSD:     reservesUSD + remaining,
		Allocations:     allocations,
	}

	for _, a := range allocations {
		portfolio.AllocatedUSD += a.AmountUSD
		portfolio.WeightedAPY += a.ExpAPYPct * a.AmountUSD
		portfolio.WeightedRisk += a.RiskScore * a.AmountUSD
	}

	if portfolio.AllocatedUSD > 0 {
		portfolio.WeightedAPY /= portfolio.AllocatedUSD
		portfolio.WeightedRisk /= portfolio.AllocatedUSD
	}
	portfolio.YieldPerYearUSD = portfolio.AllocatedUSD * portfolio.WeightedAPY / 100
	portfolio.YieldPerDayUSD = portfolio.YieldPerYearUSD / 365

	return portfolio
}

// Rebalance compares current allocations to optimal and returns actions.
func (r *Router) Rebalance(capitalUSD float64) []RebalanceAction {
	r.mu.RLock()
	current := r.current
	r.mu.RUnlock()

	optimal := r.Optimize(capitalUSD)

	// Build current map
	currentMap := make(map[string]float64)
	for _, a := range current {
		currentMap[a.SourceID] = a.AmountUSD
	}

	// Build optimal map
	optimalMap := make(map[string]float64)
	for _, a := range optimal.Allocations {
		optimalMap[a.SourceID] = a.AmountUSD
	}

	threshUSD := capitalUSD * r.config.RebalanceThreshPct / 100
	var actions []RebalanceAction

	// Withdrawals (from sources no longer optimal or over-allocated)
	for id, curAmt := range currentMap {
		optAmt := optimalMap[id]
		diff := curAmt - optAmt
		if diff > threshUSD {
			actions = append(actions, RebalanceAction{
				Type:      "WITHDRAW",
				SourceID:  id,
				AmountUSD: diff,
				Reason:    "over-allocated vs optimal",
			})
		}
	}

	// Deposits (new or under-allocated sources)
	for id, optAmt := range optimalMap {
		curAmt := currentMap[id]
		diff := optAmt - curAmt
		if diff > threshUSD {
			actions = append(actions, RebalanceAction{
				Type:      "DEPOSIT",
				SourceID:  id,
				AmountUSD: diff,
				Reason:    "under-allocated vs optimal",
			})
		}
	}

	return actions
}

// SourceCount returns the number of tracked yield sources.
func (r *Router) SourceCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sources)
}
