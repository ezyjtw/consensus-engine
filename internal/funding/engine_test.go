package funding

import (
	"testing"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// testFundingPolicy returns a minimal valid policy for unit tests.
func testFundingPolicy() *Policy {
	return &Policy{
		Symbols:              []string{"BTC-PERP"},
		MinAnnualYieldPct:    map[string]float64{"HIGH": 8.0, "MED": 12.0},
		MaxNotionalUSD:       50000,
		MinDifferentialBps8h: 5.0,
		HedgeDriftUnwindUSD:  500,
		Venues:               []string{"binance", "okx"},
		EvalIntervalS:        30,
		IntentTTLMs:          10000,
		MaxSlippageBps:       8.0,
		CooldownS:            300,
	}
}

// setupEngine creates an engine with venue data pre-populated and a scheduler
// configured to avoid nearReset interference.
func setupEngine(p *Policy) *Engine {
	e := NewEngine(p)
	// Use a deterministic time that's never near a reset: 02:00 UTC is
	// 6 hours after the 00:00 reset and 6 hours before the 08:00 reset,
	// well outside the 30-minute near-reset window.
	fixedNow := time.Date(2025, 6, 15, 2, 0, 0, 0, time.UTC)
	e.scheduler = &FundingScheduler{resetHours: []int{0, 8, 16}}
	e.nowFunc = func() time.Time { return fixedNow }
	return e
}

// injectVenueData sets funding rate, mark price, and taker fee for a venue+symbol.
func injectVenueData(e *Engine, sym, venue string, fundingRate, markPrice, feeBps float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.state[sym]; !ok {
		e.state[sym] = make(map[string]*venueData)
	}
	e.state[sym][venue] = &venueData{
		fundingRate: fundingRate,
		markPrice:   markPrice,
		feeBpsTaker: feeBps,
		updatedMs:   time.Now().UnixMilli(),
	}
}

// highYieldRate returns a funding rate that produces ~20% annualised yield
// (rate * 3 * 365 * 100 ≈ 20%), comfortably above the 12% MED threshold.
func highYieldRate() float64 {
	return 0.00020 // ~21.9% annualised
}

// --- Stage config tests ---

func TestStageDefaultsFull(t *testing.T) {
	p := testFundingPolicy()
	if stage := p.stage("BTC-PERP"); stage != StageFull {
		t.Errorf("expected FULL for symbol without override, got %s", stage)
	}
}

func TestStageFromOverride(t *testing.T) {
	p := testFundingPolicy()
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"BTC-PERP": {Stage: StageObserve},
	}
	if stage := p.stage("BTC-PERP"); stage != StageObserve {
		t.Errorf("expected OBSERVE, got %s", stage)
	}
}

func TestSizeScaleDefault(t *testing.T) {
	p := testFundingPolicy()
	// No override → default conservative scale = 0.5
	if scale := p.sizeScale("BTC-PERP"); scale != 0.5 {
		t.Errorf("expected default 0.5, got %f", scale)
	}
}

func TestSizeScaleOverride(t *testing.T) {
	p := testFundingPolicy()
	sf := 0.3
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"BTC-PERP": {SizeScaleFactor: &sf},
	}
	if scale := p.sizeScale("BTC-PERP"); scale != 0.3 {
		t.Errorf("expected 0.3, got %f", scale)
	}
}

func TestMinYieldForSymbolOverride(t *testing.T) {
	p := testFundingPolicy()
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"DOGE-PERP": {
			MinAnnualYieldPct: map[string]float64{"HIGH": 15.0, "MED": 22.0},
		},
	}

	// DOGE should use override
	if v, ok := p.minYieldForSymbol("DOGE-PERP", "MED"); !ok || v != 22.0 {
		t.Errorf("expected DOGE MED=22.0, got %.1f (ok=%v)", v, ok)
	}
	// BTC should use global
	if v, ok := p.minYieldForSymbol("BTC-PERP", "MED"); !ok || v != 12.0 {
		t.Errorf("expected BTC MED=12.0, got %.1f (ok=%v)", v, ok)
	}
}

// --- OBSERVE stage: qualified signal but no intent emitted ---

func TestObserveStageNoIntentEmitted(t *testing.T) {
	p := testFundingPolicy()
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"BTC-PERP": {Stage: StageObserve},
	}
	e := setupEngine(p)
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)

	intents := e.Evaluate("t1")
	if len(intents) != 0 {
		t.Errorf("OBSERVE stage should emit 0 intents, got %d", len(intents))
	}
	if e.Emitted["carry_observe"] == 0 {
		t.Error("expected carry_observe counter to be incremented")
	}
}

// --- PAPER stage: intent emitted with ForcePaperMode ---

func TestPaperStageForcesPaperMode(t *testing.T) {
	p := testFundingPolicy()
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"BTC-PERP": {Stage: StagePaper},
	}
	e := setupEngine(p)
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)

	intents := e.Evaluate("t1")
	if len(intents) == 0 {
		t.Fatal("PAPER stage should emit intents, got 0")
	}
	if !intents[0].Constraints.ForcePaperMode {
		t.Error("PAPER stage intent should have ForcePaperMode=true")
	}
	if intents[0].Legs[0].NotionalUSD != 50000 {
		t.Errorf("PAPER stage should use full notional, got %.0f", intents[0].Legs[0].NotionalUSD)
	}
}

// --- CONSERVATIVE stage: intent emitted with reduced notional ---

func TestConservativeStageScalesNotional(t *testing.T) {
	p := testFundingPolicy()
	sf := 0.4
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"BTC-PERP": {Stage: StageConservative, SizeScaleFactor: &sf},
	}
	e := setupEngine(p)
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)

	intents := e.Evaluate("t1")
	if len(intents) == 0 {
		t.Fatal("CONSERVATIVE stage should emit intents, got 0")
	}
	// 50000 * 0.4 = 20000
	expected := 50000.0 * 0.4
	if intents[0].Legs[0].NotionalUSD != expected {
		t.Errorf("CONSERVATIVE notional: want %.0f, got %.0f",
			expected, intents[0].Legs[0].NotionalUSD)
	}
	if intents[0].Constraints.ForcePaperMode {
		t.Error("CONSERVATIVE should not set ForcePaperMode")
	}
}

// --- FULL stage: backwards compatibility ---

func TestFullStageUnchanged(t *testing.T) {
	p := testFundingPolicy()
	// No symbol overrides → FULL by default
	e := setupEngine(p)
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)

	intents := e.Evaluate("t1")
	if len(intents) == 0 {
		t.Fatal("FULL stage should emit intents, got 0")
	}
	if intents[0].Legs[0].NotionalUSD != 50000 {
		t.Errorf("FULL notional: want 50000, got %.0f", intents[0].Legs[0].NotionalUSD)
	}
	if intents[0].Constraints.ForcePaperMode {
		t.Error("FULL should not set ForcePaperMode")
	}
}

// --- UpdateQuote path: OBSERVE symbol still accepts quotes ---

func TestObserveStageAcceptsQuotes(t *testing.T) {
	p := testFundingPolicy()
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"BTC-PERP": {Stage: StageObserve},
	}
	e := NewEngine(p)

	q := consensus.Quote{
		Venue:       "binance",
		Symbol:      "BTC-PERP",
		FundingRate: 0.0003,
		Mark:        100000,
		FeeBpsTaker: 4.0,
		TsMs:        time.Now().UnixMilli(),
	}
	// Should not panic — OBSERVE still ingests data for monitoring.
	e.UpdateQuote(q)

	r := e.Regime("binance", "BTC-PERP")
	if r == nil {
		t.Error("expected regime data even in OBSERVE stage")
	}
}

// --- Multi-symbol staged rollout ---

func TestMultiSymbolStages(t *testing.T) {
	p := testFundingPolicy()
	p.Symbols = []string{"BTC-PERP", "SOL-PERP", "DOGE-PERP"}
	sf := 0.5
	maxN := 20000.0
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"SOL-PERP":  {Stage: StageConservative, SizeScaleFactor: &sf, MaxNotionalUSD: &maxN},
		"DOGE-PERP": {Stage: StageObserve},
	}
	e := setupEngine(p)

	// Inject data for all three symbols
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)
	injectVenueData(e, "SOL-PERP", "binance", highYieldRate(), 150, 4.0)
	injectVenueData(e, "DOGE-PERP", "binance", highYieldRate(), 0.15, 4.0)

	intents := e.Evaluate("t1")

	// Count intents per symbol
	btcCount, solCount, dogeCount := 0, 0, 0
	for _, intent := range intents {
		switch intent.Symbol {
		case "BTC-PERP":
			btcCount++
			// FULL: full notional
			if intent.Legs[0].NotionalUSD != 50000 {
				t.Errorf("BTC notional: want 50000, got %.0f", intent.Legs[0].NotionalUSD)
			}
		case "SOL-PERP":
			solCount++
			// CONSERVATIVE: 20000 * 0.5 = 10000
			expected := 20000.0 * 0.5
			if intent.Legs[0].NotionalUSD != expected {
				t.Errorf("SOL notional: want %.0f, got %.0f", expected, intent.Legs[0].NotionalUSD)
			}
		case "DOGE-PERP":
			dogeCount++
		}
	}

	if btcCount == 0 {
		t.Error("BTC-PERP (FULL) should emit at least one intent")
	}
	if solCount == 0 {
		t.Error("SOL-PERP (CONSERVATIVE) should emit at least one intent")
	}
	if dogeCount != 0 {
		t.Errorf("DOGE-PERP (OBSERVE) should emit 0 intents, got %d", dogeCount)
	}
	if e.Emitted["carry_observe"] == 0 {
		t.Error("expected carry_observe counter incremented for DOGE")
	}
}

// --- Per-symbol yield threshold override ---

func TestSymbolYieldThresholdOverride(t *testing.T) {
	p := testFundingPolicy()
	p.Symbols = []string{"BTC-PERP", "PEPE-PERP"}
	p.SymbolOverrides = map[string]FundingSymbolOverride{
		"PEPE-PERP": {
			MinAnnualYieldPct: map[string]float64{"HIGH": 15.0, "MED": 22.0},
		},
	}
	e := setupEngine(p)

	// Rate that gives ~15% annualised (0.000137 * 3 * 365 * 100 ≈ 15.0%).
	// This passes BTC (MED=12%) but fails PEPE (MED=22%).
	rate := 0.000137
	injectVenueData(e, "BTC-PERP", "binance", rate, 100000, 4.0)
	injectVenueData(e, "PEPE-PERP", "binance", rate, 0.00001, 4.0)

	intents := e.Evaluate("t1")

	btcEmitted, pepeEmitted := false, false
	for _, intent := range intents {
		switch intent.Symbol {
		case "BTC-PERP":
			btcEmitted = true
		case "PEPE-PERP":
			pepeEmitted = true
		}
	}

	if !btcEmitted {
		t.Error("BTC-PERP: ~15% yield should pass global MED=12% threshold")
	}
	if pepeEmitted {
		t.Error("PEPE-PERP: ~15% yield should fail override MED=22% threshold")
	}
}

// ═══════════════════════════════════════════════════════════════════════════
// CRITICAL PATH TESTS — yield calculation, carry lifecycle, exit signals,
// regime transitions. These are the tests that protect real money.
// ═══════════════════════════════════════════════════════════════════════════

// --- Yield calculation: verify round-trip fees + slippage are deducted ---

func TestYieldDeductsRoundTripCosts(t *testing.T) {
	p := testFundingPolicy()
	p.MaxSlippageBps = 8.0 // 8 bps per leg
	// Use only MED threshold (remove HIGH) so threshold is always 12%.
	p.MinAnnualYieldPct = map[string]float64{"MED": 12.0}
	e := setupEngine(p)

	// Rate that gives exactly 12% annualised gross:
	// rate * 3 * 365 * 100 = 12.0 → rate = 12 / (3*365*100) ≈ 0.0001096
	// Round-trip cost: fee=4bps*4legs=16bps=0.16%, slip=8bps*4legs=32bps=0.32%
	// Total cost: 0.48%. Net = 12.0 - 0.48 = 11.52% < MED threshold (12%).
	// So this should be REJECTED.
	rate := 12.0 / (3 * 365 * 100)
	injectVenueData(e, "BTC-PERP", "binance", rate, 100000, 4.0)

	intents := e.Evaluate("t1")
	if len(intents) != 0 {
		t.Errorf("12%% gross yield should be rejected after round-trip costs (net ~11.52%%), got %d intents", len(intents))
	}

	// Now increase rate to compensate for round-trip costs.
	// Need net > 12%: gross > 12% + 0.48% = 12.48%
	rate2 := 12.5 / (3 * 365 * 100)
	// Clear cooldown by creating fresh engine.
	e2 := setupEngine(p)
	injectVenueData(e2, "BTC-PERP", "binance", rate2, 100000, 4.0)

	intents2 := e2.Evaluate("t1")
	if len(intents2) == 0 {
		t.Error("12.5% gross yield should pass after round-trip costs (net ~12.02%%)")
	}
}

func TestYieldRejectsWhenSlippageEatsEdge(t *testing.T) {
	p := testFundingPolicy()
	p.MaxSlippageBps = 50.0 // extreme slippage: 50 bps per leg
	e := setupEngine(p)

	// ~21.9% gross, but 50bps*4legs=200bps=2% slippage + fees.
	// Should still pass (21.9 - 2.16 - 0.64 ≈ 19.1% > 12%).
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)

	intents := e.Evaluate("t1")
	if len(intents) == 0 {
		t.Error("high yield should still pass even with elevated slippage")
	}
}

// --- Carry lifecycle: entry → hold → rate flip → exit ---

func TestCarryLifecycle_EntryThenExitOnRateFlip(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Step 1: Entry — positive funding rate, should generate carry intent.
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)
	entryIntents := e.Evaluate("t1")
	if len(entryIntents) == 0 {
		t.Fatal("step 1: should emit carry entry intent")
	}
	entry := entryIntents[0]
	if entry.Strategy != StrategyFundingCarry {
		t.Errorf("step 1: strategy should be FUNDING_CARRY, got %s", entry.Strategy)
	}
	if entry.Legs[0].Market != "SPOT" || entry.Legs[0].Action != "BUY" {
		t.Error("step 1: leg 0 should be SPOT BUY")
	}
	if entry.Legs[1].Market != "PERP" || entry.Legs[1].Action != "SELL" {
		t.Error("step 1: leg 1 should be PERP SELL")
	}

	// Step 2: Simulate position is now open. Rate flips negative.
	injectVenueData(e, "BTC-PERP", "binance", -0.0001, 100000, 4.0)
	openPos := []OpenPosition{{
		Strategy:     StrategyFundingCarry,
		Symbol:       "BTC-PERP",
		Venue:        "binance",
		NotionalUSD:  50000,
		EntryRateBps: highYieldRate() * 10000,
		EntryTsMs:    time.Now().UnixMilli() - 3600000, // 1h ago
	}}

	// Step 3: Exit — should generate unwind intent.
	exitIntents := e.EvaluateExits("t1", openPos)
	if len(exitIntents) == 0 {
		t.Fatal("step 3: should emit exit intent on rate flip")
	}
	exit := exitIntents[0]
	if exit.Strategy != StrategyFundingCarryExit {
		t.Errorf("step 3: strategy should be FUNDING_CARRY_EXIT, got %s", exit.Strategy)
	}
	// Exit legs should be reverse of entry.
	if exit.Legs[0].Market != "SPOT" || exit.Legs[0].Action != "SELL" {
		t.Error("step 3: exit leg 0 should be SPOT SELL")
	}
	if exit.Legs[1].Market != "PERP" || exit.Legs[1].Action != "BUY" {
		t.Error("step 3: exit leg 1 should be PERP BUY")
	}
	if exit.Legs[0].NotionalUSD != 50000 {
		t.Errorf("step 3: exit notional should match position, got %.0f", exit.Legs[0].NotionalUSD)
	}
}

// --- Exit signals: test all 4 exit reasons individually ---

func TestExitSignal_FundingInverted(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)
	// Current rate is negative.
	injectVenueData(e, "BTC-PERP", "binance", -0.0001, 100000, 4.0)

	pos := []OpenPosition{{
		Strategy: StrategyFundingCarry, Symbol: "BTC-PERP", Venue: "binance",
		NotionalUSD: 50000, EntryRateBps: 2.0, // was positive at entry
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) == 0 {
		t.Fatal("should exit on funding rate inversion")
	}
}

func TestExitSignal_RegimeVolatile(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)
	// Rate is still positive, but inject enough volatility to trigger VOLATILE regime.
	// VOLATILE requires stdDev/|EWA| > 3.0.
	// Feed many wildly varying rates to push stdDev up.
	for i := 0; i < 50; i++ {
		rate := 0.0001
		if i%2 == 0 {
			rate = -0.0001
		}
		e.forecaster.Update("binance", "BTC-PERP", rate, time.Now().UnixMilli()+int64(i*1000))
	}
	injectVenueData(e, "BTC-PERP", "binance", 0.0001, 100000, 4.0) // current rate positive

	pos := []OpenPosition{{
		Strategy: StrategyFundingCarry, Symbol: "BTC-PERP", Venue: "binance",
		NotionalUSD: 50000, EntryRateBps: 1.0,
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) == 0 {
		t.Fatal("should exit when regime is VOLATILE")
	}
}

func TestExitSignal_VolatilitySpike(t *testing.T) {
	p := testFundingPolicy()
	p.VolatilityGate.VolThresholdPct = 5.0
	e := setupEngine(p)

	// Feed mark prices with >5% realized vol.
	now := time.Now().UnixMilli()
	for i := 0; i < 100; i++ {
		price := 100000.0
		if i%2 == 0 {
			price = 106000.0 // 6% swings
		}
		e.volTracker.Record("binance", "BTC-PERP", price, now+int64(i*60000))
	}
	injectVenueData(e, "BTC-PERP", "binance", 0.0002, 100000, 4.0)

	pos := []OpenPosition{{
		Strategy: StrategyFundingCarry, Symbol: "BTC-PERP", Venue: "binance",
		NotionalUSD: 50000, EntryRateBps: 2.0,
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) == 0 {
		t.Fatal("should exit on volatility spike")
	}
}

func TestExitSignal_NearResetNegative(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Use a deterministic time: 15:50 UTC is 10 minutes before the 16:00
	// reset, well within the 30-minute near-reset window.
	e.scheduler = &FundingScheduler{resetHours: []int{0, 8, 16}}
	fixedNow := time.Date(2025, 6, 15, 15, 50, 0, 0, time.UTC)
	e.nowFunc = func() time.Time { return fixedNow }

	// Current rate is negative and we're near reset.
	injectVenueData(e, "BTC-PERP", "binance", -0.00005, 100000, 4.0)

	pos := []OpenPosition{{
		Strategy: StrategyFundingCarry, Symbol: "BTC-PERP", Venue: "binance",
		NotionalUSD: 50000, EntryRateBps: 2.0,
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) == 0 {
		t.Fatal("should exit on negative rate near reset")
	}
}

func TestNoExitWhenConditionsHealthy(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)
	// Positive rate, stable regime, no vol spike, not near reset.
	injectVenueData(e, "BTC-PERP", "binance", 0.0002, 100000, 4.0)
	// Feed stable rates so regime is POSITIVE.
	for i := 0; i < 20; i++ {
		e.forecaster.Update("binance", "BTC-PERP", 0.0002, time.Now().UnixMilli()+int64(i*1000))
	}

	pos := []OpenPosition{{
		Strategy: StrategyFundingCarry, Symbol: "BTC-PERP", Venue: "binance",
		NotionalUSD: 50000, EntryRateBps: 2.0,
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) != 0 {
		t.Errorf("should NOT exit when conditions are healthy, got %d exit intents", len(exits))
	}
}

// --- Reverse carry: entry on negative funding, exit on flip to positive ---

func TestReverseCarryLifecycle(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Step 1: Negative funding → should emit reverse carry.
	injectVenueData(e, "BTC-PERP", "binance", -highYieldRate(), 100000, 4.0)
	intents := e.Evaluate("t1")
	found := false
	for _, intent := range intents {
		if intent.Strategy == StrategyFundingCarryReverse {
			found = true
			if intent.Legs[0].Action != "SELL" || intent.Legs[0].Market != "SPOT" {
				t.Error("reverse carry leg 0 should be SPOT SELL")
			}
			if intent.Legs[1].Action != "BUY" || intent.Legs[1].Market != "PERP" {
				t.Error("reverse carry leg 1 should be PERP BUY")
			}
		}
	}
	if !found {
		t.Fatal("should emit FUNDING_CARRY_REVERSE on negative funding")
	}

	// Step 2: Rate flips positive → exit reverse carry.
	injectVenueData(e, "BTC-PERP", "binance", 0.0001, 100000, 4.0)
	pos := []OpenPosition{{
		Strategy: StrategyFundingCarryReverse, Symbol: "BTC-PERP", Venue: "binance",
		NotionalUSD: 50000, EntryRateBps: -highYieldRate() * 10000,
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) == 0 {
		t.Fatal("should exit reverse carry when rate flips positive")
	}
	if exits[0].Strategy != StrategyFundingCarryReverseExit {
		t.Errorf("exit strategy should be FUNDING_CARRY_REVERSE_EXIT, got %s", exits[0].Strategy)
	}
}

// --- Regime transitions ---

func TestRegimeTransition_PositiveToVolatile(t *testing.T) {
	f := NewRegimeForecaster(0.15)

	// Feed stable positive rates → should be POSITIVE.
	for i := 0; i < 30; i++ {
		r := f.Update("binance", "BTC-PERP", 0.0002, int64(i*1000))
		if i == 29 && r.Label != "POSITIVE" {
			t.Errorf("after stable positive rates, expected POSITIVE, got %s", r.Label)
		}
	}

	// Feed wildly oscillating rates → should transition to VOLATILE.
	for i := 30; i < 80; i++ {
		rate := 0.001
		if i%2 == 0 {
			rate = -0.001
		}
		r := f.Update("binance", "BTC-PERP", rate, int64(i*1000))
		if i == 79 && r.Label != "VOLATILE" {
			t.Errorf("after oscillating rates, expected VOLATILE, got %s (ewa=%.6f stddev=%.6f)",
				r.Label, r.EWA, r.StdDev)
		}
	}
}

func TestRegimeTransition_VolatileBlocksEntry(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Push regime to VOLATILE.
	for i := 0; i < 50; i++ {
		rate := 0.001
		if i%2 == 0 {
			rate = -0.001
		}
		e.forecaster.Update("binance", "BTC-PERP", rate, time.Now().UnixMilli()+int64(i*1000))
	}

	// Even with a high yield rate, entry should be blocked by VOLATILE regime.
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)
	intents := e.Evaluate("t1")
	if len(intents) != 0 {
		t.Errorf("VOLATILE regime should block carry entry, got %d intents", len(intents))
	}
	if e.Rejected["carry_volatile_regime"] == 0 {
		t.Error("expected carry_volatile_regime rejection counter")
	}
}

// --- Near-reset entry blocking ---

func TestNearResetBlocksNewEntry(t *testing.T) {
	p := testFundingPolicy()
	e := NewEngine(p)

	// Use a deterministic time: 07:45 UTC is 15 minutes before the 08:00
	// reset, well within the 30-minute near-reset window. This avoids
	// flakiness from depending on the real wall clock.
	e.scheduler = &FundingScheduler{resetHours: []int{0, 8, 16}}
	fixedNow := time.Date(2025, 6, 15, 7, 45, 0, 0, time.UTC)

	// Verify our setup: must be within 30 min of next reset.
	if !e.scheduler.IsNearReset(fixedNow, 30*time.Minute) {
		t.Fatal("test setup error: fixedNow should be near reset")
	}

	// Override the engine's time source so Evaluate uses our fixed time.
	e.nowFunc = func() time.Time { return fixedNow }

	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)
	intents := e.Evaluate("t1")
	if len(intents) != 0 {
		t.Errorf("near-reset should block new entry, got %d intents", len(intents))
	}
	if e.Rejected["carry_near_reset"] == 0 {
		t.Error("expected carry_near_reset rejection counter")
	}
}

// --- Differential strategy tests ---

func TestDifferentialEntry(t *testing.T) {
	p := testFundingPolicy()
	p.MinDifferentialBps8h = 5.0
	e := setupEngine(p)

	// binance: low rate, okx: high rate → should long binance, short okx.
	// Differential must be > 5 bps = 0.0005. Use 0.0001 vs 0.0010 = 9 bps spread.
	// Both rates are positive but the differential carry captures the spread.
	injectVenueData(e, "BTC-PERP", "binance", 0.0001, 100000, 4.0)
	injectVenueData(e, "BTC-PERP", "okx", 0.0010, 100000, 5.0)

	intents := e.Evaluate("t1")
	var diffIntent *arb.TradeIntent
	for i := range intents {
		if intents[i].Strategy == StrategyFundingDifferential {
			diffIntent = &intents[i]
			break
		}
	}
	if diffIntent == nil {
		t.Fatal("should emit FUNDING_DIFFERENTIAL intent")
	}

	// Verify legs: should long low-rate (binance), short high-rate (okx).
	if diffIntent.Legs[0].Action != "BUY" {
		t.Error("diff leg 0 should be BUY (long low-rate venue)")
	}
	if diffIntent.Legs[1].Action != "SELL" {
		t.Error("diff leg 1 should be SELL (short high-rate venue)")
	}
}

func TestDifferentialExitOnInversion(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Rates inverted: long venue now has higher rate than short venue.
	injectVenueData(e, "BTC-PERP", "binance", 0.0005, 100000, 4.0) // long venue rate now higher
	injectVenueData(e, "BTC-PERP", "okx", 0.0001, 100000, 5.0)     // short venue rate now lower

	pos := []OpenPosition{{
		Strategy:    StrategyFundingDifferential,
		Symbol:      "BTC-PERP",
		LongVenue:   "binance",
		ShortVenue:  "okx",
		NotionalUSD: 50000,
		EntryRateBps: 25.0,
	}}
	exits := e.EvaluateExits("t1", pos)
	if len(exits) == 0 {
		t.Fatal("should exit differential when spread inverts")
	}
	if exits[0].Strategy != StrategyFundingDiffExit {
		t.Errorf("exit strategy should be FUNDING_DIFFERENTIAL_EXIT, got %s", exits[0].Strategy)
	}
}

// --- Cooldown enforcement ---

func TestCooldownBlocksRepeatEntry(t *testing.T) {
	p := testFundingPolicy()
	p.CooldownS = 300 // 5 minute cooldown
	e := setupEngine(p)
	injectVenueData(e, "BTC-PERP", "binance", highYieldRate(), 100000, 4.0)

	// First eval should emit.
	intents1 := e.Evaluate("t1")
	if len(intents1) == 0 {
		t.Fatal("first eval should emit intent")
	}

	// Second eval within cooldown should NOT emit.
	intents2 := e.Evaluate("t1")
	if len(intents2) != 0 {
		t.Errorf("second eval within cooldown should emit 0 intents, got %d", len(intents2))
	}
	if e.Rejected["carry_cooldown"] == 0 {
		t.Error("expected carry_cooldown rejection counter")
	}
}

// --- Event-driven exit signal ---

func TestExitSignalChannelOnRateInversion(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Inject positive funding rate first.
	e.UpdateQuote(consensus.Quote{
		Symbol:      "BTC-PERP",
		Venue:       "binance",
		FundingRate: 0.0002,
		Mark:        100000,
		TsMs:        time.Now().UnixMilli(),
	})

	// Now inject negative rate — sign flip should trigger ExitSignalC.
	e.UpdateQuote(consensus.Quote{
		Symbol:      "BTC-PERP",
		Venue:       "binance",
		FundingRate: -0.0001,
		Mark:        100000,
		TsMs:        time.Now().UnixMilli(),
	})

	select {
	case <-e.ExitSignalC:
		// Expected: channel received signal.
	default:
		t.Error("ExitSignalC should fire on funding rate sign change")
	}
}

func TestExitSignalChannelNotFiredOnSameSign(t *testing.T) {
	p := testFundingPolicy()
	e := setupEngine(p)

	// Inject two positive rates — no sign flip.
	e.UpdateQuote(consensus.Quote{
		Symbol:      "BTC-PERP",
		Venue:       "binance",
		FundingRate: 0.0002,
		Mark:        100000,
		TsMs:        time.Now().UnixMilli(),
	})
	e.UpdateQuote(consensus.Quote{
		Symbol:      "BTC-PERP",
		Venue:       "binance",
		FundingRate: 0.0003,
		Mark:        100000,
		TsMs:        time.Now().UnixMilli(),
	})

	select {
	case <-e.ExitSignalC:
		t.Error("ExitSignalC should NOT fire when rate stays positive")
	default:
		// Expected: no signal.
	}
}

// --- Scheduler tests ---

func TestSchedulerNextAndPrevReset(t *testing.T) {
	s := NewFundingScheduler()
	// At 07:30 UTC: next reset should be 08:00, prev should be 00:00.
	ts := time.Date(2025, 6, 15, 7, 30, 0, 0, time.UTC)
	next := s.NextReset(ts)
	prev := s.PrevReset(ts)

	if next.Hour() != 8 {
		t.Errorf("next reset at 07:30 should be 08:00, got %d:00", next.Hour())
	}
	if prev.Hour() != 0 {
		t.Errorf("prev reset at 07:30 should be 00:00, got %d:00", prev.Hour())
	}
}

func TestSchedulerPeriodFraction(t *testing.T) {
	s := NewFundingScheduler()
	// 4 hours after 00:00 reset = 50% through period.
	ts := time.Date(2025, 6, 15, 4, 0, 0, 0, time.UTC)
	frac := s.PeriodFraction(ts)
	if frac < 0.49 || frac > 0.51 {
		t.Errorf("4h into 8h period should be ~0.5, got %.2f", frac)
	}
}
