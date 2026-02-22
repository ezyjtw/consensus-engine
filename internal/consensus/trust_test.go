package consensus

import (
	"math"
	"testing"
)

// spec §10 test 4: trust renormalization — penalized venue, weights still sum to 1
func TestNormalizeTrust(t *testing.T) {
	weights := map[Venue]float64{
		"coinbase": 0.30,
		"kraken":   0.20,
		"deribit":  0.30,
		"binance":  0.0, // fully penalised (blacklisted)
	}
	norm := NormalizeTrust(weights)

	sum := 0.0
	for _, w := range norm {
		sum += w
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("normalised trust must sum to 1, got %.9f", sum)
	}
	if norm["binance"] != 0 {
		t.Errorf("blacklisted venue must have trust 0, got %.6f", norm["binance"])
	}
}

// spec §10 (bug-fix verification): all-zero weights must return all zeros, not equal distribution
func TestNormalizeTrustAllZero(t *testing.T) {
	weights := map[Venue]float64{
		"coinbase": 0.0,
		"binance":  0.0,
	}
	norm := NormalizeTrust(weights)
	for v, w := range norm {
		if w != 0 {
			t.Errorf("all-zero input: venue %s got %.6f, want 0", v, w)
		}
	}
}

func TestComputeTrustBlacklisted(t *testing.T) {
	t.Run("blacklisted returns 0", func(t *testing.T) {
		trust := ComputeTrust(1.0, TrustPenalties{State: StateBlacklisted})
		if trust != 0 {
			t.Errorf("blacklisted trust = %.6f, want 0", trust)
		}
	})
}

func TestComputeTrustSpreadTiers(t *testing.T) {
	base := 1.0
	// spread ≤ 10 bps: no spread penalty
	nopenalty := ComputeTrust(base, TrustPenalties{State: StateOK, SpreadBps: 5})
	// spread 10–25 bps: ×0.80
	tier1 := ComputeTrust(base, TrustPenalties{State: StateOK, SpreadBps: 15})
	// spread > 25 bps: ×0.70 only (NOT ×0.80 then ×0.70)
	tier2 := ComputeTrust(base, TrustPenalties{State: StateOK, SpreadBps: 30})

	if math.Abs(nopenalty-1.0) > 1e-9 {
		t.Errorf("spread 5bps: want 1.0, got %.6f", nopenalty)
	}
	if math.Abs(tier1-0.80) > 1e-9 {
		t.Errorf("spread 15bps: want 0.80, got %.6f", tier1)
	}
	if math.Abs(tier2-0.70) > 1e-9 {
		t.Errorf("spread 30bps: want 0.70, got %.6f (double-penalty bug?)", tier2)
	}
}
