package makerrebate

import (
	"testing"
	"time"
)

func TestSetScheduleAndVolumeReport(t *testing.T) {
	opt := NewOptimizer(30 * 24 * 3600 * 1000) // 30d window

	opt.SetSchedule(VenueFeeSchedule{
		Venue: "binance",
		Tiers: []FeeTier{
			{Tier: "VIP0", RequiredVolume30d: 0, TakerFeeBps: 10, MakerFeeBps: 1},
			{Tier: "VIP1", RequiredVolume30d: 1_000_000, TakerFeeBps: 8, MakerFeeBps: -0.5},
			{Tier: "VIP2", RequiredVolume30d: 5_000_000, TakerFeeBps: 6, MakerFeeBps: -1.0},
		},
	})

	now := time.Now().UnixMilli()
	// Record fills worth $500k total, 60% maker
	for i := 0; i < 100; i++ {
		isMaker := i%5 != 0 && i%5 != 1 // 60% maker
		opt.RecordFill(FillSample{
			Venue:   "binance",
			SizeUSD: 5000,
			IsMaker: isMaker,
			FeeBps:  func() float64 { if isMaker { return 1.0 } else { return 10.0 } }(),
			TsMs:    now - int64(i)*60000,
		})
	}

	reports := opt.VolumeReport()
	if len(reports) != 1 {
		t.Fatalf("expected 1 venue report, got %d", len(reports))
	}

	r := reports[0]
	if r.Venue != "binance" {
		t.Errorf("expected venue binance, got %s", r.Venue)
	}
	if r.Volume30dUSD < 400000 || r.Volume30dUSD > 600000 {
		t.Errorf("expected ~500k volume, got %.0f", r.Volume30dUSD)
	}
	if r.CurrentTier != "VIP0" {
		t.Errorf("expected VIP0 tier, got %s", r.CurrentTier)
	}
	if r.NextTier != "VIP1" {
		t.Errorf("expected next tier VIP1, got %s", r.NextTier)
	}
	if r.VolumeToNext <= 0 {
		t.Error("expected positive volume to next tier")
	}
	if r.MakerPct < 55 || r.MakerPct > 65 {
		t.Errorf("expected ~60%% maker, got %.1f%%", r.MakerPct)
	}
}

func TestOptimizeUpgradeTier(t *testing.T) {
	opt := NewOptimizer(30 * 24 * 3600 * 1000)

	opt.SetSchedule(VenueFeeSchedule{
		Venue: "okx",
		Tiers: []FeeTier{
			{Tier: "Regular", RequiredVolume30d: 0, TakerFeeBps: 10, MakerFeeBps: 2},
			{Tier: "VIP1", RequiredVolume30d: 500_000, TakerFeeBps: 8, MakerFeeBps: -0.5},
		},
	})

	now := time.Now().UnixMilli()
	// Record $400k in volume — close to VIP1 threshold of $500k
	for i := 0; i < 80; i++ {
		opt.RecordFill(FillSample{
			Venue:   "okx",
			SizeUSD: 5000,
			IsMaker: i%2 == 0, // 50% maker
			FeeBps:  6.0,
			TsMs:    now - int64(i)*60000,
		})
	}

	opps := opt.Optimize()
	foundUpgrade := false
	for _, opp := range opps {
		if opp.Type == "UPGRADE_TIER" {
			foundUpgrade = true
			if opp.Venue != "okx" {
				t.Errorf("expected venue okx, got %s", opp.Venue)
			}
			if opp.SavingsPerDayUSD <= 0 {
				t.Error("expected positive savings")
			}
			if opp.Confidence <= 0 || opp.Confidence > 1 {
				t.Errorf("confidence out of range: %f", opp.Confidence)
			}
		}
	}
	if !foundUpgrade {
		t.Error("expected UPGRADE_TIER opportunity")
	}
}

func TestOptimizeShiftMaker(t *testing.T) {
	opt := NewOptimizer(30 * 24 * 3600 * 1000)

	opt.SetSchedule(VenueFeeSchedule{
		Venue: "bybit",
		Tiers: []FeeTier{
			{Tier: "Base", RequiredVolume30d: 0, TakerFeeBps: 10, MakerFeeBps: -1},
		},
	})

	now := time.Now().UnixMilli()
	// 30% maker ratio — should recommend shifting to 70%
	for i := 0; i < 100; i++ {
		isMaker := i%10 < 3 // 30% maker
		fee := 10.0
		if isMaker {
			fee = -1.0
		}
		opt.RecordFill(FillSample{
			Venue:   "bybit",
			SizeUSD: 1000,
			IsMaker: isMaker,
			FeeBps:  fee,
			TsMs:    now - int64(i)*60000,
		})
	}

	opps := opt.Optimize()
	foundShift := false
	for _, opp := range opps {
		if opp.Type == "SHIFT_MAKER" {
			foundShift = true
			if opp.SavingsPerDayUSD <= 0 {
				t.Error("expected positive savings from maker shift")
			}
		}
	}
	if !foundShift {
		t.Error("expected SHIFT_MAKER opportunity")
	}
}

func TestTrimFills(t *testing.T) {
	opt := NewOptimizer(60000) // 1 minute window
	opt.maxFills = 10

	now := time.Now().UnixMilli()
	// Add 15 fills — 5 should be trimmed by maxFills
	for i := 0; i < 15; i++ {
		opt.RecordFill(FillSample{
			Venue:   "test",
			SizeUSD: 100,
			IsMaker: true,
			FeeBps:  1.0,
			TsMs:    now - int64(14-i)*1000, // spread over 14 seconds
		})
	}

	if len(opt.fills["test"]) > 10 {
		t.Errorf("expected max 10 fills after trim, got %d", len(opt.fills["test"]))
	}
}
