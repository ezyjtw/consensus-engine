package prediction

import (
	"testing"
	"time"
)

func TestLeaderMoveDetection(t *testing.T) {
	e := NewEngine(60000)
	now := time.Now().UnixMilli()

	input := PredictionInput{
		Symbol:        "BTC-PERP",
		VenueMids:     map[string]float64{"binance": 100050, "okx": 100000, "bybit": 100000},
		VenueSpreads:  map[string]float64{"binance": 1.0, "okx": 1.0, "bybit": 1.0},
		VenueDepths:   map[string]float64{"binance": 500000, "okx": 300000, "bybit": 200000},
		FlowScore:     0.0,
		LeaderVenue:   "binance",
		LeaderMoveBps: 5.0,
		TsMs:          now,
	}

	signals := e.Predict(input)
	found := false
	for _, s := range signals {
		if s.Type == SignalLeaderMove {
			found = true
			if s.Direction != "BUY" {
				t.Errorf("expected BUY direction, got %s", s.Direction)
			}
			if s.LeadVenue != "binance" {
				t.Errorf("expected binance as leader, got %s", s.LeadVenue)
			}
			if len(s.TargetVenues) == 0 {
				t.Error("expected target venues")
			}
		}
	}
	if !found {
		t.Error("expected LEADER_MOVE signal")
	}
}

func TestFlowImbalanceSignal(t *testing.T) {
	e := NewEngine(60000)
	now := time.Now().UnixMilli()

	input := PredictionInput{
		Symbol:       "BTC-PERP",
		VenueMids:    map[string]float64{"binance": 100000, "okx": 100000},
		VenueSpreads: map[string]float64{"binance": 1.0, "okx": 1.0},
		FlowScore:    0.8, // strong buy flow
		TsMs:         now,
	}

	signals := e.Predict(input)
	found := false
	for _, s := range signals {
		if s.Type == SignalFlowImbal {
			found = true
			if s.Direction != "BUY" {
				t.Errorf("expected BUY from positive flow, got %s", s.Direction)
			}
		}
	}
	if !found {
		t.Error("expected FLOW_IMBALANCE signal")
	}
}

func TestConvergenceSignal(t *testing.T) {
	e := NewEngine(60000)
	now := time.Now().UnixMilli()

	input := PredictionInput{
		Symbol:       "BTC-PERP",
		VenueMids:    map[string]float64{"binance": 100040, "okx": 99960, "bybit": 100000},
		VenueSpreads: map[string]float64{"binance": 2.0, "okx": 2.0, "bybit": 1.0},
		FlowScore:    0.0,
		TsMs:         now,
	}

	signals := e.Predict(input)
	found := false
	for _, s := range signals {
		if s.Type == SignalConvergence {
			found = true
			if s.ExpectedEdgeBps <= 0 {
				t.Error("expected positive edge from convergence")
			}
		}
	}
	if !found {
		t.Error("expected CONVERGENCE signal from diverged prices")
	}
}

func TestNoSignalWhenCalm(t *testing.T) {
	e := NewEngine(60000)
	now := time.Now().UnixMilli()

	input := PredictionInput{
		Symbol:       "BTC-PERP",
		VenueMids:    map[string]float64{"binance": 100000, "okx": 100001},
		VenueSpreads: map[string]float64{"binance": 1.0, "okx": 1.0},
		FlowScore:    0.1,
		TsMs:         now,
	}

	signals := e.Predict(input)
	// In a calm market with no divergence, no leader move, no flow:
	// Convergence won't fire (< 3 bps divergence)
	// Leader move won't fire (no leader move)
	// Flow imbalance won't fire (< 0.4)
	for _, s := range signals {
		if s.Type == SignalLeaderMove || s.Type == SignalFlowImbal {
			t.Errorf("unexpected signal in calm market: %s", s.Type)
		}
	}
}
