package consensus

import "time"

type Engine struct {
	policy *Policy
}

func NewEngine(policy *Policy) *Engine {
	return &Engine{policy: policy}
}

type ComputeResult struct {
	Update        ConsensusUpdate
	Anomalies     []VenueAnomaly
	StatusUpdates []VenueStatusUpdate
	NewStatuses   map[Venue]VenueStatus
}

func (e *Engine) Compute(
	tenantID string,
	symbol Symbol,
	quotes map[Venue]Quote,
	getStatus func(v Venue) VenueStatus,
) ComputeResult {
	nowMs := time.Now().UnixMilli()
	p := e.policy.ResolvedPolicy(string(symbol))

	// Stale data pause: if every quote is older than StalePauseMs, return an
	// empty consensus with quality=LOW to prevent downstream use of stale prices.
	if p.StalePauseMs > 0 {
		allStale := true
		for _, q := range quotes {
			if nowMs-q.TsMs < p.StalePauseMs {
				allStale = false
				break
			}
		}
		if allStale {
			return ComputeResult{
				Update: ConsensusUpdate{
					TenantID:        tenantID,
					Symbol:          symbol,
					TsMs:            nowMs,
					SizeNotionalUSD: p.SizeNotionalUSD,
					Consensus:       Consensus{Quality: "LOW"},
				},
				NewStatuses: make(map[Venue]VenueStatus),
			}
		}
	}

	type venueRaw struct {
		venue           Venue
		quote           Quote
		mid             float64
		buyExec         float64
		sellExec        float64
		effectiveBuy    float64
		effectiveSell   float64
		spreadBps       float64
		sufficientDepth bool
	}

	var eligible []venueRaw
	for v, q := range quotes {
		mid := ComputeMid(q)
		buy, buyOK := ComputeExecutableBuy(q, p.SizeNotionalUSD,
			p.SlippageBufferBps, p.DepthPenaltyBps)
		sell, sellOK := ComputeExecutableSell(q, p.SizeNotionalUSD,
			p.SlippageBufferBps, p.DepthPenaltyBps)
		eligible = append(eligible, venueRaw{
			venue:           v,
			quote:           q,
			mid:             mid,
			buyExec:         buy,
			sellExec:        sell,
			effectiveBuy:    ApplyFeesBuy(buy, q.FeeBpsTaker),
			effectiveSell:   ApplyFeesSell(sell, q.FeeBpsTaker),
			spreadBps:       SpreadBps(q.BestBid, q.BestAsk),
			sufficientDepth: buyOK && sellOK,
		})
	}

	mids := make([]float64, len(eligible))
	for i, r := range eligible {
		mids[i] = r.mid
	}
	medianMid := Median(mids)
	mad := MAD(mids)

	rawTrust := make(map[Venue]float64, len(eligible))
	newStatuses := make(map[Venue]VenueStatus, len(eligible))
	var anomalies []VenueAnomaly
	var statusUpdates []VenueStatusUpdate
	var venueMetrics []VenueMetrics

	for _, r := range eligible {
		devBps := DeviationBps(r.mid, medianMid)
		zScore := RobustZScore(r.mid, medianMid, mad)
		isOutlier := zScore > 6 || devBps > p.OutlierBpsWarn
		isHardOutlier := devBps > p.OutlierBpsBlacklist

		curStatus := getStatus(r.venue)
		nextStatus, anomaly, transitioned := UpdateVenueStatus(
			nowMs, curStatus, devBps, p)
		newStatuses[r.venue] = nextStatus

		if transitioned {
			su := VenueStatusUpdate{
				TenantID: tenantID,
				Venue:    r.venue,
				Symbol:   symbol,
				TsMs:     nowMs,
				Status:   nextStatus.State,
				Reason:   nextStatus.Reason,
			}
			if nextStatus.State == StateBlacklisted {
				su.TtlMs = p.BlacklistTtlMs
			}
			statusUpdates = append(statusUpdates, su)
		}

		if anomaly != nil {
			anomalies = append(anomalies, VenueAnomaly{
				TenantID:          tenantID,
				Symbol:            symbol,
				Venue:             r.venue,
				TsMs:              nowMs,
				AnomalyType:       anomaly.AnomalyType,
				Severity:          anomaly.Severity,
				DeviationBps:      devBps,
				ConsensusMid:      medianMid,
				VenueMid:          r.mid,
				WindowMs:          anomaly.WindowMs,
				RecommendedAction: anomaly.RecommendedAction,
			})
		}

		stalenessMs := nowMs - r.quote.FeedHealth.LastMsgTsMs
		baseTrustVal := p.BaseTrust[string(r.venue)]
		if baseTrustVal == 0 {
			baseTrustVal = 0.05
		}
		t := ComputeTrust(baseTrustVal, TrustPenalties{
			IsOutlier:     isOutlier,
			IsHardOutlier: isHardOutlier,
			SpreadBps:     r.spreadBps,
			StalenessMs:   stalenessMs,
			StaleTholdMs:  p.StaleMs,
			State:         nextStatus.State,
		})
		rawTrust[r.venue] = t

		flags := []string{}
		if isOutlier {
			flags = append(flags, "OUTLIER")
		}
		if isHardOutlier {
			flags = append(flags, "HARD_OUTLIER")
		}
		if !r.sufficientDepth {
			flags = append(flags, "INSUFFICIENT_DEPTH")
		}

		venueMetrics = append(venueMetrics, VenueMetrics{
			Venue:         r.venue,
			Status:        nextStatus.State,
			Mid:           r.mid,
			BuyExec:       r.buyExec,
			SellExec:      r.sellExec,
			EffectiveBuy:  r.effectiveBuy,
			EffectiveSell: r.effectiveSell,
			DeviationBps:  devBps,
			Flags:         flags,
		})
	}

	normTrust := NormalizeTrust(rawTrust)
	for i, vm := range venueMetrics {
		venueMetrics[i].Trust = normTrust[vm.Venue]
	}

	coreSet := p.CoreVenueSet()
	coreEligible := 0
	for _, vm := range venueMetrics {
		if coreSet[vm.Venue] && vm.Status != StateBlacklisted {
			coreEligible++
		}
	}

	var consMid, consBuy, consSell float64
	for _, r := range eligible {
		tw := normTrust[r.venue]
		consMid += tw * r.mid
		consBuy += tw * r.effectiveBuy
		consSell += tw * r.effectiveSell
	}

	var effBuys, effSells []float64
	for _, r := range eligible {
		if normTrust[r.venue] > 0 {
			effBuys = append(effBuys, r.effectiveBuy)
			effSells = append(effSells, r.effectiveSell)
		}
	}

	bandHigh := Percentile(effBuys, 75)
	bandLow := Percentile(effSells, 25)

	midRef := consMid
	if midRef < epsilon {
		midRef = medianMid
	}
	var widenBps float64
	switch {
	case coreEligible < 2:
		widenBps = 50
	case coreEligible == 2:
		widenBps = 25
	case coreEligible == 3:
		widenBps = 10
	}
	if widenBps > 0 {
		widen := midRef * widenBps / 10000.0
		bandHigh += widen
		bandLow -= widen
	}

	quality := qualityScore(coreEligible, venueMetrics, p.MinCoreQuorum)

	return ComputeResult{
		Update: ConsensusUpdate{
			TenantID:        tenantID,
			Symbol:          symbol,
			TsMs:            nowMs,
			SizeNotionalUSD: p.SizeNotionalUSD,
			Consensus: Consensus{
				Mid:      consMid,
				BuyExec:  consBuy,
				SellExec: consSell,
				BandLow:  bandLow,
				BandHigh: bandHigh,
				Quality:  quality,
			},
			Venues: venueMetrics,
		},
		Anomalies:     anomalies,
		StatusUpdates: statusUpdates,
		NewStatuses:   newStatuses,
	}
}

func qualityScore(coreEligible int, metrics []VenueMetrics, minQuorum int) string {
	warnCount := 0
	for _, m := range metrics {
		if m.Status == StateWarn {
			warnCount++
		}
	}
	switch {
	case coreEligible >= 4 && warnCount == 0:
		return "HIGH"
	case coreEligible >= minQuorum:
		return "MED"
	default:
		return "LOW"
	}
}
