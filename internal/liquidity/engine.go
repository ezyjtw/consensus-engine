package liquidity

import (
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// Policy configures the liquidity engine's detection thresholds.
type Policy struct {
	// Spread blowout: trigger when spread > SpreadBlowoutMult × rolling baseline.
	SpreadBlowoutMult float64 `yaml:"spread_blowout_mult"`
	// Rolling baseline window for spread (number of samples).
	SpreadBaselineSamples int `yaml:"spread_baseline_samples"`
	// Thin book: trigger when 1% depth < this USD threshold.
	ThinBookThresholdUSD float64 `yaml:"thin_book_threshold_usd"`
	// Mark/index divergence threshold in bps.
	MarkIndexDivergeBps float64 `yaml:"mark_index_diverge_bps"`
	// Order book imbalance: bid_depth / ask_depth > this or < 1/this.
	ImbalanceRatio float64 `yaml:"imbalance_ratio"`
	// Liquidation cascade: require price move > this bps in 1m.
	LiqCascadePriceBps float64 `yaml:"liq_cascade_price_bps"`
	// Notional for SPREAD_CAPTURE intents (USD).
	SpreadCaptureNotionalUSD float64 `yaml:"spread_capture_notional_usd"`
	// Notional for LIQUIDATION_CONTRA intents (USD).
	LiqContraNotionalUSD float64 `yaml:"liq_contra_notional_usd"`
	// Cooldown between intents per venue+symbol (ms).
	CooldownMs int64 `yaml:"cooldown_ms"`
	// Min consensus quality required.
	MinConsensusQuality string `yaml:"min_consensus_quality"`
}

func DefaultPolicy() *Policy {
	return &Policy{
		SpreadBlowoutMult:        3.0,
		SpreadBaselineSamples:    50,
		ThinBookThresholdUSD:     100000,
		MarkIndexDivergeBps:      20,
		ImbalanceRatio:           5.0,
		LiqCascadePriceBps:       30,
		SpreadCaptureNotionalUSD: 5000,
		LiqContraNotionalUSD:     2500,
		CooldownMs:               5000,
		MinConsensusQuality:      "HIGH",
	}
}

// Engine detects liquidity inefficiency signals and emits trade intents.
type Engine struct {
	mu     sync.Mutex
	policy *Policy
	// Per venue+symbol rolling state.
	states    map[string]*venueState
	lastEmit  map[string]int64 // cooldown tracking: venue:symbol → last emit ms
}

type venueState struct {
	// Rolling spread baseline (circular buffer).
	spreadBuf []float64
	bufIdx    int
	bufFull   bool
	// Previous mid price (for cascade detection).
	prevMid      float64
	prevMidTsMs  int64
}

func (vs *venueState) spreadBaseline() float64 {
	if len(vs.spreadBuf) == 0 {
		return 0
	}
	var sum float64
	n := len(vs.spreadBuf)
	if !vs.bufFull {
		n = vs.bufIdx
	}
	if n == 0 {
		return 0
	}
	for i := 0; i < n; i++ {
		sum += vs.spreadBuf[i]
	}
	return sum / float64(n)
}

func (vs *venueState) pushSpread(spread float64, cap int) {
	if len(vs.spreadBuf) < cap {
		vs.spreadBuf = append(vs.spreadBuf, spread)
		vs.bufIdx = len(vs.spreadBuf)
		vs.bufFull = len(vs.spreadBuf) == cap
		return
	}
	vs.spreadBuf[vs.bufIdx%cap] = spread
	vs.bufIdx++
	vs.bufFull = true
}

func NewEngine(policy *Policy) *Engine {
	if policy == nil {
		policy = DefaultPolicy()
	}
	return &Engine{
		policy:   policy,
		states:   make(map[string]*venueState),
		lastEmit: make(map[string]int64),
	}
}

// Evaluate processes a new quote and returns any trade intents to emit.
// consensusQuality is the string label from the consensus engine ("HIGH"/"MED"/"LOW").
func (e *Engine) Evaluate(q consensus.Quote, consensusQuality string) []arb.TradeIntent {
	// Safety gate: consensus must be HIGH.
	if !qualityOK(consensusQuality, e.policy.MinConsensusQuality) {
		return nil
	}
	// Venue must have bid/ask.
	if q.BestBid <= 0 || q.BestAsk <= 0 {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	key := string(q.Venue) + ":" + string(q.Symbol)
	vs, ok := e.states[key]
	if !ok {
		vs = &venueState{
			spreadBuf:   make([]float64, 0, e.policy.SpreadBaselineSamples),
			prevMid:     (q.BestBid + q.BestAsk) / 2,
			prevMidTsMs: q.TsMs,
		}
		e.states[key] = vs
	}

	now := time.Now().UnixMilli()
	mid := (q.BestBid + q.BestAsk) / 2
	if mid <= 0 {
		return nil
	}
	spreadBps := (q.BestAsk - q.BestBid) / mid * 10000

	var signals []Signal

	// ── 1. Spread blowout ─────────────────────────────────────────────────
	baseline := vs.spreadBaseline()
	if baseline > 0 && spreadBps > e.policy.SpreadBlowoutMult*baseline {
		signals = append(signals, Signal{
			Type:     SignalSpreadBlowout,
			Venue:    string(q.Venue),
			Symbol:   string(q.Symbol),
			Value:    spreadBps,
			Baseline: baseline,
			TsMs:     now,
		})
	}
	vs.pushSpread(spreadBps, e.policy.SpreadBaselineSamples)

	// ── 2. Thin book ──────────────────────────────────────────────────────
	if q.BidDepth1Pct > 0 && q.BidDepth1Pct < e.policy.ThinBookThresholdUSD {
		signals = append(signals, Signal{
			Type:  SignalThinBook,
			Venue: string(q.Venue), Symbol: string(q.Symbol),
			Value: q.BidDepth1Pct, Baseline: e.policy.ThinBookThresholdUSD, TsMs: now,
		})
	}
	if q.AskDepth1Pct > 0 && q.AskDepth1Pct < e.policy.ThinBookThresholdUSD {
		signals = append(signals, Signal{
			Type:  SignalThinBook,
			Venue: string(q.Venue), Symbol: string(q.Symbol),
			Value: q.AskDepth1Pct, Baseline: e.policy.ThinBookThresholdUSD, TsMs: now,
		})
	}

	// ── 3. Mark/index divergence ──────────────────────────────────────────
	if q.Mark > 0 && q.Index > 0 {
		divBps := math.Abs(q.Mark-q.Index) / q.Index * 10000
		if divBps > e.policy.MarkIndexDivergeBps {
			signals = append(signals, Signal{
				Type:     SignalMarkIndexDivergence,
				Venue:    string(q.Venue), Symbol: string(q.Symbol),
				Value:    divBps, Baseline: e.policy.MarkIndexDivergeBps, TsMs: now,
			})
		}
	}

	// ── 4. Order imbalance ────────────────────────────────────────────────
	if q.BidDepth1Pct > 0 && q.AskDepth1Pct > 0 {
		ratio := q.BidDepth1Pct / q.AskDepth1Pct
		if ratio > e.policy.ImbalanceRatio || ratio < 1/e.policy.ImbalanceRatio {
			signals = append(signals, Signal{
				Type:     SignalOrderImbalance,
				Venue:    string(q.Venue), Symbol: string(q.Symbol),
				Value:    ratio, Baseline: e.policy.ImbalanceRatio, TsMs: now,
			})
		}
	}

	// ── 5. Liquidation cascade proxy ──────────────────────────────────────
	if vs.prevMid > 0 && vs.prevMidTsMs > 0 {
		dtMs := now - vs.prevMidTsMs
		if dtMs > 0 && dtMs <= 60000 { // within 1 minute
			priceMoveAbsBps := math.Abs(mid-vs.prevMid) / vs.prevMid * 10000
			if priceMoveAbsBps > e.policy.LiqCascadePriceBps &&
				spreadBps > baseline*1.5 &&
				(q.BidDepth1Pct < e.policy.ThinBookThresholdUSD || q.AskDepth1Pct < e.policy.ThinBookThresholdUSD) {
				signals = append(signals, Signal{
					Type:     SignalLiquidationCascade,
					Venue:    string(q.Venue), Symbol: string(q.Symbol),
					Value:    priceMoveAbsBps, Baseline: e.policy.LiqCascadePriceBps, TsMs: now,
				})
			}
		}
	}
	vs.prevMid = mid
	vs.prevMidTsMs = now

	if len(signals) == 0 {
		return nil
	}

	// Enforce cooldown per venue+symbol.
	if last := e.lastEmit[key]; now-last < e.policy.CooldownMs {
		return nil
	}

	// Determine strategy and notional from signal mix.
	var strategy string
	notional := e.policy.SpreadCaptureNotionalUSD
	for _, sig := range signals {
		if sig.Type == SignalLiquidationCascade {
			strategy = "LIQUIDATION_CONTRA"
			notional = e.policy.LiqContraNotionalUSD
			break
		}
		strategy = "SPREAD_CAPTURE"
	}
	if strategy == "" {
		return nil
	}

	e.lastEmit[key] = now
	log.Printf("liquidity-engine: signal=%s venue=%s symbol=%s",
		signals[0].Type, q.Venue, q.Symbol)

	// For SPREAD_CAPTURE: go long (buy at bid) on this venue if spread is wide.
	// The execution router will simulate a fill at mid ± slippage.
	action := "BUY"
	if q.Mark > 0 && q.Index > 0 && q.Mark < q.Index {
		action = "SELL" // mark below index → fade the move
	}

	intent := arb.TradeIntent{
		IntentID:  fmt.Sprintf("liq-%s-%d", key, now),
		TenantID:  string(q.TenantID),
		Strategy:  strategy,
		Symbol:    string(q.Symbol),
		TsMs:      now,
		ExpiresMs: now + 3000, // 3s TTL — these are fast signals
		Constraints: arb.IntentConstraints{
			MinQuality: "HIGH",
		},
		Expected: arb.ExpectedMetrics{
			EdgeBpsGross: spreadBps / 2,
			EdgeBpsNet:   spreadBps/2 - 5, // assume 5bps costs
		},
		Legs: []arb.TradeLeg{{
			Venue:          string(q.Venue),
			Action:         action,
			NotionalUSD:    notional,
			Market:         "PERP",
			MaxSlippageBps: spreadBps * 0.4, // allow 40% of the spread as slippage
		}},
	}
	return []arb.TradeIntent{intent}
}

func qualityOK(have, need string) bool {
	rank := map[string]int{"LOW": 1, "MED": 2, "HIGH": 3}
	return rank[have] >= rank[need]
}
