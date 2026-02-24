# Failure Modes and Incident Playbooks

> How the system detects, responds to, and recovers from operational failures.

---

## Risk Daemon State Machine

```
RUNNING ──→ PAUSED ──→ SAFE_MODE ──→ FLATTEN ──→ HALTED
   ↑           │           │            │
   └───────────┴───────────┘            │
        (manual resolution)              │
                                         └──→ (manual restart required)
```

| Mode | Behavior | Triggers |
|---|---|---|
| **RUNNING** | Normal operation | Default state |
| **PAUSED** | No new intents; existing orders finish | Kill switch, drawdown > threshold, ADL risk, error rate |
| **SAFE_MODE** | Reduce-only hedging | Venue deleveraging events, approaching drawdown limit |
| **FLATTEN** | Close all positions | Drawdown > safe_mode_drawdown_pct |
| **HALTED** | System completely stopped | All positions closed after FLATTEN |

---

## Incident Playbooks

Each playbook defines a structured automated response to a detected incident.

### 1. Venue Maintenance (`VENUE_MAINTENANCE`)

**Trigger:** Venue-wide deleveraging events >= `venue_delev_safe_mode_count` within rolling window.

**Actions:**
1. Enter SAFE mode (reduce-only)
2. Cancel all pending orders on affected venues
3. Mark affected venues as degraded in consensus
4. Require manual resolution before resuming

**Resolution:** Manual — operator must call `ResolvePlaybook(VENUE_MAINTENANCE)` after confirming venue stability.

### 2. Volatility Spike (`VOLATILITY_SPIKE`)

**Trigger:** Drawdown approaching threshold (> 75% of `max_drawdown_pct`).

**Actions:**
1. Enter SAFE mode (reduce-only hedging)
2. Widen slippage tolerance on open hedges
3. Reduce position sizing
4. Alert operator for manual review

**Resolution:** Auto-resolves after 10 minutes if conditions stabilize.

### 3. API Degradation (`API_DEGRADATION`)

**Trigger:** 5-minute error rate >= `max_error_rate_5m_pct`.

**Actions:**
1. PAUSE new order placement
2. Log degraded venue API endpoints
3. Retry with exponential backoff

**Resolution:** Auto-resolves after 5 minutes if error rate drops.

### 4. ADL Event (`ADL_EVENT`)

**Trigger:** ADL risk % >= `adl_risk_pause_pct` at any venue.

**Actions:**
1. PAUSE new order placement
2. Mark affected venues as degraded
3. Alert operator for manual review

**Resolution:** Auto-resolves after 5 minutes if ADL risk subsides.

**Detection:** The `LiveExecutor.DetectADLEvents()` method polls exchanges that implement the `ADLDetector` interface. Detected events are reported to the risk daemon for immediate action.

### 5. Liquidation Cascade (`LIQUIDATION_CASCADE`)

**Trigger:** Liquidation clusters within `liq_cluster_window_bps` of mid >= `liq_cluster_pause_count`.

**Actions:**
1. PAUSE new order placement
2. Widen slippage tolerance on open hedges
3. Reduce position sizing to quarter-Kelly

**Resolution:** Auto-resolves after 10 minutes.

---

## Reconciliation Mismatch

**Detection:** `LiveExecutor.reconcileFills()` compares internal fill records against exchange state after every fill. `ReconcilePositions()` performs periodic truth pulls.

**Divergence Types:**
- **Quantity divergence:** Internal qty != exchange qty
- **Price divergence:** Internal avg price != exchange avg price
- **Fee divergence:** Internal fees != exchange fees

**Response:**
1. Log divergence with WARN severity
2. Emit `RECON_DIVERGENCE` execution event
3. If divergence > `max_reconciliation_divergence_usd`, alert operator
4. Periodic reconciliation runs every 30s in LIVE mode

---

## Configuration Thresholds

All thresholds are configurable in `configs/policies/risk.yaml`:

```yaml
max_net_delta_usd: 5000
max_drawdown_pct: 3.0
safe_mode_drawdown_pct: 5.0
max_error_rate_5m_pct: 10.0
min_core_venues_for_safe_mode: 2
adl_risk_pause_pct: 40
liq_cluster_pause_count: 3
venue_delev_safe_mode_count: 2
venue_delev_window_ms: 300000
```

---

## Kill Switch Behavior

When `kill:switch` is set in Redis:

| Service | Behavior |
|---|---|
| consensus-engine | Skips consensus computation |
| arb-engine | Pauses opportunity detection |
| funding-engine | Skips evaluation |
| liquidity-engine | Skips intent emission |
| capital-allocator | Blocks approvals |
| execution-router | Blocks trade execution |
| risk-daemon | Auto-transitions to PAUSED |

---

## Monitoring Endpoints

| Endpoint | Description |
|---|---|
| `GET /api/risk/state` | Current risk state snapshot (mode, drawdown, metrics) |
| `GET /api/risk/alerts` | Recent risk alerts |
| `GET /api/risk/history` | Historical risk state snapshots |
| `GET /api/timeline` | Merged activity feed (anomalies, fills, risk actions) |
| `GET /api/mode` | Current system mode + kill switch status |
