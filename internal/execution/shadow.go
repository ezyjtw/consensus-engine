package execution

import (
	"math"
	"sync"
)

// ShadowMetrics tracks predicted-vs-realized edge for shadow mode confidence.
// After every shadow fill, Record() should be called to accumulate metrics.
// ConfidenceReport() returns the composite confidence snapshot used by the
// graduation harness and dashboard.
type ShadowMetrics struct {
	mu      sync.Mutex
	records []shadowRecord
}

type shadowRecord struct {
	intentID       string
	predictedEdge  float64 // edge_at_signal_bps
	realizedEdge   float64 // edge_captured_bps
	latencyMs      int64
	slippageBps    float64
	missedEdgeBps  float64 // predicted - realized
}

func NewShadowMetrics() *ShadowMetrics {
	return &ShadowMetrics{}
}

// Record accumulates a fill into the shadow metrics tracker.
func (sm *ShadowMetrics) Record(fill *SimulatedFill) {
	if fill == nil {
		return
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()

	missed := fill.EdgeAtSignalBps - fill.EdgeCapturedBps
	sm.records = append(sm.records, shadowRecord{
		intentID:      fill.IntentID,
		predictedEdge: fill.EdgeAtSignalBps,
		realizedEdge:  fill.EdgeCapturedBps,
		latencyMs:     fill.LatencyMs,
		slippageBps:   fill.SlippageAssumedBps,
		missedEdgeBps: missed,
	})
	// Keep last 1000 records.
	if len(sm.records) > 1000 {
		sm.records = sm.records[len(sm.records)-1000:]
	}
}

// ShadowConfidenceReport is the composite shadow confidence snapshot.
type ShadowConfidenceReport struct {
	FillCount              int     `json:"fill_count"`
	AvgPredictedEdgeBps    float64 `json:"avg_predicted_edge_bps"`
	AvgRealizedEdgeBps     float64 `json:"avg_realized_edge_bps"`
	AvgMissedEdgeBps       float64 `json:"avg_missed_edge_bps"`
	AvgSlippageBps         float64 `json:"avg_slippage_bps"`
	AvgLatencyMs           int64   `json:"avg_latency_ms"`
	EdgeCaptureRatio       float64 `json:"edge_capture_ratio"`
	RealizedEdgeStddevBps  float64 `json:"realized_edge_stddev_bps"`
	SlippageSensitivityBps float64 `json:"slippage_sensitivity_bps"`
}

// ConfidenceReport returns shadow mode confidence metrics.
func (sm *ShadowMetrics) ConfidenceReport() ShadowConfidenceReport {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	if len(sm.records) == 0 {
		return ShadowConfidenceReport{}
	}

	var totalPredicted, totalRealized, totalMissed, totalSlippage float64
	var totalLatency int64

	for _, r := range sm.records {
		totalPredicted += r.predictedEdge
		totalRealized += r.realizedEdge
		totalMissed += r.missedEdgeBps
		totalSlippage += r.slippageBps
		totalLatency += r.latencyMs
	}

	n := float64(len(sm.records))
	avgPredicted := totalPredicted / n
	avgRealized := totalRealized / n

	// Compute stddev of realized edge.
	var variance float64
	for _, r := range sm.records {
		diff := r.realizedEdge - avgRealized
		variance += diff * diff
	}
	stddev := math.Sqrt(variance / n)

	return ShadowConfidenceReport{
		FillCount:              len(sm.records),
		AvgPredictedEdgeBps:    avgPredicted,
		AvgRealizedEdgeBps:     avgRealized,
		AvgMissedEdgeBps:       totalMissed / n,
		AvgSlippageBps:         totalSlippage / n,
		AvgLatencyMs:           totalLatency / int64(len(sm.records)),
		EdgeCaptureRatio:       avgRealized / math.Max(avgPredicted, 0.01),
		RealizedEdgeStddevBps:  stddev,
		SlippageSensitivityBps: totalSlippage/n - avgRealized,
	}
}
