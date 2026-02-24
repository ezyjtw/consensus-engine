package execution

import (
	"testing"
	"time"
)

func TestGraduationCurrentLimits(t *testing.T) {
	cfg := DefaultGraduationConfig()
	g := NewGraduationHarness(cfg)

	maxOrder, maxDaily, week := g.CurrentLimits()
	if week != 1 {
		t.Errorf("expected week 1, got %d", week)
	}
	if maxOrder != cfg.Week1MaxOrderUSD {
		t.Errorf("expected order cap $%.0f, got $%.0f", cfg.Week1MaxOrderUSD, maxOrder)
	}
	if maxDaily != cfg.Week1MaxDailyUSD {
		t.Errorf("expected daily cap $%.0f, got $%.0f", cfg.Week1MaxDailyUSD, maxDaily)
	}
}

func TestGraduationRampSchedule(t *testing.T) {
	cfg := DefaultGraduationConfig()
	g := NewGraduationHarness(cfg)

	weeks := g.RampScheduleSummary()
	if len(weeks) != 4 {
		t.Fatalf("expected 4 weeks, got %d", len(weeks))
	}
	// Week 2 should be 2x week 1.
	if weeks[1].MaxOrderUSD != cfg.Week1MaxOrderUSD*2 {
		t.Errorf("week 2 order cap: got $%.0f, want $%.0f",
			weeks[1].MaxOrderUSD, cfg.Week1MaxOrderUSD*2)
	}
	// Week 4 should be 8x week 1 (2^3).
	if weeks[3].MaxOrderUSD != cfg.Week1MaxOrderUSD*8 {
		t.Errorf("week 4 order cap: got $%.0f, want $%.0f",
			weeks[3].MaxOrderUSD, cfg.Week1MaxOrderUSD*8)
	}
}

func TestGraduationEligiblePaperToShadow(t *testing.T) {
	cfg := DefaultGraduationConfig()
	g := NewGraduationHarness(cfg)

	// Too early — only 1 day of paper.
	err := g.CheckGraduationEligible("PAPER", "SHADOW",
		time.Now().Add(-24*time.Hour), time.Time{}, 0, 0)
	if err == nil {
		t.Error("expected error for insufficient paper days")
	}

	// Enough days.
	err = g.CheckGraduationEligible("PAPER", "SHADOW",
		time.Now().Add(-8*24*time.Hour), time.Time{}, 0, 0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGraduationEligibleShadowToLive(t *testing.T) {
	cfg := DefaultGraduationConfig()
	g := NewGraduationHarness(cfg)

	// Too early + bad Sharpe.
	err := g.CheckGraduationEligible("SHADOW", "LIVE",
		time.Time{}, time.Now().Add(-7*24*time.Hour), 0.3, 2.0)
	if err == nil {
		t.Error("expected error for insufficient shadow days")
	}

	// Enough days but bad Sharpe.
	err = g.CheckGraduationEligible("SHADOW", "LIVE",
		time.Time{}, time.Now().Add(-15*24*time.Hour), 0.3, 2.0)
	if err == nil {
		t.Error("expected error for low Sharpe")
	}

	// Enough days, good Sharpe, high drawdown.
	err = g.CheckGraduationEligible("SHADOW", "LIVE",
		time.Time{}, time.Now().Add(-15*24*time.Hour), 1.0, 6.0)
	if err == nil {
		t.Error("expected error for high drawdown")
	}

	// All good.
	err = g.CheckGraduationEligible("SHADOW", "LIVE",
		time.Time{}, time.Now().Add(-15*24*time.Hour), 1.0, 3.0)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
