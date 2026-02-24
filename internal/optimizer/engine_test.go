package optimizer

import (
	"testing"
	"time"
)

func TestOptimizerBasic(t *testing.T) {
	params := []Parameter{
		{Name: "min_edge_bps", Current: 3.0, Min: 1.0, Max: 10.0, StepSize: 0.5},
		{Name: "max_size_usd", Current: 10000, Min: 1000, Max: 50000, StepSize: 1000},
	}

	e := NewEngine(params, 300000)

	now := time.Now().UnixMilli()
	// Record some samples with positive correlation between edge threshold and PnL
	for i := 0; i < 30; i++ {
		edge := 3.0 + float64(i%5)*0.5
		pnl := edge * 2 // higher edge → better PnL
		e.Record(PerformanceSample{
			Params: map[string]float64{"min_edge_bps": edge, "max_size_usd": 10000},
			PnLUSD: pnl,
			Sharpe: pnl / 10,
			TsMs:   now - int64((30-i)*1000),
		})
	}

	result := e.Optimise()
	if result == nil {
		t.Fatal("expected optimisation result")
	}
	if result.SamplesUsed < 20 {
		t.Errorf("expected >= 20 samples, got %d", result.SamplesUsed)
	}
}

func TestOptimizerInsufficientSamples(t *testing.T) {
	params := []Parameter{
		{Name: "edge", Current: 3.0, Min: 1.0, Max: 10.0, StepSize: 0.5},
	}
	e := NewEngine(params, 300000)

	// Only 5 samples — below minimum
	now := time.Now().UnixMilli()
	for i := 0; i < 5; i++ {
		e.Record(PerformanceSample{
			Params: map[string]float64{"edge": 3.0},
			PnLUSD: 10,
			TsMs:   now - int64(i*1000),
		})
	}

	result := e.Optimise()
	if result != nil {
		t.Error("expected nil result with insufficient samples")
	}
}

func TestCurrentParams(t *testing.T) {
	params := []Parameter{
		{Name: "a", Current: 1.0, Min: 0, Max: 10, StepSize: 0.1},
		{Name: "b", Current: 5.0, Min: 0, Max: 10, StepSize: 0.5},
	}
	e := NewEngine(params, 300000)

	p := e.CurrentParams()
	if p["a"] != 1.0 || p["b"] != 5.0 {
		t.Errorf("unexpected params: %v", p)
	}
}
