// Package defirisk provides DeFi risk models including impermanent loss
// estimation, depeg detection, oracle sanity checks, and protocol risk scoring.
package defirisk

import (
	"math"
	"sort"
	"sync"
	"time"
)

// ILEstimate holds impermanent loss calculations for an LP position.
type ILEstimate struct {
	Pool          string  `json:"pool"`
	Token0        string  `json:"token0"`
	Token1        string  `json:"token1"`
	EntryPrice    float64 `json:"entry_price"`    // token0/token1 at entry
	CurrentPrice  float64 `json:"current_price"`  // token0/token1 now
	PriceRatio    float64 `json:"price_ratio"`    // current / entry
	ILPct         float64 `json:"il_pct"`         // impermanent loss %
	FeesEarnedPct float64 `json:"fees_earned_pct"` // estimated fees earned %
	NetPnLPct     float64 `json:"net_pnl_pct"`    // fees - IL
	HoldValue     float64 `json:"hold_value"`     // value if just held
	LPValue       float64 `json:"lp_value"`       // current LP value
	PositionUSD   float64 `json:"position_usd"`
}

// CalculateIL computes impermanent loss for a constant-product AMM.
func CalculateIL(entryPrice, currentPrice, positionUSD, feesEarnedPct float64) ILEstimate {
	ratio := currentPrice / entryPrice
	// IL formula: 2*sqrt(ratio)/(1+ratio) - 1
	ilFactor := 2*math.Sqrt(ratio)/(1+ratio) - 1
	ilPct := ilFactor * 100

	holdValue := positionUSD * (1 + (ratio-1)/2) // simplified
	lpValue := positionUSD * (1 + ilFactor)

	return ILEstimate{
		EntryPrice:    entryPrice,
		CurrentPrice:  currentPrice,
		PriceRatio:    ratio,
		ILPct:         ilPct,
		FeesEarnedPct: feesEarnedPct,
		NetPnLPct:     feesEarnedPct + ilPct, // IL is negative
		HoldValue:     holdValue,
		LPValue:       lpValue,
		PositionUSD:   positionUSD,
	}
}

// DepegDetector monitors stablecoin and pegged asset deviations.
type DepegDetector struct {
	mu         sync.RWMutex
	prices     map[string][]priceObs // asset → recent prices
	threshBps  float64               // depeg threshold in bps
	windowMs   int64
	maxSamples int
}

type priceObs struct {
	price float64
	tsMs  int64
}

// DepegAlert is emitted when a pegged asset deviates beyond threshold.
type DepegAlert struct {
	Asset       string  `json:"asset"`
	PegTarget   float64 `json:"peg_target"`
	CurrentPrice float64 `json:"current_price"`
	DeviationBps float64 `json:"deviation_bps"`
	Duration     int64   `json:"duration_ms"` // how long depegged
	Severity     string  `json:"severity"`    // WARNING, CRITICAL
	TsMs         int64   `json:"ts_ms"`
}

// NewDepegDetector creates a depeg detector.
func NewDepegDetector(threshBps float64, windowMs int64) *DepegDetector {
	return &DepegDetector{
		prices:     make(map[string][]priceObs),
		threshBps:  threshBps,
		windowMs:   windowMs,
		maxSamples: 10000,
	}
}

// RecordPrice records a price observation for a pegged asset.
func (d *DepegDetector) RecordPrice(asset string, price float64, tsMs int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prices[asset] = append(d.prices[asset], priceObs{price: price, tsMs: tsMs})
	if len(d.prices[asset]) > d.maxSamples {
		d.prices[asset] = d.prices[asset][len(d.prices[asset])-d.maxSamples:]
	}
}

// Check evaluates all tracked assets for depeg conditions.
func (d *DepegDetector) Check(pegTargets map[string]float64) []DepegAlert {
	d.mu.RLock()
	defer d.mu.RUnlock()

	now := time.Now().UnixMilli()
	cutoff := now - d.windowMs
	var alerts []DepegAlert

	for asset, target := range pegTargets {
		obs := d.prices[asset]
		if len(obs) == 0 {
			continue
		}

		// Latest price
		latest := obs[len(obs)-1]
		devBps := math.Abs(latest.price-target) / target * 10000

		if devBps >= d.threshBps {
			// Find how long it's been depegged
			depegStart := latest.tsMs
			for i := len(obs) - 2; i >= 0; i-- {
				if obs[i].tsMs < cutoff {
					break
				}
				dev := math.Abs(obs[i].price-target) / target * 10000
				if dev < d.threshBps {
					break
				}
				depegStart = obs[i].tsMs
			}

			severity := "WARNING"
			if devBps >= d.threshBps*3 || (latest.tsMs-depegStart) > 300000 {
				severity = "CRITICAL"
			}

			alerts = append(alerts, DepegAlert{
				Asset:        asset,
				PegTarget:    target,
				CurrentPrice: latest.price,
				DeviationBps: devBps,
				Duration:     latest.tsMs - depegStart,
				Severity:     severity,
				TsMs:         now,
			})
		}
	}

	return alerts
}

// OracleChecker validates oracle feeds for sanity.
type OracleChecker struct {
	mu           sync.RWMutex
	feeds        map[string][]oracleObs // feedID → observations
	maxDevBps    float64
	maxStaleSec  int64
	maxSamples   int
}

type oracleObs struct {
	price     float64
	tsMs      int64
	source    string
}

// OracleAlert describes an oracle anomaly.
type OracleAlert struct {
	FeedID      string  `json:"feed_id"`
	Type        string  `json:"type"` // STALE, DEVIATION, FLATLINE
	Details     string  `json:"details"`
	Severity    string  `json:"severity"`
	TsMs        int64   `json:"ts_ms"`
}

// NewOracleChecker creates an oracle sanity checker.
func NewOracleChecker(maxDevBps float64, maxStaleSec int64) *OracleChecker {
	return &OracleChecker{
		feeds:       make(map[string][]oracleObs),
		maxDevBps:   maxDevBps,
		maxStaleSec: maxStaleSec,
		maxSamples:  5000,
	}
}

// RecordOraclePrice records a price from an oracle feed.
func (oc *OracleChecker) RecordOraclePrice(feedID, source string, price float64, tsMs int64) {
	oc.mu.Lock()
	defer oc.mu.Unlock()
	oc.feeds[feedID] = append(oc.feeds[feedID], oracleObs{
		price:  price,
		tsMs:   tsMs,
		source: source,
	})
	if len(oc.feeds[feedID]) > oc.maxSamples {
		oc.feeds[feedID] = oc.feeds[feedID][len(oc.feeds[feedID])-oc.maxSamples:]
	}
}

// Validate checks all oracle feeds for anomalies.
func (oc *OracleChecker) Validate(refPrices map[string]float64) []OracleAlert {
	oc.mu.RLock()
	defer oc.mu.RUnlock()

	now := time.Now().UnixMilli()
	var alerts []OracleAlert

	for feedID, obs := range oc.feeds {
		if len(obs) == 0 {
			continue
		}

		latest := obs[len(obs)-1]

		// Check staleness
		ageSec := (now - latest.tsMs) / 1000
		if ageSec > oc.maxStaleSec {
			alerts = append(alerts, OracleAlert{
				FeedID:   feedID,
				Type:     "STALE",
				Details:  "oracle feed stale",
				Severity: "WARNING",
				TsMs:     now,
			})
		}

		// Check deviation from reference
		if ref, ok := refPrices[feedID]; ok {
			devBps := math.Abs(latest.price-ref) / ref * 10000
			if devBps > oc.maxDevBps {
				alerts = append(alerts, OracleAlert{
					FeedID:   feedID,
					Type:     "DEVIATION",
					Details:  "oracle deviates from reference",
					Severity: "CRITICAL",
					TsMs:     now,
				})
			}
		}

		// Check flatline (price unchanged for many observations)
		if len(obs) >= 10 {
			allSame := true
			for i := len(obs) - 10; i < len(obs); i++ {
				if obs[i].price != obs[len(obs)-1].price {
					allSame = false
					break
				}
			}
			if allSame {
				alerts = append(alerts, OracleAlert{
					FeedID:   feedID,
					Type:     "FLATLINE",
					Details:  "oracle price unchanged for 10+ observations",
					Severity: "WARNING",
					TsMs:     now,
				})
			}
		}
	}

	return alerts
}

// ProtocolRiskScorer rates DeFi protocol risk.
type ProtocolRiskScorer struct {
	mu       sync.RWMutex
	profiles map[string]*ProtocolProfile
}

// ProtocolProfile captures risk factors for a DeFi protocol.
type ProtocolProfile struct {
	Protocol      string  `json:"protocol"`
	AuditCount    int     `json:"audit_count"`
	TVLMillions   float64 `json:"tvl_millions"`
	AgeMonths     int     `json:"age_months"`
	IncidentCount int     `json:"incident_count"`
	IsUpgradeable bool    `json:"is_upgradeable"`
	HasTimelock   bool    `json:"has_timelock"`
	ChainCount    int     `json:"chain_count"`
	RiskScore     float64 `json:"risk_score"` // 0-100, lower = safer
}

// NewProtocolRiskScorer creates a protocol risk scorer.
func NewProtocolRiskScorer() *ProtocolRiskScorer {
	return &ProtocolRiskScorer{
		profiles: make(map[string]*ProtocolProfile),
	}
}

// SetProfile registers or updates a protocol's risk profile.
func (s *ProtocolRiskScorer) SetProfile(p ProtocolProfile) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.RiskScore = s.computeScore(&p)
	s.profiles[p.Protocol] = &p
}

// Score returns the risk score for a protocol (0-100, lower = safer).
func (s *ProtocolRiskScorer) Score(protocol string) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if p, ok := s.profiles[protocol]; ok {
		return p.RiskScore
	}
	return 100 // unknown protocol = max risk
}

// RankedProtocols returns protocols sorted by risk score (safest first).
func (s *ProtocolRiskScorer) RankedProtocols() []ProtocolProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []ProtocolProfile
	for _, p := range s.profiles {
		result = append(result, *p)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].RiskScore < result[j].RiskScore
	})
	return result
}

func (s *ProtocolRiskScorer) computeScore(p *ProtocolProfile) float64 {
	score := 50.0 // base

	// Audits reduce risk
	score -= math.Min(float64(p.AuditCount)*5, 20)

	// TVL reduces risk (battle-tested)
	if p.TVLMillions > 1000 {
		score -= 15
	} else if p.TVLMillions > 100 {
		score -= 10
	} else if p.TVLMillions > 10 {
		score -= 5
	}

	// Age reduces risk
	if p.AgeMonths > 24 {
		score -= 10
	} else if p.AgeMonths > 12 {
		score -= 5
	}

	// Incidents increase risk
	score += float64(p.IncidentCount) * 10

	// Upgradeability increases risk
	if p.IsUpgradeable && !p.HasTimelock {
		score += 15
	} else if p.IsUpgradeable {
		score += 5
	}

	return math.Max(0, math.Min(100, score))
}
