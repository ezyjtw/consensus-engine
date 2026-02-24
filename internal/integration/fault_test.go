// Package integration provides fault injection tests to verify the pipeline
// handles failure modes safely and deterministically.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/execution"
	"github.com/ezyjtw/consensus-engine/internal/risk"
)

// ── Fault: stale quotes ─────────────────────────────────────────────────────

// TestFaultAllQuotesStale verifies that the consensus engine produces a
// LOW quality result when ALL quotes are stale and StalePauseMs is configured.
func TestFaultAllQuotesStale(t *testing.T) {
	p := testConsensusPolicy()
	p.StalePauseMs = 750 // enable the all-stale pause guard
	ce := consensus.NewEngine(p)
	sym := consensus.Symbol("BTC-PERP")
	now := time.Now().UnixMilli()

	// All quotes are 2 seconds stale (threshold is 750ms).
	quotes := map[consensus.Venue]consensus.Quote{
		"binance": makeReplayQuote("binance", sym, 100000, 10, now-2000),
		"okx":     makeReplayQuote("okx", sym, 100000, 10, now-2000),
		"bybit":   makeReplayQuote("bybit", sym, 100000, 10, now-2000),
		"deribit": makeReplayQuote("deribit", sym, 100000, 10, now-2000),
	}

	statusFn := func(v consensus.Venue) consensus.VenueStatus {
		return consensus.VenueStatus{State: consensus.StateOK}
	}

	result := ce.Compute("test", sym, quotes, statusFn)

	// With all stale quotes and StalePauseMs configured, quality must be LOW.
	if result.Update.Consensus.Quality != "LOW" {
		t.Errorf("all-stale quotes should produce LOW quality, got %s", result.Update.Consensus.Quality)
	}
	// Mid should be zero (empty consensus returned by stale-pause guard).
	if result.Update.Consensus.Mid != 0 {
		t.Errorf("all-stale consensus should have zero mid, got %.2f", result.Update.Consensus.Mid)
	}
}

// TestFaultSingleFreshQuote verifies the engine handles a mix of fresh and stale venues.
// When not all quotes are stale, the engine includes all venues but penalises stale
// ones via trust weight. The fresh venue should carry the most trust.
func TestFaultSingleFreshQuote(t *testing.T) {
	p := testConsensusPolicy()
	p.StalePauseMs = 750 // enable the all-stale pause guard
	ce := consensus.NewEngine(p)
	sym := consensus.Symbol("BTC-PERP")
	now := time.Now().UnixMilli()

	// Only binance is fresh; others are stale.
	quotes := map[consensus.Venue]consensus.Quote{
		"binance": makeReplayQuote("binance", sym, 100000, 10, now),
		"okx":     makeReplayQuote("okx", sym, 100000, 10, now-2000),
		"bybit":   makeReplayQuote("bybit", sym, 100000, 10, now-2000),
		"deribit": makeReplayQuote("deribit", sym, 100000, 10, now-2000),
	}

	statusFn := func(v consensus.Venue) consensus.VenueStatus {
		return consensus.VenueStatus{State: consensus.StateOK}
	}

	result := ce.Compute("test", sym, quotes, statusFn)

	// Not all quotes are stale, so the engine computes a full consensus.
	if result.Update.Consensus.Mid == 0 {
		t.Error("should produce a consensus mid when at least one venue is fresh")
	}

	// Fresh venue (binance) should have the highest trust weight.
	var binanceTrust float64
	for _, vm := range result.Update.Venues {
		if vm.Venue == "binance" {
			binanceTrust = vm.Trust
		}
	}
	if binanceTrust <= 0 {
		t.Error("binance (fresh) should have positive trust weight")
	}
	// Binance should have the highest trust because it's the only fresh venue.
	for _, vm := range result.Update.Venues {
		if vm.Venue != "binance" && vm.Trust > binanceTrust {
			t.Errorf("stale venue %s has higher trust (%.4f) than fresh binance (%.4f)",
				vm.Venue, vm.Trust, binanceTrust)
		}
	}
}

// ── Fault: venue blacklist propagation ──────────────────────────────────────

// TestFaultBlacklistedVenueNotUsedInConsensus verifies that a venue in
// BLACKLISTED state has reduced trust weight in consensus computation.
func TestFaultBlacklistedVenueNotUsedInConsensus(t *testing.T) {
	ce := consensus.NewEngine(testConsensusPolicy())
	sym := consensus.Symbol("BTC-PERP")
	now := time.Now().UnixMilli()

	quotes := map[consensus.Venue]consensus.Quote{
		"binance": makeReplayQuote("binance", sym, 100000, 10, now),
		"okx":     makeReplayQuote("okx", sym, 100000, 10, now),
		"bybit":   makeReplayQuote("bybit", sym, 100000, 10, now),
		"deribit": makeReplayQuote("deribit", sym, 99500, 10, now), // 50 bps off
	}

	// Deribit is already blacklisted.
	statusFn := func(v consensus.Venue) consensus.VenueStatus {
		if v == "deribit" {
			return consensus.VenueStatus{State: consensus.StateBlacklisted}
		}
		return consensus.VenueStatus{State: consensus.StateOK}
	}

	result := ce.Compute("test", sym, quotes, statusFn)

	// Consensus mid should be close to 100000 (deribit's off-price should be penalized).
	if result.Update.Consensus.Mid < 99900 {
		t.Errorf("consensus mid should not be pulled down by blacklisted deribit: %.2f",
			result.Update.Consensus.Mid)
	}
}

// ── Fault: execution errors ─────────────────────────────────────────────────

// TestFaultExpiredIntent verifies the paper executor correctly rejects expired intents.
func TestFaultExpiredIntent(t *testing.T) {
	cfg := &execution.Config{
		TradingMode:        "PAPER",
		SimSlippageBps:     4.0,
		SimLatencyMs:       50,
		MaxOrdersPerMinute: 1000,
		TenantID:           "test",
	}

	cache := execution.NewPriceCache()
	cache.Update(consensus.ConsensusUpdate{
		Symbol: "BTC-PERP",
		Consensus: consensus.Consensus{
			Mid:     100000,
			Quality: "HIGH",
		},
	})

	pe := execution.NewPaperExecutor(cfg, cache)

	// Intent already expired.
	intent := arb.TradeIntent{
		TenantID:  "test",
		IntentID:  "expired-1",
		Strategy:  "CROSS_VENUE_ARB",
		Symbol:    "BTC-PERP",
		TsMs:      time.Now().UnixMilli() - 5000,
		ExpiresMs: time.Now().UnixMilli() - 1000,
		Legs: []arb.TradeLeg{
			{Venue: "binance", Action: "BUY", NotionalUSD: 10000, MaxSlippageBps: 8},
		},
	}

	_, fill := pe.Execute(context.Background(), intent)
	if fill == nil {
		t.Fatal("expected expired fill, got nil")
	}
	if !fill.IntentExpired {
		t.Error("fill should be marked as expired")
	}
}

// TestFaultNoConsensusPrice verifies the executor handles missing prices gracefully.
func TestFaultNoConsensusPrice(t *testing.T) {
	cfg := &execution.Config{
		TradingMode:        "PAPER",
		SimSlippageBps:     4.0,
		SimLatencyMs:       50,
		MaxOrdersPerMinute: 1000,
		TenantID:           "test",
	}

	cache := execution.NewPriceCache()
	// Deliberately do NOT populate price cache for ETH-PERP.

	pe := execution.NewPaperExecutor(cfg, cache)

	intent := arb.TradeIntent{
		TenantID:  "test",
		IntentID:  "no-price-1",
		Strategy:  "CROSS_VENUE_ARB",
		Symbol:    "ETH-PERP", // no price cached
		TsMs:      time.Now().UnixMilli(),
		ExpiresMs: time.Now().UnixMilli() + 5000,
		Legs: []arb.TradeLeg{
			{Venue: "binance", Action: "BUY", NotionalUSD: 10000, MaxSlippageBps: 8},
		},
	}

	events, fill := pe.Execute(context.Background(), intent)
	if events != nil || fill != nil {
		t.Error("should return nil when no consensus price available")
	}
}

// ── Fault: risk daemon escalation ───────────────────────────────────────────

// TestFaultDrawdownEscalation verifies the risk daemon transitions modes on drawdown.
func TestFaultDrawdownEscalation(t *testing.T) {
	policy := &risk.Policy{
		MaxDrawdownPct:           3.0,
		SafeModeDrawdownPct:      5.0,
		MaxErrorRate5mPct:        10.0,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       300000,
	}

	d := risk.NewDaemon(policy)

	// Initial state should be RUNNING.
	if d.CurrentMode() != risk.ModeRunning {
		t.Fatalf("expected RUNNING, got %s", d.CurrentMode())
	}

	// Record losses to push drawdown past 3%.
	d.RecordFill(-3500, false) // -3.5% of starting 100k equity

	if d.CurrentMode() != risk.ModePaused {
		t.Errorf("expected PAUSED after drawdown, got %s", d.CurrentMode())
	}
}

// TestFaultErrorRateEscalation verifies error rate triggers pause.
func TestFaultErrorRateEscalation(t *testing.T) {
	policy := &risk.Policy{
		MaxDrawdownPct:           3.0,
		SafeModeDrawdownPct:      5.0,
		MaxErrorRate5mPct:        10.0,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       300000,
	}

	d := risk.NewDaemon(policy)

	// Record 5 events: 3 errors, 2 normal. Error rate = 60% > 10%.
	d.RecordFill(0, true)
	d.RecordFill(0, true)
	d.RecordFill(0, true)
	d.RecordFill(0, false)
	d.RecordFill(0, false)

	if d.CurrentMode() != risk.ModePaused {
		t.Errorf("expected PAUSED after high error rate, got %s", d.CurrentMode())
	}
}

// TestFaultADLRiskEscalation verifies ADL risk triggers pause.
func TestFaultADLRiskEscalation(t *testing.T) {
	policy := &risk.Policy{
		MaxDrawdownPct:           3.0,
		SafeModeDrawdownPct:      5.0,
		MaxErrorRate5mPct:        10.0,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       300000,
	}

	d := risk.NewDaemon(policy)
	d.RecordADLRisk("binance", 50) // 50% > 40% threshold

	if d.CurrentMode() != risk.ModePaused {
		t.Errorf("expected PAUSED after ADL risk escalation, got %s", d.CurrentMode())
	}
}

// TestFaultVenueDelevSafeMode verifies venue deleveraging triggers SAFE mode.
func TestFaultVenueDelevSafeMode(t *testing.T) {
	policy := &risk.Policy{
		MaxDrawdownPct:           3.0,
		SafeModeDrawdownPct:      5.0,
		MaxErrorRate5mPct:        10.0,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       300000,
	}

	d := risk.NewDaemon(policy)
	d.RecordVenueDelevEvent() // 1
	d.RecordVenueDelevEvent() // 2 — triggers SAFE mode

	mode := d.CurrentMode()
	if mode != risk.ModeSafe {
		t.Errorf("expected SAFE after 2 delev events, got %s", mode)
	}
}

// ── Fault: playbook activation ──────────────────────────────────────────────

// TestFaultPlaybookActivation verifies that incident playbooks are activated.
func TestFaultPlaybookActivation(t *testing.T) {
	policy := &risk.Policy{
		MaxDrawdownPct:           3.0,
		SafeModeDrawdownPct:      5.0,
		MaxErrorRate5mPct:        10.0,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       300000,
	}

	d := risk.NewDaemon(policy)
	d.RecordADLRisk("binance", 50)

	playbooks := d.ActivePlaybooks()
	found := false
	for _, pb := range playbooks {
		if pb.Name == risk.PlaybookADLEvent {
			found = true
		}
	}
	if !found {
		t.Error("expected ADL_EVENT playbook to be activated")
	}
}

// TestFaultPlaybookResolution verifies manual playbook resolution.
func TestFaultPlaybookResolution(t *testing.T) {
	policy := &risk.Policy{
		MaxDrawdownPct:           3.0,
		SafeModeDrawdownPct:      5.0,
		MaxErrorRate5mPct:        10.0,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       300000,
	}

	d := risk.NewDaemon(policy)
	d.RecordVenueDelevEvent()
	d.RecordVenueDelevEvent()

	// Should have VENUE_MAINTENANCE playbook.
	playbooks := d.ActivePlaybooks()
	if len(playbooks) == 0 {
		t.Fatal("expected at least one active playbook")
	}

	// Resolve it.
	d.ResolvePlaybook(risk.PlaybookVenueMaintenance)

	// Should be resolved.
	remaining := d.ActivePlaybooks()
	for _, pb := range remaining {
		if pb.Name == risk.PlaybookVenueMaintenance {
			t.Error("VENUE_MAINTENANCE should have been resolved")
		}
	}
}

// ── Fault: arb engine quality gating ────────────────────────────────────────

// TestFaultLowQualityBlocksArb verifies that LOW quality consensus blocks arb intents.
func TestFaultLowQualityBlocksArb(t *testing.T) {
	ae := arb.NewEngine(testArbPolicy())

	// LOW quality update with a big spread between venues — would normally arb.
	update := consensus.ConsensusUpdate{
		TenantID: "test",
		Symbol:   "BTC-PERP",
		TsMs:     time.Now().UnixMilli(),
		Consensus: consensus.Consensus{
			Mid:     100000,
			Quality: "LOW",
		},
		Venues: []consensus.VenueMetrics{
			{Venue: "binance", Mid: 100020, Status: consensus.StateOK},
			{Venue: "okx", Mid: 99980, Status: consensus.StateOK},
			{Venue: "bybit", Mid: 100000, Status: consensus.StateOK},
		},
	}

	intents := ae.Process(update)

	// With MinConsensusQuality "MED" and quality "LOW", no intents should be emitted.
	if len(intents) > 0 {
		t.Errorf("expected 0 intents for LOW quality consensus, got %d", len(intents))
	}
}
