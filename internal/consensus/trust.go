package consensus

type TrustPenalties struct {
	IsOutlier     bool
	IsHardOutlier bool
	SpreadBps     float64
	StalenessMs   int64
	StaleTholdMs  int64
	State         VenueState
}

func ComputeTrust(base float64, p TrustPenalties) float64 {
	if p.State == StateBlacklisted {
		return 0
	}
	t := base
	if p.IsHardOutlier {
		t *= 0.05
	} else if p.IsOutlier {
		t *= 0.10
	}
	if p.State == StateWarn {
		t *= 0.60
	}
	if p.StaleTholdMs > 0 && p.StalenessMs > p.StaleTholdMs/2 {
		fraction := float64(p.StalenessMs) / float64(p.StaleTholdMs)
		if fraction > 1 {
			fraction = 1
		}
		t *= 1.0 - 0.6*fraction
	}
	if p.SpreadBps > 25 {
		t *= 0.70
	} else if p.SpreadBps > 10 {
		t *= 0.80
	}
	return t
}

func NormalizeTrust(weights map[Venue]float64) map[Venue]float64 {
	sum := 0.0
	for _, w := range weights {
		if w > 0 {
			sum += w
		}
	}
	norm := make(map[Venue]float64, len(weights))
	if sum < epsilon {
		// All venues have zero trust (e.g. all blacklisted).
		// Return all-zero weights so no venue influences the consensus.
		return norm
	}
	for v, w := range weights {
		norm[v] = w / sum
	}
	return norm
}
