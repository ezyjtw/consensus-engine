package treasuryyield

import (
	"testing"
)

func TestOptimizeAllocations(t *testing.T) {
	r := NewRouter(RouterConfig{
		MaxRiskScore:      50,
		MaxSingleAllocPct: 40,
		MaxLockDays:       30,
		ReservesPct:       20,
		MinAPYPct:         1.0,
	})

	r.UpdateSource(YieldSource{
		ID:       "aave-usdc",
		Protocol: "aave-v3",
		Asset:    "USDC",
		Type:     "LENDING",
		APYPct:   4.5,
		RiskScore: 10,
		TVLMillions: 5000,
	})
	r.UpdateSource(YieldSource{
		ID:       "tbill-ondo",
		Protocol: "ondo",
		Asset:    "OUSG",
		Type:     "TBILL",
		APYPct:   5.2,
		RiskScore: 15,
		TVLMillions: 500,
	})
	r.UpdateSource(YieldSource{
		ID:       "risky-vault",
		Protocol: "sketchy",
		Asset:    "ETH",
		Type:     "VAULT",
		APYPct:   25.0,
		RiskScore: 80, // too risky, should be filtered
	})

	portfolio := r.Optimize(1_000_000)

	// Reserves = 20% set-aside + any unallocated deployable capital.
	// With 2 sources at 40% max each, up to 80% of deployable (800k) = 640k allocated.
	// So reserves = 200k + 160k = 360k.
	if portfolio.ReservesUSD < 300000 {
		t.Errorf("expected significant reserves (unallocated + set-aside), got %.0f", portfolio.ReservesUSD)
	}

	// Should not include risky-vault (risk > 50)
	for _, a := range portfolio.Allocations {
		if a.SourceID == "risky-vault" {
			t.Error("risky-vault should be filtered out")
		}
	}

	// Should have allocated to aave and ondo
	if len(portfolio.Allocations) < 2 {
		t.Errorf("expected at least 2 allocations, got %d", len(portfolio.Allocations))
	}

	// Weighted APY should be reasonable
	if portfolio.WeightedAPY < 3 || portfolio.WeightedAPY > 10 {
		t.Errorf("unexpected weighted APY: %.2f%%", portfolio.WeightedAPY)
	}

	// Yield per year should be positive
	if portfolio.YieldPerYearUSD <= 0 {
		t.Error("expected positive yearly yield")
	}
}

func TestMaxSingleAllocation(t *testing.T) {
	r := NewRouter(RouterConfig{
		MaxRiskScore:      100,
		MaxSingleAllocPct: 25,
		ReservesPct:       0,
	})

	r.UpdateSource(YieldSource{
		ID:       "high-yield",
		Protocol: "compound",
		Asset:    "USDC",
		Type:     "LENDING",
		APYPct:   10.0,
		RiskScore: 10,
	})

	portfolio := r.Optimize(100_000)

	for _, a := range portfolio.Allocations {
		if a.AmountUSD > 25001 { // 25% + rounding
			t.Errorf("allocation %.0f exceeds 25%% max", a.AmountUSD)
		}
	}
}

func TestRebalance(t *testing.T) {
	r := NewRouter(RouterConfig{
		MaxRiskScore:       100,
		MaxSingleAllocPct:  50,
		ReservesPct:        10,
		RebalanceThreshPct: 2,
	})

	r.UpdateSource(YieldSource{
		ID:       "aave-usdc",
		Protocol: "aave-v3",
		Asset:    "USDC",
		Type:     "LENDING",
		APYPct:   5.0,
		RiskScore: 10,
	})
	r.UpdateSource(YieldSource{
		ID:       "comp-usdc",
		Protocol: "compound",
		Asset:    "USDC",
		Type:     "LENDING",
		APYPct:   4.0,
		RiskScore: 12,
	})

	// Current: all in comp, none in aave
	r.SetCurrentAllocations([]Allocation{
		{SourceID: "comp-usdc", AmountUSD: 90000},
	})

	actions := r.Rebalance(100_000)

	// Should suggest withdrawing from comp and depositing to aave
	hasWithdraw := false
	hasDeposit := false
	for _, a := range actions {
		if a.Type == "WITHDRAW" {
			hasWithdraw = true
		}
		if a.Type == "DEPOSIT" {
			hasDeposit = true
		}
	}

	if !hasWithdraw && !hasDeposit {
		t.Error("expected at least one rebalance action")
	}
}

func TestLockDayFilter(t *testing.T) {
	r := NewRouter(RouterConfig{
		MaxRiskScore:      100,
		MaxSingleAllocPct: 50,
		MaxLockDays:       7,
		ReservesPct:       0,
	})

	r.UpdateSource(YieldSource{
		ID:       "locked",
		Protocol: "lido",
		Asset:    "stETH",
		Type:     "STAKING",
		APYPct:   5.0,
		RiskScore: 20,
		LockDays: 30, // exceeds max
	})
	r.UpdateSource(YieldSource{
		ID:       "liquid",
		Protocol: "aave",
		Asset:    "USDC",
		Type:     "LENDING",
		APYPct:   3.0,
		RiskScore: 10,
		LockDays: 0,
	})

	portfolio := r.Optimize(100_000)

	for _, a := range portfolio.Allocations {
		if a.SourceID == "locked" {
			t.Error("locked source should be filtered by MaxLockDays")
		}
	}
	if len(portfolio.Allocations) == 0 {
		t.Error("expected at least liquid source")
	}
}
