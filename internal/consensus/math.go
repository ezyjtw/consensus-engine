package consensus

import (
	"math"
	"sort"
)

const epsilon = 1e-10

func Median(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2.0
	}
	return sorted[n/2]
}

func Percentile(vals []float64, p float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	rank := (p / 100.0) * float64(n-1)
	lo := int(math.Floor(rank))
	hi := lo + 1
	if hi >= n {
		return sorted[n-1]
	}
	frac := rank - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

func DeviationBps(px, ref float64) float64 {
	if math.Abs(ref) < epsilon {
		return 0
	}
	return math.Abs(px-ref) / ref * 10000.0
}

func MAD(vals []float64) float64 {
	med := Median(vals)
	devs := make([]float64, len(vals))
	for i, v := range vals {
		devs[i] = math.Abs(v - med)
	}
	return Median(devs)
}

func RobustZScore(val, med, mad float64) float64 {
	return 0.6745 * math.Abs(val-med) / (mad + epsilon)
}

func SpreadBps(bid, ask float64) float64 {
	mid := (bid + ask) / 2.0
	if mid < epsilon {
		return 0
	}
	return (ask - bid) / mid * 10000.0
}
