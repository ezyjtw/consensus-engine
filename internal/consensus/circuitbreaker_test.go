package consensus

import "testing"

var cbPolicy = &Policy{
	OutlierBpsWarn:      25.0,
	OutlierBpsBlacklist: 50.0,
	WarnPersistMs:       500,
	BlacklistPersistMs:  1500,
	BlacklistTtlMs:      60000,
	RecoveryMs:          2000,
}

const t0 = int64(1_700_000_000_000)

// spec §10 test 6a: deviation persists beyond WarnPersistMs → WARN
func TestCircuitBreakerTransitionToWarn(t *testing.T) {
	status := VenueStatus{State: StateOK}
	devBps := 30.0 // > OutlierBpsWarn (25)

	// first tick: sets OutlierSinceMs, no transition yet
	next, _, transitioned := UpdateVenueStatus(t0, status, devBps, cbPolicy)
	if transitioned {
		t.Fatal("should not transition on first tick")
	}
	if next.OutlierSinceMs != t0 {
		t.Errorf("OutlierSinceMs should be set to t0=%d, got %d", t0, next.OutlierSinceMs)
	}

	// after WarnPersistMs (500ms): must transition to WARN
	next2, _, transitioned2 := UpdateVenueStatus(t0+600, next, devBps, cbPolicy)
	if !transitioned2 {
		t.Fatal("should transition to WARN after WarnPersistMs")
	}
	if next2.State != StateWarn {
		t.Errorf("expected WARN, got %s", next2.State)
	}
}

// spec §10 test 6b: hard-outlier persists beyond BlacklistPersistMs → BLACKLISTED with TTL
func TestCircuitBreakerTransitionToBlacklist(t *testing.T) {
	// start in WARN to take the shorter blacklist path from StateWarn
	status := VenueStatus{State: StateWarn, WarnSinceMs: t0}
	devBps := 80.0 // > OutlierBpsBlacklist (50)

	// first tick: sets HardOutlierSinceMs, not yet blacklisted
	next, _, transitioned := UpdateVenueStatus(t0, status, devBps, cbPolicy)
	if transitioned {
		t.Fatal("should not blacklist on first tick")
	}
	if next.HardOutlierSinceMs != t0 {
		t.Errorf("HardOutlierSinceMs should be t0=%d, got %d", t0, next.HardOutlierSinceMs)
	}

	// after BlacklistPersistMs (1500ms): must blacklist
	next2, anomaly, transitioned2 := UpdateVenueStatus(t0+2000, next, devBps, cbPolicy)
	if !transitioned2 {
		t.Fatal("should blacklist after BlacklistPersistMs")
	}
	if next2.State != StateBlacklisted {
		t.Errorf("expected BLACKLISTED, got %s", next2.State)
	}
	if anomaly == nil {
		t.Error("expected anomaly event on blacklist transition")
	}
	wantUntil := t0 + 2000 + cbPolicy.BlacklistTtlMs
	if next2.BlacklistUntilMs != wantUntil {
		t.Errorf("BlacklistUntilMs = %d, want %d", next2.BlacklistUntilMs, wantUntil)
	}
}

// spec §10 test 6c: after TTL expires → WARN; after RecoveryMs with no deviation → OK
func TestCircuitBreakerRecovery(t *testing.T) {
	expiry := t0 + cbPolicy.BlacklistTtlMs

	// still blacklisted just before expiry
	status := VenueStatus{State: StateBlacklisted, BlacklistUntilMs: expiry}
	_, _, transitioned := UpdateVenueStatus(expiry-1, status, 0, cbPolicy)
	if transitioned {
		t.Fatal("should remain blacklisted before TTL expiry")
	}

	// at expiry: transition to WARN (recovery mode)
	warn, _, transitioned2 := UpdateVenueStatus(expiry, status, 0, cbPolicy)
	if !transitioned2 {
		t.Fatal("should transition to WARN at TTL expiry")
	}
	if warn.State != StateWarn {
		t.Errorf("expected WARN after TTL expiry, got %s", warn.State)
	}
	if warn.RecoverySinceMs != expiry {
		t.Errorf("RecoverySinceMs should be %d, got %d", expiry, warn.RecoverySinceMs)
	}

	// deviation still present: recovery timer reset, no OK transition
	mid, _, transitioned3 := UpdateVenueStatus(expiry+cbPolicy.RecoveryMs+1, warn, 40.0 /*> OutlierBpsWarn*/, cbPolicy)
	if transitioned3 {
		t.Fatal("should not recover to OK while deviation persists")
	}
	_ = mid

	// no deviation for RecoveryMs: transition to OK
	warn.RecoverySinceMs = expiry
	ok, _, transitioned4 := UpdateVenueStatus(expiry+cbPolicy.RecoveryMs+1, warn, 0, cbPolicy)
	if !transitioned4 {
		t.Fatal("should recover to OK after RecoveryMs with no deviation")
	}
	if ok.State != StateOK {
		t.Errorf("expected OK after recovery, got %s", ok.State)
	}
}

// deviation below threshold does not set outlier timer
func TestCircuitBreakerNoOutlierBelowThreshold(t *testing.T) {
	status := VenueStatus{State: StateOK}
	next, _, transitioned := UpdateVenueStatus(t0, status, 10.0 /*< OutlierBpsWarn*/, cbPolicy)
	if transitioned {
		t.Error("no transition expected for sub-threshold deviation")
	}
	if next.OutlierSinceMs != 0 {
		t.Errorf("OutlierSinceMs should remain 0, got %d", next.OutlierSinceMs)
	}
}
