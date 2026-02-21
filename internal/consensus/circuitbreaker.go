package consensus

func UpdateVenueStatus(
	nowMs int64,
	current VenueStatus,
	deviationBps float64,
	policy *Policy,
) (VenueStatus, *VenueAnomaly, bool) {
	next := current
	var anomaly *VenueAnomaly
	transitioned := false

	isOutlier := deviationBps > policy.OutlierBpsWarn
	isHardOutlier := deviationBps > policy.OutlierBpsBlacklist

	switch current.State {
	case StateBlacklisted:
		if nowMs >= current.BlacklistUntilMs {
			next.State = StateWarn
			next.WarnSinceMs = nowMs
			next.RecoverySinceMs = nowMs
			next.OutlierSinceMs = 0
			next.HardOutlierSinceMs = 0
			next.Reason = "Blacklist TTL expired; in recovery"
			transitioned = true
		}
		return next, anomaly, transitioned

	case StateOK:
		if isHardOutlier {
			if next.HardOutlierSinceMs == 0 {
				next.HardOutlierSinceMs = nowMs
			}
			if nowMs-next.HardOutlierSinceMs >= policy.BlacklistPersistMs {
				next, anomaly = blacklist(nowMs, next, deviationBps, policy,
					"PRICE_OUTLIER > blacklist threshold")
				transitioned = true
			}
		} else if isOutlier {
			next.HardOutlierSinceMs = 0
			if next.OutlierSinceMs == 0 {
				next.OutlierSinceMs = nowMs
			}
			if nowMs-next.OutlierSinceMs >= policy.WarnPersistMs {
				next.State = StateWarn
				next.WarnSinceMs = nowMs
				next.RecoverySinceMs = 0
				next.Reason = "PRICE_OUTLIER > warn threshold"
				transitioned = true
				anomaly = &VenueAnomaly{
					AnomalyType:       "PRICE_OUTLIER",
					Severity:          "MEDIUM",
					DeviationBps:      deviationBps,
					WindowMs:          policy.WarnPersistMs,
					RecommendedAction: "REDUCE_TRUST",
				}
			}
		} else {
			next.OutlierSinceMs = 0
			next.HardOutlierSinceMs = 0
		}

	case StateWarn:
		if isHardOutlier {
			if next.HardOutlierSinceMs == 0 {
				next.HardOutlierSinceMs = nowMs
			}
			if nowMs-next.HardOutlierSinceMs >= policy.BlacklistPersistMs {
				next, anomaly = blacklist(nowMs, next, deviationBps, policy,
					"PRICE_OUTLIER > blacklist threshold from WARN")
				transitioned = true
			}
		} else if !isOutlier {
			if next.RecoverySinceMs == 0 {
				next.RecoverySinceMs = nowMs
			}
			if nowMs-next.RecoverySinceMs >= policy.RecoveryMs {
				next.State = StateOK
				next.WarnSinceMs = 0
				next.RecoverySinceMs = 0
				next.OutlierSinceMs = 0
				next.HardOutlierSinceMs = 0
				next.Reason = "Recovered to OK"
				transitioned = true
			}
		} else {
			next.RecoverySinceMs = 0
			next.HardOutlierSinceMs = 0
		}
	}
	return next, anomaly, transitioned
}

func blacklist(nowMs int64, s VenueStatus, deviationBps float64,
	policy *Policy, reason string) (VenueStatus, *VenueAnomaly) {
	s.State = StateBlacklisted
	s.BlacklistUntilMs = nowMs + policy.BlacklistTtlMs
	s.WarnSinceMs = 0
	s.RecoverySinceMs = 0
	s.OutlierSinceMs = 0
	s.HardOutlierSinceMs = 0
	s.Reason = reason
	a := &VenueAnomaly{
		AnomalyType:       "PRICE_OUTLIER",
		Severity:          "HIGH",
		DeviationBps:      deviationBps,
		WindowMs:          policy.BlacklistPersistMs,
		RecommendedAction: "BLACKLIST_60S",
	}
	return s, a
}
