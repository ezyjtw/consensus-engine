package funding

import (
	"testing"
	"time"

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
	// Set reset hours far from current time to avoid nearReset blocking intents.
	h := time.Now().UTC().Hour()
	safeHour := (h + 4) % 24
	e.scheduler = &FundingScheduler{resetHours: []int{safeHour}}
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
