package arb

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

const StrategyCorrelationBreak = "CORRELATION_BREAK"

// CorrelationTracker monitors the rolling correlation between two symbols
// (e.g. BTC-PERP and ETH-PERP). When correlation breaks down significantly,
// it generates pair-trade intents.
type CorrelationTracker struct {
	mu       sync.Mutex
	primary  string // e.g. "BTC-PERP"
	secondary string // e.g. "ETH-PERP"
	// Per-venue price observations.
	prices   map[string]*corrPrices // venue → prices
	windowSize int
}

type corrPrices struct {
	primaryObs   []corrObs
	secondaryObs []corrObs
	pidx, sidx   int
	pfull, sfull bool
	cap          int
}

type corrObs struct {
	mid  float64
	tsMs int64
}

// CorrelationConfig configures the correlation break strategy.
type CorrelationConfig struct {
	Enabled            bool    `yaml:"enabled"`
	PrimarySymbol      string  `yaml:"primary_symbol"`   // e.g. "BTC-PERP"
	SecondarySymbol    string  `yaml:"secondary_symbol"`  // e.g. "ETH-PERP"
	WindowSize         int     `yaml:"window_size"`       // number of observations
	MinCorrelation     float64 `yaml:"min_correlation"`   // baseline correlation (e.g. 0.85)
	BreakThreshold     float64 `yaml:"break_threshold"`   // correlation drop to trigger (e.g. 0.5)
	SpreadZScoreMin    float64 `yaml:"spread_z_score_min"` // min z-score of spread deviation
	NotionalUSD        float64 `yaml:"notional_usd"`
	MaxSlippageBps     float64 `yaml:"max_slippage_bps"`
	IntentTTLMs        int64   `yaml:"intent_ttl_ms"`
	CooldownMs         int64   `yaml:"cooldown_ms"`
}

// CorrelationSnapshot holds the current correlation state for a venue.
type CorrelationSnapshot struct {
	Venue       string
	Correlation float64
	SpreadZScore float64
	PrimaryMid  float64
	SecondaryMid float64
}

// NewCorrelationTracker creates a tracker for the given symbol pair.
func NewCorrelationTracker(primary, secondary string, windowSize int) *CorrelationTracker {
	if windowSize < 20 {
		windowSize = 100
	}
	return &CorrelationTracker{
		primary:    primary,
		secondary:  secondary,
		prices:     make(map[string]*corrPrices),
		windowSize: windowSize,
	}
}

// RecordQuote ingests a market quote and updates the appropriate series.
func (ct *CorrelationTracker) RecordQuote(q consensus.Quote) {
	sym := string(q.Symbol)
	if sym != ct.primary && sym != ct.secondary {
		return
	}
	if q.BestBid <= 0 || q.BestAsk <= 0 {
		return
	}
	mid := (q.BestBid + q.BestAsk) / 2

	ct.mu.Lock()
	defer ct.mu.Unlock()

	venue := string(q.Venue)
	cp, ok := ct.prices[venue]
	if !ok {
		cp = &corrPrices{
			primaryObs:   make([]corrObs, ct.windowSize),
			secondaryObs: make([]corrObs, ct.windowSize),
			cap:          ct.windowSize,
		}
		ct.prices[venue] = cp
	}

	obs := corrObs{mid: mid, tsMs: q.TsMs}
	if sym == ct.primary {
		cp.primaryObs[cp.pidx%cp.cap] = obs
		cp.pidx++
		if cp.pidx >= cp.cap {
			cp.pfull = true
		}
	} else {
		cp.secondaryObs[cp.sidx%cp.cap] = obs
		cp.sidx++
		if cp.sidx >= cp.cap {
			cp.sfull = true
		}
	}
}

// Snapshot returns the current correlation state for a venue.
func (ct *CorrelationTracker) Snapshot(venue string) *CorrelationSnapshot {
	ct.mu.Lock()
	defer ct.mu.Unlock()

	cp, ok := ct.prices[venue]
	if !ok {
		return nil
	}
	pCount := cp.pidx
	if cp.pfull {
		pCount = cp.cap
	}
	sCount := cp.sidx
	if cp.sfull {
		sCount = cp.cap
	}
	n := pCount
	if sCount < n {
		n = sCount
	}
	if n < 20 {
		return nil
	}

	// Use the most recent n observations from each.
	pReturns := logReturns(cp.primaryObs, cp.pidx, cp.pfull, cp.cap, n)
	sReturns := logReturns(cp.secondaryObs, cp.sidx, cp.sfull, cp.cap, n)

	if len(pReturns) < 10 || len(sReturns) < 10 {
		return nil
	}

	// Align to same length.
	minLen := len(pReturns)
	if len(sReturns) < minLen {
		minLen = len(sReturns)
	}
	pReturns = pReturns[:minLen]
	sReturns = sReturns[:minLen]

	corr := pearsonCorrelation(pReturns, sReturns)

	// Compute spread z-score: ratio deviation from mean.
	latestP := cp.primaryObs[(cp.pidx-1+cp.cap)%cp.cap].mid
	latestS := cp.secondaryObs[(cp.sidx-1+cp.cap)%cp.cap].mid
	spreadZ := 0.0
	if latestP > 0 && latestS > 0 {
		spreadZ = computeSpreadZScore(cp, ct.windowSize)
	}

	return &CorrelationSnapshot{
		Venue:        venue,
		Correlation:  corr,
		SpreadZScore: spreadZ,
		PrimaryMid:   latestP,
		SecondaryMid: latestS,
	}
}

// EvaluateCorrelationTrades checks for correlation breaks and emits pair trades.
func (ct *CorrelationTracker) EvaluateCorrelationTrades(cfg CorrelationConfig, tenantID string, cooldown *Cooldown) []TradeIntent {
	if !cfg.Enabled {
		return nil
	}

	ct.mu.Lock()
	venues := make([]string, 0, len(ct.prices))
	for v := range ct.prices {
		venues = append(venues, v)
	}
	ct.mu.Unlock()

	now := time.Now().UnixMilli()
	var intents []TradeIntent

	for _, venue := range venues {
		snap := ct.Snapshot(venue)
		if snap == nil {
			continue
		}

		// Check for correlation break.
		if snap.Correlation > cfg.BreakThreshold {
			continue // correlation still holding
		}
		if math.Abs(snap.SpreadZScore) < cfg.SpreadZScoreMin {
			continue // spread not extreme enough
		}

		cooldownKey := "corr:" + venue
		if cooldown != nil && cooldown.IsOnCooldown(cooldownKey, now) {
			continue
		}

		log.Printf("arb: CORRELATION BREAK venue=%s corr=%.3f spread_z=%.2f",
			venue, snap.Correlation, snap.SpreadZScore)

		// Pair trade: when spread z-score is positive (primary outperforming),
		// sell primary + buy secondary, expecting mean reversion.
		var primaryAction, secondaryAction string
		if snap.SpreadZScore > 0 {
			primaryAction = "SELL"
			secondaryAction = "BUY"
		} else {
			primaryAction = "BUY"
			secondaryAction = "SELL"
		}

		intent := TradeIntent{
			TenantID:  tenantID,
			IntentID:  newUUID(),
			Strategy:  StrategyCorrelationBreak,
			Symbol:    cfg.PrimarySymbol + "/" + cfg.SecondarySymbol,
			TsMs:      now,
			ExpiresMs: now + cfg.IntentTTLMs,
			Legs: []TradeLeg{
				{
					Venue:          venue,
					Action:         primaryAction,
					Market:         "PERP",
					Type:           "MARKET_OR_IOC",
					NotionalUSD:    cfg.NotionalUSD,
					MaxSlippageBps: cfg.MaxSlippageBps,
					PriceLimit:     0, // market order
				},
				{
					Venue:          venue,
					Action:         secondaryAction,
					Market:         "PERP",
					Type:           "MARKET_OR_IOC",
					NotionalUSD:    cfg.NotionalUSD,
					MaxSlippageBps: cfg.MaxSlippageBps,
					PriceLimit:     0,
				},
			},
			Expected: ExpectedMetrics{
				EdgeBpsGross: math.Abs(snap.SpreadZScore) * 5, // ~5 bps per z-score unit
				EdgeBpsNet:   math.Abs(snap.SpreadZScore)*5 - 8,
			},
			Constraints: IntentConstraints{
				MinQuality:      "MED",
				MaxAgeMs:        cfg.IntentTTLMs,
				CooldownKey:     cooldownKey,
				HedgePreference: "SIMULTANEOUS_OR_HEDGE_FIRST",
			},
		}
		intents = append(intents, intent)
		if cooldown != nil {
			cooldown.Mark(cooldownKey, now)
		}
	}
	return intents
}

func logReturns(obs []corrObs, idx int, full bool, cap, n int) []float64 {
	count := idx
	if full {
		count = cap
	}
	if count < 2 {
		return nil
	}
	if n > count {
		n = count
	}

	returns := make([]float64, 0, n-1)
	for i := 1; i < n; i++ {
		curr := obs[(idx-1-i+cap+cap)%cap]
		prev := obs[(idx-2-i+cap+cap)%cap]
		if curr.mid > 0 && prev.mid > 0 {
			returns = append(returns, math.Log(curr.mid/prev.mid))
		}
	}
	return returns
}

func pearsonCorrelation(x, y []float64) float64 {
	n := len(x)
	if n != len(y) || n < 2 {
		return 0
	}
	var sumX, sumY, sumXY, sumX2, sumY2 float64
	for i := 0; i < n; i++ {
		sumX += x[i]
		sumY += y[i]
		sumXY += x[i] * y[i]
		sumX2 += x[i] * x[i]
		sumY2 += y[i] * y[i]
	}
	nf := float64(n)
	num := nf*sumXY - sumX*sumY
	den := math.Sqrt((nf*sumX2 - sumX*sumX) * (nf*sumY2 - sumY*sumY))
	if den == 0 {
		return 0
	}
	return num / den
}

func computeSpreadZScore(cp *corrPrices, windowSize int) float64 {
	n := cp.pidx
	if cp.pfull {
		n = cp.cap
	}
	sn := cp.sidx
	if cp.sfull {
		sn = cp.cap
	}
	if n < 20 || sn < 20 {
		return 0
	}
	count := n
	if sn < count {
		count = sn
	}

	// Compute ratio (primary/secondary) statistics.
	var sum, sumSq float64
	for i := 0; i < count; i++ {
		p := cp.primaryObs[(cp.pidx-1-i+cp.cap+cp.cap)%cp.cap]
		s := cp.secondaryObs[(cp.sidx-1-i+cp.cap+cp.cap)%cp.cap]
		if p.mid > 0 && s.mid > 0 {
			ratio := p.mid / s.mid
			sum += ratio
			sumSq += ratio * ratio
		}
	}
	mean := sum / float64(count)
	variance := sumSq/float64(count) - mean*mean
	if variance <= 0 {
		return 0
	}
	stdDev := math.Sqrt(variance)

	latestP := cp.primaryObs[(cp.pidx-1+cp.cap)%cp.cap].mid
	latestS := cp.secondaryObs[(cp.sidx-1+cp.cap)%cp.cap].mid
	if latestS <= 0 {
		return 0
	}
	currentRatio := latestP / latestS
	return (currentRatio - mean) / stdDev
}
