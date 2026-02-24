// Package keeper provides a liquidation keeper engine that monitors lending
// protocols for undercollateralised positions and executes profitable liquidations.
package keeper

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Position represents a borrowing position on a lending protocol.
type Position struct {
	Protocol       string  `json:"protocol"`
	ChainID        uint64  `json:"chain_id"`
	Account        string  `json:"account"`
	CollateralAsset string `json:"collateral_asset"`
	CollateralUSD  float64 `json:"collateral_usd"`
	DebtAsset      string  `json:"debt_asset"`
	DebtUSD        float64 `json:"debt_usd"`
	HealthFactor   float64 `json:"health_factor"` // <1 = liquidatable
	LiqThreshold   float64 `json:"liq_threshold"` // e.g. 0.825
	LiqBonus       float64 `json:"liq_bonus"`     // e.g. 0.05 = 5% bonus
	LastUpdateMs   int64   `json:"last_update_ms"`
}

// LiquidationCandidate is a position eligible for liquidation.
type LiquidationCandidate struct {
	Position
	ProfitUSD      float64 `json:"profit_usd"`
	GasCostUSD     float64 `json:"gas_cost_usd"`
	NetProfitUSD   float64 `json:"net_profit_usd"`
	RepayAmountUSD float64 `json:"repay_amount_usd"`
	ReceiveUSD     float64 `json:"receive_usd"`
	Score          float64 `json:"score"` // priority score
}

// KeeperConfig configures the liquidation keeper.
type KeeperConfig struct {
	MinProfitUSD      float64 `yaml:"min_profit_usd"`
	MaxPositionUSD    float64 `yaml:"max_position_usd"`
	GasEstimateGwei   float64 `yaml:"gas_estimate_gwei"`
	GasLimitLiq       uint64  `yaml:"gas_limit_liq"`
	HealthFactorMax   float64 `yaml:"health_factor_max"` // only liquidate below this
	MaxConcurrent     int     `yaml:"max_concurrent"`
	CooldownMs        int64   `yaml:"cooldown_ms"`
	FlashLoanEnabled  bool    `yaml:"flash_loan_enabled"`
}

// Engine monitors positions and identifies liquidation opportunities.
type Engine struct {
	mu         sync.RWMutex
	positions  map[string]*Position // key: protocol:chain:account
	config     KeeperConfig
	cooldowns  map[string]int64 // account → cooldown until
	stats      KeeperStats
}

// KeeperStats tracks keeper performance.
type KeeperStats struct {
	PositionsMonitored int     `json:"positions_monitored"`
	LiquidationsFound  int     `json:"liquidations_found"`
	LiquidationsExec   int     `json:"liquidations_executed"`
	TotalProfitUSD     float64 `json:"total_profit_usd"`
	TotalGasCostUSD    float64 `json:"total_gas_cost_usd"`
	AvgProfitUSD       float64 `json:"avg_profit_usd"`
}

// NewEngine creates a liquidation keeper engine.
func NewEngine(cfg KeeperConfig) *Engine {
	if cfg.HealthFactorMax == 0 {
		cfg.HealthFactorMax = 1.0
	}
	if cfg.MinProfitUSD == 0 {
		cfg.MinProfitUSD = 10
	}
	if cfg.MaxPositionUSD == 0 {
		cfg.MaxPositionUSD = 500000
	}
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 3
	}
	return &Engine{
		positions: make(map[string]*Position),
		config:    cfg,
		cooldowns: make(map[string]int64),
	}
}

// UpdatePosition records or updates a position snapshot.
func (e *Engine) UpdatePosition(p Position) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p.LastUpdateMs = time.Now().UnixMilli()
	key := p.Protocol + ":" + p.Account
	e.positions[key] = &p
}

// RemovePosition removes a position (repaid/closed).
func (e *Engine) RemovePosition(protocol, account string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.positions, protocol+":"+account)
}

// Scan evaluates all positions and returns profitable liquidation candidates.
func (e *Engine) Scan() []LiquidationCandidate {
	e.mu.RLock()
	defer e.mu.RUnlock()

	now := time.Now().UnixMilli()
	var candidates []LiquidationCandidate

	for _, pos := range e.positions {
		// Skip healthy positions
		if pos.HealthFactor >= e.config.HealthFactorMax {
			continue
		}

		// Skip positions on cooldown
		if cd, ok := e.cooldowns[pos.Account]; ok && now < cd {
			continue
		}

		// Skip positions too large
		if pos.DebtUSD > e.config.MaxPositionUSD {
			continue
		}

		candidate := e.evaluateLiquidation(pos)
		if candidate == nil {
			continue
		}

		if candidate.NetProfitUSD >= e.config.MinProfitUSD {
			candidates = append(candidates, *candidate)
		}
	}

	// Sort by net profit descending
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Score > candidates[j].Score
	})

	// Limit to max concurrent
	if len(candidates) > e.config.MaxConcurrent {
		candidates = candidates[:e.config.MaxConcurrent]
	}

	return candidates
}

// RecordExecution records a completed liquidation for stats.
func (e *Engine) RecordExecution(account string, profitUSD, gasCostUSD float64) {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.stats.LiquidationsExec++
	e.stats.TotalProfitUSD += profitUSD
	e.stats.TotalGasCostUSD += gasCostUSD
	if e.stats.LiquidationsExec > 0 {
		e.stats.AvgProfitUSD = e.stats.TotalProfitUSD / float64(e.stats.LiquidationsExec)
	}

	// Set cooldown
	if e.config.CooldownMs > 0 {
		e.cooldowns[account] = time.Now().UnixMilli() + e.config.CooldownMs
	}
}

// Stats returns current keeper statistics.
func (e *Engine) Stats() KeeperStats {
	e.mu.RLock()
	defer e.mu.RUnlock()
	e.stats.PositionsMonitored = len(e.positions)

	count := 0
	for _, pos := range e.positions {
		if pos.HealthFactor < e.config.HealthFactorMax {
			count++
		}
	}
	e.stats.LiquidationsFound = count
	return e.stats
}

func (e *Engine) evaluateLiquidation(pos *Position) *LiquidationCandidate {
	// Standard lending protocol liquidation:
	// Liquidator repays up to 50% of debt, receives collateral + bonus
	maxRepayPct := 0.5
	repayUSD := pos.DebtUSD * maxRepayPct

	// Collateral received = repayUSD * (1 + bonus)
	receiveUSD := repayUSD * (1 + pos.LiqBonus)

	// Can't receive more than available collateral
	if receiveUSD > pos.CollateralUSD {
		receiveUSD = pos.CollateralUSD
		repayUSD = receiveUSD / (1 + pos.LiqBonus)
	}

	grossProfit := receiveUSD - repayUSD

	// Gas cost estimate
	ethPriceUSD := 2000.0 // placeholder
	gasCost := float64(e.config.GasLimitLiq) * e.config.GasEstimateGwei * 1e-9 * ethPriceUSD
	if gasCost == 0 {
		gasCost = 5.0 // default estimate
	}

	netProfit := grossProfit - gasCost

	// Score: profit-weighted, with urgency for low health factors
	urgency := math.Max(0, 1-pos.HealthFactor) * 10
	score := netProfit + urgency

	return &LiquidationCandidate{
		Position:       *pos,
		ProfitUSD:      grossProfit,
		GasCostUSD:     gasCost,
		NetProfitUSD:   netProfit,
		RepayAmountUSD: repayUSD,
		ReceiveUSD:     receiveUSD,
		Score:          score,
	}
}
