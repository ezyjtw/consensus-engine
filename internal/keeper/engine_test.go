package keeper

import (
	"testing"
)

func TestScanLiquidatable(t *testing.T) {
	e := NewEngine(KeeperConfig{
		MinProfitUSD:    5,
		MaxPositionUSD:  100000,
		GasEstimateGwei: 30,
		GasLimitLiq:     400000,
		HealthFactorMax: 1.0,
		MaxConcurrent:   5,
	})

	// Healthy position — should not appear
	e.UpdatePosition(Position{
		Protocol:        "aave-v3",
		Account:         "0xHealthy",
		CollateralAsset: "ETH",
		CollateralUSD:   10000,
		DebtAsset:       "USDC",
		DebtUSD:         5000,
		HealthFactor:    1.5,
		LiqThreshold:    0.825,
		LiqBonus:        0.05,
	})

	// Liquidatable position
	e.UpdatePosition(Position{
		Protocol:        "aave-v3",
		Account:         "0xUnder",
		CollateralAsset: "ETH",
		CollateralUSD:   5000,
		DebtAsset:       "USDC",
		DebtUSD:         5200,
		HealthFactor:    0.85,
		LiqThreshold:    0.825,
		LiqBonus:        0.05,
	})

	candidates := e.Scan()
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(candidates))
	}

	c := candidates[0]
	if c.Account != "0xUnder" {
		t.Errorf("expected 0xUnder, got %s", c.Account)
	}
	if c.RepayAmountUSD <= 0 {
		t.Error("expected positive repay amount")
	}
	if c.ReceiveUSD <= c.RepayAmountUSD {
		t.Error("expected receive > repay (bonus)")
	}
}

func TestScanRespectsCooldown(t *testing.T) {
	e := NewEngine(KeeperConfig{
		MinProfitUSD:    1,
		MaxPositionUSD:  100000,
		HealthFactorMax: 1.0,
		CooldownMs:      60000,
	})

	e.UpdatePosition(Position{
		Protocol:        "compound",
		Account:         "0xTarget",
		CollateralUSD:   10000,
		DebtUSD:         9000,
		HealthFactor:    0.8,
		LiqBonus:        0.08,
	})

	// Should find it initially
	candidates := e.Scan()
	if len(candidates) == 0 {
		t.Fatal("expected candidate before cooldown")
	}

	// Record execution (sets cooldown)
	e.RecordExecution("0xTarget", 100, 5)

	// Should not find it during cooldown
	candidates = e.Scan()
	if len(candidates) != 0 {
		t.Errorf("expected 0 candidates during cooldown, got %d", len(candidates))
	}
}

func TestScanMaxConcurrent(t *testing.T) {
	e := NewEngine(KeeperConfig{
		MinProfitUSD:    1,
		MaxPositionUSD:  100000,
		HealthFactorMax: 1.0,
		MaxConcurrent:   2,
	})

	for i := 0; i < 5; i++ {
		e.UpdatePosition(Position{
			Protocol:      "aave-v3",
			Account:       "0x" + string(rune('A'+i)),
			CollateralUSD: 10000,
			DebtUSD:       9000,
			HealthFactor:  0.9,
			LiqBonus:      0.05,
		})
	}

	candidates := e.Scan()
	if len(candidates) > 2 {
		t.Errorf("expected max 2 candidates, got %d", len(candidates))
	}
}

func TestStats(t *testing.T) {
	e := NewEngine(KeeperConfig{
		MinProfitUSD:    1,
		MaxPositionUSD:  100000,
		HealthFactorMax: 1.0,
	})

	e.UpdatePosition(Position{
		Protocol:      "aave",
		Account:       "0xA",
		CollateralUSD: 1000,
		DebtUSD:       800,
		HealthFactor:  0.9,
		LiqBonus:      0.05,
	})

	e.RecordExecution("0xA", 50, 3)
	e.RecordExecution("0xB", 100, 5)

	stats := e.Stats()
	if stats.LiquidationsExec != 2 {
		t.Errorf("expected 2 executions, got %d", stats.LiquidationsExec)
	}
	if stats.TotalProfitUSD != 150 {
		t.Errorf("expected 150 total profit, got %f", stats.TotalProfitUSD)
	}
	if stats.PositionsMonitored != 1 {
		t.Errorf("expected 1 position monitored, got %d", stats.PositionsMonitored)
	}
}
