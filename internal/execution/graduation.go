package execution

import (
	"fmt"
	"sync"
	"time"
)

// GraduationHarness enforces micro-live graduation ramp schedule.
// It tracks per-week caps that progressively increase, and validates
// mode transitions (PAPER→SHADOW→LIVE) against minimum time and performance
// requirements.
type GraduationHarness struct {
	mu      sync.Mutex
	cfg     GraduationConfig
	startTs time.Time
}

// GraduationConfig controls micro-live ramp parameters.
type GraduationConfig struct {
	RampWeeks          int     `yaml:"ramp_weeks"`            // total weeks in ramp (default 4)
	Week1MaxOrderUSD   float64 `yaml:"week1_max_order_usd"`   // week 1 per-order cap
	Week1MaxDailyUSD   float64 `yaml:"week1_max_daily_usd"`   // week 1 daily cap
	WeeklyMultiplier   float64 `yaml:"weekly_multiplier"`     // cap multiplier per week
	MinPaperDays       int     `yaml:"min_paper_days"`        // min days in PAPER before SHADOW
	MinShadowDays      int     `yaml:"min_shadow_days"`       // min days in SHADOW before LIVE
	MinSharpeForLive   float64 `yaml:"min_sharpe_for_live"`   // min Sharpe proxy for LIVE graduation
	MaxDrawdownForLive float64 `yaml:"max_drawdown_for_live"` // max drawdown % allowed for LIVE
}

// DefaultGraduationConfig returns conservative ramp defaults.
func DefaultGraduationConfig() GraduationConfig {
	return GraduationConfig{
		RampWeeks:          4,
		Week1MaxOrderUSD:   5000,
		Week1MaxDailyUSD:   25000,
		WeeklyMultiplier:   2.0,
		MinPaperDays:       7,
		MinShadowDays:      14,
		MinSharpeForLive:   0.5,
		MaxDrawdownForLive: 5.0,
	}
}

// NewGraduationHarness creates a graduation harness with the given config.
func NewGraduationHarness(cfg GraduationConfig) *GraduationHarness {
	return &GraduationHarness{cfg: cfg, startTs: time.Now()}
}

// CurrentLimits returns the effective per-order and daily caps for the current ramp week.
func (g *GraduationHarness) CurrentLimits() (maxOrderUSD, maxDailyUSD float64, week int) {
	g.mu.Lock()
	defer g.mu.Unlock()

	elapsed := time.Since(g.startTs)
	week = int(elapsed.Hours()/168) + 1 // 168 hours = 1 week
	if week > g.cfg.RampWeeks {
		week = g.cfg.RampWeeks
	}

	multiplier := 1.0
	for i := 1; i < week; i++ {
		multiplier *= g.cfg.WeeklyMultiplier
	}

	return g.cfg.Week1MaxOrderUSD * multiplier,
		g.cfg.Week1MaxDailyUSD * multiplier,
		week
}

// CheckGraduationEligible validates whether the system can graduate from one
// mode to another based on time requirements and performance thresholds.
func (g *GraduationHarness) CheckGraduationEligible(
	from, to string,
	paperStartedAt time.Time,
	shadowStartedAt time.Time,
	sharpe float64,
	drawdownPct float64,
) error {
	if to == "SHADOW" && from == "PAPER" {
		days := int(time.Since(paperStartedAt).Hours() / 24)
		if days < g.cfg.MinPaperDays {
			return fmt.Errorf("minimum %d paper days required, only %d elapsed",
				g.cfg.MinPaperDays, days)
		}
	}
	if to == "LIVE" {
		days := int(time.Since(shadowStartedAt).Hours() / 24)
		if days < g.cfg.MinShadowDays {
			return fmt.Errorf("minimum %d shadow days required, only %d elapsed",
				g.cfg.MinShadowDays, days)
		}
		if sharpe < g.cfg.MinSharpeForLive {
			return fmt.Errorf("Sharpe %.2f below minimum %.2f for LIVE",
				sharpe, g.cfg.MinSharpeForLive)
		}
		if drawdownPct > g.cfg.MaxDrawdownForLive {
			return fmt.Errorf("drawdown %.1f%% exceeds maximum %.1f%% for LIVE",
				drawdownPct, g.cfg.MaxDrawdownForLive)
		}
	}
	return nil
}

// RampScheduleSummary returns a human-readable summary of the ramp schedule.
func (g *GraduationHarness) RampScheduleSummary() []RampWeek {
	weeks := make([]RampWeek, g.cfg.RampWeeks)
	multiplier := 1.0
	for i := range weeks {
		weeks[i] = RampWeek{
			Week:        i + 1,
			MaxOrderUSD: g.cfg.Week1MaxOrderUSD * multiplier,
			MaxDailyUSD: g.cfg.Week1MaxDailyUSD * multiplier,
		}
		multiplier *= g.cfg.WeeklyMultiplier
	}
	return weeks
}

// RampWeek describes the caps for a single week in the graduation ramp.
type RampWeek struct {
	Week        int     `json:"week"`
	MaxOrderUSD float64 `json:"max_order_usd"`
	MaxDailyUSD float64 `json:"max_daily_usd"`
}
