package funding

import "time"

// FundingScheduler provides timing intelligence for funding rate collection.
// Major exchanges (Binance, OKX, Bybit) reset funding every 8 hours at
// UTC 00:00, 08:00, 16:00. Deribit resets every 8 hours as well.
//
// Key insight: the best time to enter a funding carry position is just after
// a reset (maximum collection window), and the worst time is just before
// (funding rate may flip, and you only collect a partial period).
type FundingScheduler struct {
	resetHours []int // UTC hours when funding resets
}

// NewFundingScheduler creates a scheduler with the standard 8h reset schedule.
func NewFundingScheduler() *FundingScheduler {
	return &FundingScheduler{resetHours: []int{0, 8, 16}}
}

// NextReset returns the time of the next funding rate reset after now.
func (s *FundingScheduler) NextReset(now time.Time) time.Time {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for _, h := range s.resetHours {
		t := today.Add(time.Duration(h) * time.Hour)
		if t.After(now) {
			return t
		}
	}
	// All today's resets have passed; next is tomorrow's first reset.
	return today.Add(24*time.Hour + time.Duration(s.resetHours[0])*time.Hour)
}

// PrevReset returns the time of the most recent funding reset before now.
func (s *FundingScheduler) PrevReset(now time.Time) time.Time {
	now = now.UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	for i := len(s.resetHours) - 1; i >= 0; i-- {
		t := today.Add(time.Duration(s.resetHours[i]) * time.Hour)
		if !t.After(now) {
			return t
		}
	}
	// Before today's first reset; use yesterday's last.
	yesterday := today.Add(-24 * time.Hour)
	return yesterday.Add(time.Duration(s.resetHours[len(s.resetHours)-1]) * time.Hour)
}

// TimeToNextReset returns how long until the next funding reset.
func (s *FundingScheduler) TimeToNextReset(now time.Time) time.Duration {
	return s.NextReset(now).Sub(now)
}

// TimeSinceLastReset returns how long since the last funding reset.
func (s *FundingScheduler) TimeSinceLastReset(now time.Time) time.Duration {
	return now.Sub(s.PrevReset(now))
}

// IsNearReset returns true if we're within `window` of the next reset.
// Near-reset periods are risky for new entries because the rate may flip.
func (s *FundingScheduler) IsNearReset(now time.Time, window time.Duration) bool {
	return s.TimeToNextReset(now) < window
}

// OptimalEntry returns true if we're in the optimal entry window: the first
// 90 minutes after a funding reset. This gives the longest collection period
// before the next reset (6.5+ hours of accrual remaining).
func (s *FundingScheduler) OptimalEntry(now time.Time) bool {
	since := s.TimeSinceLastReset(now)
	return since < 90*time.Minute
}

// PeriodFraction returns what fraction of the current 8h period has elapsed (0.0–1.0).
// Used to estimate partial-period funding accrual.
func (s *FundingScheduler) PeriodFraction(now time.Time) float64 {
	since := s.TimeSinceLastReset(now).Seconds()
	total := 8 * 3600.0 // 8 hours
	frac := since / total
	if frac > 1 {
		frac = 1
	}
	return frac
}

// RemainingPeriodFraction returns what fraction of the current 8h period remains.
func (s *FundingScheduler) RemainingPeriodFraction(now time.Time) float64 {
	return 1 - s.PeriodFraction(now)
}
