package consensus

import (
	"math"
	"testing"
)

func TestMedian(t *testing.T) {
	tests := []struct {
		name  string
		input []float64
		want  float64
	}{
		{"empty", []float64{}, 0},
		{"single", []float64{5}, 5},
		{"odd", []float64{3, 1, 2}, 2},
		{"even", []float64{1, 2, 3, 4}, 2.5},
		{"identical", []float64{7, 7, 7, 7}, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Median(tt.input); math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("Median(%v) = %.6f, want %.6f", tt.input, got, tt.want)
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	data := []float64{10, 20, 30, 40, 50}
	tests := []struct {
		p    float64
		want float64
	}{
		{0, 10},
		{25, 20},
		{50, 30},
		{75, 40},
		{100, 50},
	}
	for _, tt := range tests {
		if got := Percentile(data, tt.p); math.Abs(got-tt.want) > 1e-9 {
			t.Errorf("Percentile(%v, %.0f) = %.6f, want %.6f", data, tt.p, got, tt.want)
		}
	}
}

func TestPercentileEmpty(t *testing.T) {
	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("Percentile(nil, 50) = %v, want 0", got)
	}
}

// spec §8 edge case 1: MAD=0 when all mids are identical
func TestMADAllIdentical(t *testing.T) {
	if got := MAD([]float64{100, 100, 100, 100}); got != 0 {
		t.Errorf("MAD of identical values should be 0, got %v", got)
	}
}

func TestDeviationBps(t *testing.T) {
	// 1% deviation = 100 bps
	if got := DeviationBps(101, 100); math.Abs(got-100) > 1e-9 {
		t.Errorf("DeviationBps(101,100) = %.6f, want 100", got)
	}
	// zero ref → 0
	if got := DeviationBps(999, 0); got != 0 {
		t.Errorf("DeviationBps with zero ref = %v, want 0", got)
	}
}

// spec §8 band computation: P25(sells) < P75(buys)
func TestBandPercentiles(t *testing.T) {
	sells := []float64{99990, 99992, 99994, 99996}
	buys := []float64{100010, 100012, 100014, 100016}

	// rank = 0.25*3 = 0.75 → interp(99990,99992,0.75) = 99991.5
	wantBandLow := 99991.5
	// rank = 0.75*3 = 2.25 → interp(100014,100016,0.25) = 100014.5
	wantBandHigh := 100014.5

	if got := Percentile(sells, 25); math.Abs(got-wantBandLow) > 0.01 {
		t.Errorf("band_low P25(sells) = %.4f, want %.4f", got, wantBandLow)
	}
	if got := Percentile(buys, 75); math.Abs(got-wantBandHigh) > 0.01 {
		t.Errorf("band_high P75(buys) = %.4f, want %.4f", got, wantBandHigh)
	}
}
