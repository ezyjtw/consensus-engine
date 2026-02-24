package treasury

import (
	"testing"
	"time"
)

func TestLastSeen(t *testing.T) {
	ls := NewLastSeen()

	if ls.Seen("dep-1") {
		t.Fatal("expected dep-1 to be unseen")
	}

	ls.Mark("dep-1")
	if !ls.Seen("dep-1") {
		t.Fatal("expected dep-1 to be seen after marking")
	}

	// Mark another and verify both are tracked.
	ls.Mark("dep-2")
	if !ls.Seen("dep-1") || !ls.Seen("dep-2") {
		t.Fatal("expected both deposits to be seen")
	}

	if ls.Seen("dep-3") {
		t.Fatal("expected dep-3 to be unseen")
	}
}

func TestLastSeenPrune(t *testing.T) {
	ls := NewLastSeen()
	ls.DepositIDs["old"] = time.Now().Add(-25 * time.Hour)
	ls.DepositIDs["recent"] = time.Now()

	ls.Prune(24 * time.Hour)

	if ls.Seen("old") {
		t.Fatal("expected old deposit to be pruned")
	}
	if !ls.Seen("recent") {
		t.Fatal("expected recent deposit to survive prune")
	}
}

func TestAllocationWeightsSum(t *testing.T) {
	// Verify a well-configured allocation sums to ~1.0.
	weights := []AllocationWeight{
		{Venue: "binance", Weight: 0.35},
		{Venue: "okx", Weight: 0.30},
		{Venue: "bybit", Weight: 0.25},
		{Venue: "deribit", Weight: 0.10},
	}

	var sum float64
	for _, w := range weights {
		if w.Weight < 0 || w.Weight > 1 {
			t.Errorf("invalid weight for %s: %f", w.Venue, w.Weight)
		}
		sum += w.Weight
	}

	if sum < 0.999 || sum > 1.001 {
		t.Errorf("weights should sum to 1.0, got %f", sum)
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}

	// Simulate what LoadConfig applies.
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	if cfg.PollIntervalSec == 0 {
		cfg.PollIntervalSec = 30
	}
	if cfg.ConvertTo == "" {
		cfg.ConvertTo = "USDC"
	}
	if cfg.SweepIntervalMin == 0 {
		cfg.SweepIntervalMin = 60
	}
	if cfg.ReconcileIntervalMin == 0 {
		cfg.ReconcileIntervalMin = 15
	}
	if cfg.DriftAlertPct == 0 {
		cfg.DriftAlertPct = 2.0
	}

	if cfg.TenantID != "default" {
		t.Errorf("expected default tenant, got %s", cfg.TenantID)
	}
	if cfg.PollIntervalSec != 30 {
		t.Errorf("expected 30s poll interval, got %d", cfg.PollIntervalSec)
	}
	if cfg.ConvertTo != "USDC" {
		t.Errorf("expected USDC, got %s", cfg.ConvertTo)
	}
	if cfg.DriftAlertPct != 2.0 {
		t.Errorf("expected 2.0%% drift alert, got %f", cfg.DriftAlertPct)
	}
}
