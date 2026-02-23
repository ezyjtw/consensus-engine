package risk

import (
	"testing"
)

func testPolicy() *Policy {
	return &Policy{
		MaxNetDeltaUSD:           5000,
		MaxMarginUtilisationPct:  70,
		SafeModeMarginUtilPct:    85,
		MinLiqDistancePct:        20,
		MaxHedgeDriftUSDSec:      500,
		MaxDrawdownPct:           3,
		SafeModeDrawdownPct:      5,
		MaxErrorRate5mPct:        20,
		MinCoreVenuesForSafeMode: 2,
		TenantID:                 "test",
		ADLRiskPausePct:          40,
		LiqClusterPauseCount:     3,
		VenueDelevSafeModeCount:  2,
		VenueDelevWindowMs:       5 * 60 * 1000,
	}
}

// Test that a fresh daemon starts in RUNNING mode.
func TestNewDaemonStartsRunning(t *testing.T) {
	d := NewDaemon(testPolicy())
	if d.CurrentMode() != ModeRunning {
		t.Errorf("expected RUNNING, got %s", d.CurrentMode())
	}
}

// Test that a small positive fill keeps RUNNING mode.
func TestRecordFillPositivePnLStaysRunning(t *testing.T) {
	d := NewDaemon(testPolicy())
	alerts := d.RecordFill(100, false)
	if d.CurrentMode() != ModeRunning {
		t.Errorf("expected RUNNING after profitable fill, got %s", d.CurrentMode())
	}
	for _, a := range alerts {
		if a.Severity == "CRITICAL" {
			t.Errorf("unexpected CRITICAL alert after small profitable fill: %s", a.Source)
		}
	}
}

// Test equity tracking: peak and current update correctly.
func TestEquityTrackingPeakUpdate(t *testing.T) {
	d := NewDaemon(testPolicy())
	initial := d.CurrentState().PeakEquityUSD

	d.RecordFill(500, false) // equity goes up
	state := d.CurrentState()
	if state.PeakEquityUSD != initial+500 {
		t.Errorf("peak equity should be %.2f, got %.2f", initial+500, state.PeakEquityUSD)
	}
	if state.CurrentEquityUSD != initial+500 {
		t.Errorf("current equity should be %.2f, got %.2f", initial+500, state.CurrentEquityUSD)
	}
}

// Test drawdown triggers PAUSED mode (MaxDrawdownPct = 3%).
func TestDrawdownTriggersPause(t *testing.T) {
	d := NewDaemon(testPolicy())
	// Peak is 100000. 3% drawdown = -3000.
	d.RecordFill(-3100, false) // slightly over threshold
	if d.CurrentMode() != ModePaused {
		t.Errorf("expected PAUSED at 3.1%% drawdown, got %s", d.CurrentMode())
	}
}

// Test severe drawdown triggers SAFE mode (SafeModeDrawdownPct = 5%).
func TestSevereDrawdownTriggersSafeMode(t *testing.T) {
	d := NewDaemon(testPolicy())
	// Peak is 100000. 5% drawdown = -5000.
	d.RecordFill(-5100, false)
	if d.CurrentMode() != ModeSafe {
		t.Errorf("expected SAFE at 5.1%% drawdown, got %s", d.CurrentMode())
	}
}

// Test mode only escalates, never de-escalates automatically.
func TestModeOnlyEscalates(t *testing.T) {
	d := NewDaemon(testPolicy())
	// Trigger PAUSED via drawdown.
	d.RecordFill(-3100, false)
	if d.CurrentMode() != ModePaused {
		t.Fatalf("expected PAUSED, got %s", d.CurrentMode())
	}
	// Profitable fill should NOT de-escalate back to RUNNING.
	d.RecordFill(5000, false)
	if d.CurrentMode() != ModePaused {
		t.Errorf("mode should stay PAUSED (no auto de-escalation), got %s", d.CurrentMode())
	}
}

// Test HALTED and FLATTEN are terminal — no further transitions.
func TestHaltedIsTerminal(t *testing.T) {
	d := NewDaemon(testPolicy())
	d.SetMode(ModeHalted, "manual halt")
	// Even a severe loss should not change mode.
	alerts := d.RecordFill(-50000, false)
	if d.CurrentMode() != ModeHalted {
		t.Errorf("expected HALTED to be terminal, got %s", d.CurrentMode())
	}
	if len(alerts) != 0 {
		t.Errorf("expected no alerts in HALTED state, got %d", len(alerts))
	}
}

func TestFlattenIsTerminal(t *testing.T) {
	d := NewDaemon(testPolicy())
	d.SetMode(ModeFlatten, "emergency flatten")
	alerts := d.RecordFill(-50000, false)
	if d.CurrentMode() != ModeFlatten {
		t.Errorf("expected FLATTEN to be terminal, got %s", d.CurrentMode())
	}
	if len(alerts) != 0 {
		t.Errorf("expected no alerts in FLATTEN state, got %d", len(alerts))
	}
}

// Test error rate triggers PAUSED (MaxErrorRate5mPct = 20%).
func TestHighErrorRateTriggersPause(t *testing.T) {
	d := NewDaemon(testPolicy())
	// Record 4 errors out of 5 total events = 80% error rate.
	d.RecordFill(0, true)
	d.RecordFill(0, true)
	d.RecordFill(0, true)
	d.RecordFill(0, true)
	d.RecordFill(0, false) // one success
	if d.CurrentMode() != ModePaused {
		t.Errorf("expected PAUSED at 80%% error rate, got %s", d.CurrentMode())
	}
}

// Test blacklisted venues trigger SAFE mode.
func TestMultipleBlacklistedVenuesTriggerSafe(t *testing.T) {
	d := NewDaemon(testPolicy()) // MinCoreVenuesForSafeMode = 2
	d.RecordBlacklist("binance", 60000)
	if d.CurrentMode() != ModeRunning {
		t.Errorf("one blacklisted venue should stay RUNNING, got %s", d.CurrentMode())
	}
	d.RecordBlacklist("okx", 60000)
	if d.CurrentMode() != ModeSafe {
		t.Errorf("two blacklisted venues should trigger SAFE, got %s", d.CurrentMode())
	}
}

// Test expired blacklists are not counted.
func TestExpiredBlacklistNotCounted(t *testing.T) {
	d := NewDaemon(testPolicy())
	// TTL of 0ms means it expires immediately.
	d.RecordBlacklist("binance", 0)
	d.RecordBlacklist("okx", 0)
	// By next tick the entries should have expired.
	d.Tick()
	if d.CurrentMode() != ModeRunning {
		t.Errorf("expired blacklists should not trigger SAFE, got %s", d.CurrentMode())
	}
}

// Test ADL risk triggers PAUSED (ADLRiskPausePct = 40).
func TestADLRiskTriggersPause(t *testing.T) {
	d := NewDaemon(testPolicy())
	d.RecordADLRisk("binance", 50) // above 40% threshold
	if d.CurrentMode() != ModePaused {
		t.Errorf("expected PAUSED on high ADL risk, got %s", d.CurrentMode())
	}
}

// Test liquidation cluster count triggers PAUSED.
func TestLiqClusterTriggersPause(t *testing.T) {
	d := NewDaemon(testPolicy()) // LiqClusterPauseCount = 3
	d.RecordLiqCluster(2)
	if d.CurrentMode() != ModeRunning {
		t.Errorf("2 clusters should stay RUNNING, got %s", d.CurrentMode())
	}
	d.RecordLiqCluster(3)
	if d.CurrentMode() != ModePaused {
		t.Errorf("3 clusters should trigger PAUSED, got %s", d.CurrentMode())
	}
}

// Test venue deleveraging events trigger SAFE mode.
func TestVenueDelevTriggersSafe(t *testing.T) {
	d := NewDaemon(testPolicy()) // VenueDelevSafeModeCount = 2
	d.RecordVenueDelevEvent()
	if d.CurrentMode() != ModeRunning {
		t.Errorf("1 delev event should stay RUNNING, got %s", d.CurrentMode())
	}
	d.RecordVenueDelevEvent()
	if d.CurrentMode() != ModeSafe {
		t.Errorf("2 delev events should trigger SAFE, got %s", d.CurrentMode())
	}
}

// Test SetMode forces transition.
func TestSetModeForces(t *testing.T) {
	d := NewDaemon(testPolicy())
	d.SetMode(ModeSafe, "operator command")
	if d.CurrentMode() != ModeSafe {
		t.Errorf("SetMode should force SAFE, got %s", d.CurrentMode())
	}
	state := d.CurrentState()
	if state.Reason != "operator command" {
		t.Errorf("reason should be 'operator command', got %q", state.Reason)
	}
}

// Test CurrentState snapshot includes correct fields.
func TestCurrentStateSnapshot(t *testing.T) {
	d := NewDaemon(testPolicy())
	d.RecordFill(-1000, false) // 1% drawdown
	state := d.CurrentState()
	if state.TenantID != "test" {
		t.Errorf("tenant_id should be 'test', got %q", state.TenantID)
	}
	if state.TsMs == 0 {
		t.Error("TsMs should be set")
	}
	if state.DrawdownPct < 0.9 || state.DrawdownPct > 1.1 {
		t.Errorf("drawdown should be ~1%%, got %.4f%%", state.DrawdownPct)
	}
	if state.PeakEquityUSD != 100000 {
		t.Errorf("peak should be 100000, got %.2f", state.PeakEquityUSD)
	}
}

// Test modeRank ordering.
func TestModeRankOrdering(t *testing.T) {
	modes := []Mode{ModeRunning, ModePaused, ModeSafe, ModeFlatten, ModeHalted}
	for i := 1; i < len(modes); i++ {
		if modeRank(modes[i]) <= modeRank(modes[i-1]) {
			t.Errorf("rank(%s)=%d should be > rank(%s)=%d",
				modes[i], modeRank(modes[i]),
				modes[i-1], modeRank(modes[i-1]))
		}
	}
}

// Test pruneOlderThan utility function.
func TestPruneOlderThan(t *testing.T) {
	ts := []int64{100, 200, 300, 400, 500}
	result := pruneOlderThan(ts, 300)
	if len(result) != 2 {
		t.Errorf("expected 2 remaining after prune at 300, got %d", len(result))
	}
	if result[0] != 400 {
		t.Errorf("first remaining should be 400, got %d", result[0])
	}
}

// Test escalation order: drawdown triggers PAUSED then SAFE.
func TestEscalationFromPauseToSafe(t *testing.T) {
	d := NewDaemon(testPolicy())
	// 3.5% drawdown → PAUSED.
	d.RecordFill(-3500, false)
	if d.CurrentMode() != ModePaused {
		t.Fatalf("expected PAUSED at 3.5%% drawdown, got %s", d.CurrentMode())
	}
	// Additional loss to 5.5% → SAFE.
	d.RecordFill(-2000, false)
	if d.CurrentMode() != ModeSafe {
		t.Errorf("expected SAFE at 5.5%% drawdown, got %s", d.CurrentMode())
	}
}

// Test alerts contain correct metadata.
func TestAlertMetadata(t *testing.T) {
	d := NewDaemon(testPolicy())
	alerts := d.RecordFill(-5100, false) // triggers SAFE
	found := false
	for _, a := range alerts {
		if a.Source == "drawdown_safe_mode" {
			found = true
			if a.Severity != "CRITICAL" {
				t.Errorf("safe mode alert should be CRITICAL, got %s", a.Severity)
			}
			if a.TenantID != "test" {
				t.Errorf("alert tenant should be 'test', got %s", a.TenantID)
			}
			if a.Threshold != 5.0 {
				t.Errorf("alert threshold should be 5.0, got %.2f", a.Threshold)
			}
		}
	}
	if !found {
		t.Error("expected drawdown_safe_mode alert")
	}
}
