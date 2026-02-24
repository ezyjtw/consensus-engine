package defirisk

import (
	"testing"
	"time"
)

func TestCalculateIL(t *testing.T) {
	// Price doubles: IL should be about -5.72%
	il := CalculateIL(1.0, 2.0, 10000, 3.0)

	if il.ILPct >= 0 {
		t.Errorf("expected negative IL, got %.4f%%", il.ILPct)
	}
	// IL for 2x price ratio = 2*sqrt(2)/(1+2) - 1 ≈ -0.0572 = -5.72%
	expectedIL := (2*1.4142/(1+2) - 1) * 100
	if diff := il.ILPct - expectedIL; diff > 0.1 || diff < -0.1 {
		t.Errorf("IL %.4f%% differs from expected %.4f%%", il.ILPct, expectedIL)
	}

	// With 3% fees earned, net PnL should be fees + IL
	if il.NetPnLPct != il.FeesEarnedPct+il.ILPct {
		t.Errorf("net PnL %.4f%% != fees %.4f%% + IL %.4f%%",
			il.NetPnLPct, il.FeesEarnedPct, il.ILPct)
	}
}

func TestCalculateILNoChange(t *testing.T) {
	// Same price: IL should be 0
	il := CalculateIL(100.0, 100.0, 10000, 0)
	if il.ILPct != 0 {
		t.Errorf("expected 0 IL for unchanged price, got %.6f%%", il.ILPct)
	}
}

func TestDepegDetector(t *testing.T) {
	d := NewDepegDetector(50, 300000) // 50 bps threshold

	now := time.Now().UnixMilli()
	targets := map[string]float64{
		"USDC": 1.0,
		"USDT": 1.0,
	}

	// Normal prices — no alert
	d.RecordPrice("USDC", 1.0002, now-10000)
	d.RecordPrice("USDC", 0.9998, now)
	d.RecordPrice("USDT", 1.0001, now)

	alerts := d.Check(targets)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts for normal prices, got %d", len(alerts))
	}

	// Depeg USDC
	d.RecordPrice("USDC", 0.993, now+1000) // 70 bps deviation

	alerts = d.Check(targets)
	found := false
	for _, a := range alerts {
		if a.Asset == "USDC" {
			found = true
			if a.DeviationBps < 50 {
				t.Errorf("expected deviation > 50 bps, got %.1f", a.DeviationBps)
			}
		}
	}
	if !found {
		t.Error("expected depeg alert for USDC")
	}
}

func TestOracleChecker(t *testing.T) {
	oc := NewOracleChecker(100, 60) // 100 bps max dev, 60s max stale

	now := time.Now().UnixMilli()

	// Normal feed
	oc.RecordOraclePrice("ETH-USD", "chainlink", 2000, now)

	refs := map[string]float64{"ETH-USD": 2000}
	alerts := oc.Validate(refs)
	if len(alerts) != 0 {
		t.Errorf("expected 0 alerts, got %d", len(alerts))
	}

	// Deviating feed
	oc.RecordOraclePrice("ETH-USD", "chainlink", 2050, now+1000) // 250 bps dev from ref

	alerts = oc.Validate(refs)
	foundDev := false
	for _, a := range alerts {
		if a.Type == "DEVIATION" {
			foundDev = true
		}
	}
	if !foundDev {
		t.Error("expected DEVIATION alert")
	}

	// Flatline detection
	for i := 0; i < 12; i++ {
		oc.RecordOraclePrice("BTC-USD", "chainlink", 50000, now+int64(i)*1000)
	}
	refs["BTC-USD"] = 50000
	alerts = oc.Validate(refs)
	foundFlat := false
	for _, a := range alerts {
		if a.FeedID == "BTC-USD" && a.Type == "FLATLINE" {
			foundFlat = true
		}
	}
	if !foundFlat {
		t.Error("expected FLATLINE alert for BTC-USD")
	}
}

func TestProtocolRiskScorer(t *testing.T) {
	scorer := NewProtocolRiskScorer()

	scorer.SetProfile(ProtocolProfile{
		Protocol:    "aave-v3",
		AuditCount:  4,
		TVLMillions: 5000,
		AgeMonths:   30,
		IncidentCount: 0,
		IsUpgradeable: true,
		HasTimelock: true,
	})
	scorer.SetProfile(ProtocolProfile{
		Protocol:    "sketchy-farm",
		AuditCount:  0,
		TVLMillions: 2,
		AgeMonths:   1,
		IncidentCount: 2,
		IsUpgradeable: true,
		HasTimelock: false,
	})

	aaveScore := scorer.Score("aave-v3")
	sketchyScore := scorer.Score("sketchy-farm")

	if aaveScore >= sketchyScore {
		t.Errorf("aave (%f) should be lower risk than sketchy (%f)", aaveScore, sketchyScore)
	}

	ranked := scorer.RankedProtocols()
	if len(ranked) != 2 {
		t.Fatalf("expected 2 protocols, got %d", len(ranked))
	}
	if ranked[0].Protocol != "aave-v3" {
		t.Errorf("expected aave-v3 first (safest), got %s", ranked[0].Protocol)
	}

	// Unknown protocol = max risk
	if scorer.Score("unknown") != 100 {
		t.Errorf("expected 100 for unknown protocol, got %f", scorer.Score("unknown"))
	}
}
