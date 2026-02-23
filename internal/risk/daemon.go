package risk

import (
	"log"
	"sync"
	"time"
)

// Daemon tracks risk metrics and determines operating mode transitions.
type Daemon struct {
	policy *Policy
	mu     sync.Mutex
	state  State

	// Rolling execution error rate tracking (5-minute window).
	errorEvents  []int64 // unix ms timestamps of errors
	totalEvents  []int64 // unix ms timestamps of all fills

	// Blacklisted venues from consensus anomaly stream.
	blacklisted map[string]int64 // venue → blacklist expiry ms

	// Equity tracking.
	peakEquity    float64
	currentEquity float64
}

func NewDaemon(policy *Policy) *Daemon {
	return &Daemon{
		policy:      policy,
		blacklisted: make(map[string]int64),
		state: State{
			TenantID: policy.TenantID,
			Mode:     ModeRunning,
		},
		peakEquity:    100000, // seed with notional starting equity
		currentEquity: 100000,
	}
}

// CurrentMode returns the current operating mode.
func (d *Daemon) CurrentMode() Mode {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.state.Mode
}

// CurrentState returns a snapshot of the risk state.
func (d *Daemon) CurrentState() State {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.state
	s.TsMs = time.Now().UnixMilli()
	s.PeakEquityUSD = d.peakEquity
	s.CurrentEquityUSD = d.currentEquity
	if d.peakEquity > 0 {
		s.DrawdownPct = (d.peakEquity - d.currentEquity) / d.peakEquity * 100
	}
	// Copy blacklisted venues.
	now := time.Now().UnixMilli()
	var bl []string
	for v, exp := range d.blacklisted {
		if now < exp {
			bl = append(bl, v)
		}
	}
	s.BlacklistedVenues = bl
	return s
}

// RecordFill updates equity and error rate on a fill event.
func (d *Daemon) RecordFill(pnlUSD float64, isError bool) []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()

	now := time.Now().UnixMilli()
	d.totalEvents = append(d.totalEvents, now)
	if isError {
		d.errorEvents = append(d.errorEvents, now)
	}

	d.currentEquity += pnlUSD
	if d.currentEquity > d.peakEquity {
		d.peakEquity = d.currentEquity
	}

	return d.evaluate()
}

// RecordBlacklist marks a venue as blacklisted for ttlMs milliseconds.
func (d *Daemon) RecordBlacklist(venue string, ttlMs int64) []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.blacklisted[venue] = time.Now().UnixMilli() + ttlMs
	return d.evaluate()
}

// SetMode forces a mode transition (from kill switch / dashboard command).
func (d *Daemon) SetMode(m Mode, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	log.Printf("risk-daemon: mode transition %s → %s reason=%q", d.state.Mode, m, reason)
	d.state.Mode = m
	d.state.Reason = reason
}

// Tick runs periodic evaluation — call every few seconds.
func (d *Daemon) Tick() []Alert {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.evaluate()
}

// evaluate applies all risk rules and transitions mode accordingly.
// Must be called with d.mu held.
func (d *Daemon) evaluate() []Alert {
	var alerts []Alert
	now := time.Now().UnixMilli()

	// Prune event windows older than 5 minutes.
	window := int64(5 * 60 * 1000)
	d.errorEvents = pruneOlderThan(d.errorEvents, now-window)
	d.totalEvents = pruneOlderThan(d.totalEvents, now-window)

	// Error rate.
	errorRate := 0.0
	if len(d.totalEvents) > 0 {
		errorRate = float64(len(d.errorEvents)) / float64(len(d.totalEvents)) * 100
	}
	d.state.ErrorRate5mPct = errorRate

	// Drawdown.
	drawdown := 0.0
	if d.peakEquity > 0 {
		drawdown = (d.peakEquity - d.currentEquity) / d.peakEquity * 100
	}
	d.state.DrawdownPct = drawdown

	// Count currently blacklisted core venues.
	blacklistCount := 0
	for _, exp := range d.blacklisted {
		if now < exp {
			blacklistCount++
		}
	}

	// ── Mode transitions ───────────────────────────────────────────────────
	// Only escalate — don't automatically de-escalate (requires operator action).

	currentMode := d.state.Mode
	if currentMode == ModeHalted || currentMode == ModeFlatten {
		return alerts // terminal / in-progress states
	}

	newMode := currentMode

	// SAFE MODE triggers.
	if blacklistCount >= d.policy.MinCoreVenuesForSafeMode {
		alerts = append(alerts, d.alert("CRITICAL",
			"multiple_core_venues_blacklisted",
			"Too many core venues blacklisted", "blacklisted_count",
			float64(blacklistCount), float64(d.policy.MinCoreVenuesForSafeMode)))
		if modeRank(newMode) < modeRank(ModeSafe) {
			newMode = ModeSafe
		}
	}
	if drawdown >= d.policy.SafeModeDrawdownPct {
		alerts = append(alerts, d.alert("CRITICAL",
			"drawdown_safe_mode",
			"Drawdown exceeded safe-mode threshold", "drawdown_pct",
			drawdown, d.policy.SafeModeDrawdownPct))
		if modeRank(newMode) < modeRank(ModeSafe) {
			newMode = ModeSafe
		}
	}

	// PAUSED triggers.
	if drawdown >= d.policy.MaxDrawdownPct && modeRank(newMode) < modeRank(ModePaused) {
		alerts = append(alerts, d.alert("WARN",
			"drawdown_pause",
			"Drawdown exceeded pause threshold", "drawdown_pct",
			drawdown, d.policy.MaxDrawdownPct))
		newMode = ModePaused
	}
	if errorRate >= d.policy.MaxErrorRate5mPct && modeRank(newMode) < modeRank(ModePaused) {
		alerts = append(alerts, d.alert("WARN",
			"error_rate_pause",
			"Execution error rate exceeded threshold", "error_rate_5m_pct",
			errorRate, d.policy.MaxErrorRate5mPct))
		newMode = ModePaused
	}

	if newMode != currentMode {
		log.Printf("risk-daemon: auto-transition %s → %s", currentMode, newMode)
		d.state.Mode = newMode
	}

	return alerts
}

func (d *Daemon) alert(severity, source, msg, metric string, value, threshold float64) Alert {
	return Alert{
		TenantID:  d.policy.TenantID,
		TsMs:      time.Now().UnixMilli(),
		Source:    source,
		Severity:  severity,
		Message:   msg,
		Metric:    metric,
		Value:     value,
		Threshold: threshold,
	}
}

func pruneOlderThan(ts []int64, cutoffMs int64) []int64 {
	i := 0
	for i < len(ts) && ts[i] < cutoffMs {
		i++
	}
	return ts[i:]
}

func modeRank(m Mode) int {
	switch m {
	case ModeRunning:
		return 0
	case ModePaused:
		return 1
	case ModeSafe:
		return 2
	case ModeFlatten:
		return 3
	case ModeHalted:
		return 4
	default:
		return 0
	}
}
