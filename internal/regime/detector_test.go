package regime

import (
	"testing"
	"time"
)

func TestDetectorCalm(t *testing.T) {
	d := NewDetector(300000) // 5 minute window
	now := time.Now().UnixMilli()

	// Feed calm market data: stable price, tight spread
	for i := 0; i < 100; i++ {
		d.Record(Observation{
			Symbol:    "BTC-PERP",
			Mid:       100000 + float64(i%3), // minor fluctuation
			SpreadBps: 1.0,
			Volume1m:  50000,
			TsMs:      now - int64((100-i)*1000),
		})
	}

	state := d.Detect("BTC-PERP")
	if state.Regime != RegimeCalm {
		t.Errorf("expected CALM regime, got %s", state.Regime)
	}
}

func TestDetectorVolatile(t *testing.T) {
	d := NewDetector(300000)
	now := time.Now().UnixMilli()

	// Feed a large calm baseline (150 obs) so baseline is solidly "calm"
	for i := 0; i < 150; i++ {
		d.Record(Observation{
			Symbol:    "BTC-PERP",
			Mid:       100000,
			SpreadBps: 1.0,
			Volume1m:  50000,
			TsMs:      now - int64((200-i)*1000),
		})
	}

	// Then volatile data in the most recent 50 observations: large swings, very wide spreads
	for i := 0; i < 50; i++ {
		swing := float64(i%2)*400 - 200 // ±200
		d.Record(Observation{
			Symbol:    "BTC-PERP",
			Mid:       100000 + swing,
			SpreadBps: 15.0,
			Volume1m:  200000,
			TsMs:      now - int64((50-i)*1000),
		})
	}

	state := d.Detect("BTC-PERP")
	if state.Regime == RegimeCalm {
		t.Errorf("expected non-CALM regime with volatile data, got %s (vol=%.2f spread_widen=%.2f)",
			state.Regime, state.VolatilityBps, state.SpreadWidening)
	}
	if state.SpreadWidening <= 1.0 {
		t.Errorf("expected spread widening > 1.0, got %.2f", state.SpreadWidening)
	}
}

func TestAdjustmentForRegime(t *testing.T) {
	tests := []struct {
		regime MarketRegime
		sizing float64
		minEdge float64
	}{
		{RegimeCalm, 1.2, 0.8},
		{RegimeVolatile, 0.5, 1.5},
		{RegimeCascade, 0.25, 2.0},
	}

	for _, tt := range tests {
		adj := adjustmentForRegime(tt.regime)
		if adj.SizingMultiplier != tt.sizing {
			t.Errorf("%s: expected sizing %.2f, got %.2f", tt.regime, tt.sizing, adj.SizingMultiplier)
		}
		if adj.MinEdgeMultiplier != tt.minEdge {
			t.Errorf("%s: expected minEdge %.2f, got %.2f", tt.regime, tt.minEdge, adj.MinEdgeMultiplier)
		}
	}
}
