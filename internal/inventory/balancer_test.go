package inventory

import (
	"testing"
	"time"
)

func TestBalancerRebalance(t *testing.T) {
	cfg := DefaultBalancerConfig()
	b := NewBalancer(cfg)
	now := time.Now().UnixMilli()

	// Binance over-allocated (60% vs 40% target), OKX under-allocated (10% vs 25%)
	balances := []VenueBalance{
		{Venue: "binance", EquityUSD: 12000, FreeMarginUSD: 8000, UsedMarginUSD: 4000, UtilizationPct: 33, TsMs: now},
		{Venue: "okx", EquityUSD: 2000, FreeMarginUSD: 1500, UsedMarginUSD: 500, UtilizationPct: 25, TsMs: now},
		{Venue: "bybit", EquityUSD: 4000, FreeMarginUSD: 3000, UsedMarginUSD: 1000, UtilizationPct: 25, TsMs: now},
		{Venue: "deribit", EquityUSD: 2000, FreeMarginUSD: 1500, UsedMarginUSD: 500, UtilizationPct: 25, TsMs: now},
	}

	snap := b.Analyse(balances)
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if snap.TotalEquityUSD != 20000 {
		t.Errorf("expected $20k total, got %.2f", snap.TotalEquityUSD)
	}

	// Should have rebalance actions
	if len(snap.Actions) == 0 {
		t.Error("expected rebalance actions (binance is 60% vs 40% target)")
	}

	// At least one action should move from binance
	found := false
	for _, a := range snap.Actions {
		if a.FromVenue == "binance" {
			found = true
			if a.AmountUSD < cfg.MinTransferUSD {
				t.Errorf("transfer too small: %.2f", a.AmountUSD)
			}
		}
	}
	if !found {
		t.Error("expected rebalance from over-allocated binance")
	}
}

func TestMarginCriticalTopUp(t *testing.T) {
	cfg := DefaultBalancerConfig()
	b := NewBalancer(cfg)
	now := time.Now().UnixMilli()

	balances := []VenueBalance{
		{Venue: "binance", EquityUSD: 10000, FreeMarginUSD: 8000, UsedMarginUSD: 2000, UtilizationPct: 20, TsMs: now},
		{Venue: "okx", EquityUSD: 5000, FreeMarginUSD: 500, UsedMarginUSD: 4500, UtilizationPct: 90, TsMs: now}, // critical!
	}

	snap := b.Analyse(balances)
	urgent := false
	for _, a := range snap.Actions {
		if a.ToVenue == "okx" && a.Priority == 1 {
			urgent = true
		}
	}
	if !urgent {
		t.Error("expected urgent top-up for OKX (90% utilization)")
	}
}

func TestMarginEfficiency(t *testing.T) {
	cfg := DefaultBalancerConfig()
	b := NewBalancer(cfg)
	now := time.Now().UnixMilli()

	balances := []VenueBalance{
		{Venue: "binance", EquityUSD: 10000, FreeMarginUSD: 6000, UsedMarginUSD: 4000, UtilizationPct: 40, TsMs: now},
		{Venue: "okx", EquityUSD: 5000, FreeMarginUSD: 3000, UsedMarginUSD: 2000, UtilizationPct: 40, TsMs: now},
	}

	report := b.MarginEfficiency(balances)
	if report.TotalEquityUSD != 15000 {
		t.Errorf("expected 15000, got %.2f", report.TotalEquityUSD)
	}
	if report.VenueCount != 2 {
		t.Errorf("expected 2 venues, got %d", report.VenueCount)
	}
	if report.UtilizationPct < 30 || report.UtilizationPct > 50 {
		t.Errorf("utilization out of range: %.2f%%", report.UtilizationPct)
	}
}
