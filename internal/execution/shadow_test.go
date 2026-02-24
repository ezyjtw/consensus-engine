package execution

import (
	"math"
	"testing"
)

func TestShadowMetricsEmpty(t *testing.T) {
	sm := NewShadowMetrics()
	report := sm.ConfidenceReport()
	if report.FillCount != 0 {
		t.Errorf("expected 0 fills, got %d", report.FillCount)
	}
}

func TestShadowMetricsRecordAndReport(t *testing.T) {
	sm := NewShadowMetrics()

	// Record 3 fills with known edge values.
	sm.Record(&SimulatedFill{
		IntentID:           "a",
		EdgeAtSignalBps:    10.0,
		EdgeCapturedBps:    8.0,
		LatencyMs:          20,
		SlippageAssumedBps: 2.0,
	})
	sm.Record(&SimulatedFill{
		IntentID:           "b",
		EdgeAtSignalBps:    12.0,
		EdgeCapturedBps:    9.0,
		LatencyMs:          30,
		SlippageAssumedBps: 3.0,
	})
	sm.Record(&SimulatedFill{
		IntentID:           "c",
		EdgeAtSignalBps:    8.0,
		EdgeCapturedBps:    7.0,
		LatencyMs:          10,
		SlippageAssumedBps: 1.0,
	})

	report := sm.ConfidenceReport()
	if report.FillCount != 3 {
		t.Fatalf("expected 3 fills, got %d", report.FillCount)
	}

	// avg predicted = (10+12+8)/3 = 10
	if math.Abs(report.AvgPredictedEdgeBps-10.0) > 0.01 {
		t.Errorf("avg predicted edge: got %.2f, want 10.0", report.AvgPredictedEdgeBps)
	}

	// avg realized = (8+9+7)/3 = 8
	if math.Abs(report.AvgRealizedEdgeBps-8.0) > 0.01 {
		t.Errorf("avg realized edge: got %.2f, want 8.0", report.AvgRealizedEdgeBps)
	}

	// avg missed = (2+3+1)/3 = 2
	if math.Abs(report.AvgMissedEdgeBps-2.0) > 0.01 {
		t.Errorf("avg missed edge: got %.2f, want 2.0", report.AvgMissedEdgeBps)
	}

	// capture ratio = 8/10 = 0.8
	if math.Abs(report.EdgeCaptureRatio-0.8) > 0.01 {
		t.Errorf("edge capture ratio: got %.2f, want 0.80", report.EdgeCaptureRatio)
	}
}

func TestShadowMetricsNilFill(t *testing.T) {
	sm := NewShadowMetrics()
	sm.Record(nil) // should not panic
	if sm.ConfidenceReport().FillCount != 0 {
		t.Error("nil fill should not be recorded")
	}
}

func TestShadowMetricsCapAt1000(t *testing.T) {
	sm := NewShadowMetrics()
	for i := 0; i < 1100; i++ {
		sm.Record(&SimulatedFill{
			IntentID:        "x",
			EdgeAtSignalBps: 5.0,
			EdgeCapturedBps: 4.0,
		})
	}
	report := sm.ConfidenceReport()
	if report.FillCount != 1000 {
		t.Errorf("expected cap at 1000, got %d", report.FillCount)
	}
}
