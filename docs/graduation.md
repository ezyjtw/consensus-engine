# Graduation to LIVE

> How the system enforces safe progression from PAPER to SHADOW to LIVE.

---

## Mode Hierarchy

| Mode | Description | Risk Level |
|------|-------------|------------|
| **PAPER** | Simulated fills against live market data. No real orders. | None |
| **SHADOW** | Simulated fills logged alongside what real fills *would have been*. Confidence metrics computed. | None |
| **LIVE** | Real orders placed on exchanges. Micro-live caps enforced. | Real capital at risk |

---

## Graduation Checklist

### PAPER to SHADOW

Before the system allows upgrade from PAPER to SHADOW, all of the following must be true:

| Requirement | Threshold | Enforced By |
|---|---|---|
| Minimum paper duration | 7 days | `gateway.go` — checks `trading:mode_started` Redis key |
| Minimum fill count | 50 fills | `gateway.go` — `computeConfidence()` |
| Confidence score | >= 50/100 | Composite of fill volume, win rate, Sharpe, slippage discipline |

### SHADOW to LIVE

| Requirement | Threshold | Enforced By |
|---|---|---|
| Minimum shadow duration | 14 days | `gateway.go` — checks `trading:mode_started` Redis key |
| Minimum fill count | 200 fills | `gateway.go` — `computeConfidence()` |
| Confidence score | >= 80/100 | Composite score |
| Sharpe proxy | >= 0.50 | `KPISummary()` from Postgres |
| Max drawdown | <= 5.0% | Risk state from Redis |
| Shadow edge capture ratio | Reviewed by operator | `ShadowMetrics.ConfidenceReport()` |

### Admin Override

All graduation checks can be bypassed with `force=true` in the mode transition request. This is available to `admin` role only and is logged in the audit trail.

---

## Confidence Score Computation

The confidence score is a composite of four sub-scores, each 0-100:

```
score = (fill_volume_score + win_rate_score + sharpe_score + slippage_score) / 4
```

| Sub-Score | Formula | Meaning |
|---|---|---|
| Fill Volume | `clamp(fill_count / 2, 0, 100)` | Sufficient sample size |
| Win Rate | `clamp((win_rate% - 50) / 20 * 100, 0, 100)` | Consistent profitability |
| Sharpe | `clamp(sharpe / 2 * 100, 0, 100)` | Risk-adjusted returns |
| Slippage Discipline | `clamp(100 - avg_slippage_bps * 10, 0, 100)` | Execution quality |

---

## Shadow Confidence Metrics

During SHADOW mode, the `ShadowMetrics` tracker computes:

| Metric | Description |
|---|---|
| `avg_predicted_edge_bps` | Average edge at signal time |
| `avg_realized_edge_bps` | Average edge captured after execution simulation |
| `avg_missed_edge_bps` | Edge lost due to latency and slippage |
| `edge_capture_ratio` | Realized / Predicted (target: > 0.7) |
| `slippage_sensitivity_bps` | How much slippage erodes edge |
| `avg_latency_ms` | Average signal-to-fill latency |

These metrics are available via `GET /api/paper/confidence`.

---

## Micro-Live Graduation Ramp

Once in LIVE mode, the system enforces a progressive ramp schedule:

| Week | Per-Order Cap | Daily Cap |
|---|---|---|
| Week 1 | $5,000 | $25,000 |
| Week 2 | $10,000 | $50,000 |
| Week 3 | $20,000 | $100,000 |
| Week 4 | $40,000 | $200,000 |

Defaults are configured in `configs/execution_router.yaml`:

```yaml
micro_live_graduation:
  max_order_notional_usd: 10000
  max_daily_notional_usd: 100000
  max_open_orders: 4
```

The `GraduationHarness` (`internal/execution/graduation.go`) can be wired to dynamically adjust these caps week-over-week.

---

## Kill Switch and Rollback

### Kill Switch
Setting `kill:switch` in Redis immediately pauses all services:
- Consensus engine skips computation
- Arb/funding/liquidity engines pause intent emission
- Capital allocator blocks approvals
- Execution router blocks order placement
- Risk daemon transitions to PAUSED

### Mode Rollback
Downgrades (e.g. LIVE to PAPER) are always allowed without checks. The mode transition is logged in the audit trail and `audit:mode_transitions` Redis stream.

### Emergency Procedure
1. Activate kill switch via dashboard or `SET kill:switch 1` in Redis
2. Risk daemon will auto-transition to PAUSED
3. Review open positions via `GET /api/positions`
4. If needed, enter FLATTEN mode to close all positions
5. After resolution, remove kill switch and set mode to RUNNING

---

## API Endpoints

| Endpoint | Method | Description |
|---|---|---|
| `/api/paper/mode` | GET | Current trading mode |
| `/api/paper/mode` | PUT | Set mode (with graduation checks) |
| `/api/paper/confidence` | GET | Confidence score + sub-scores |
| `/api/metrics/kpi` | GET | KPI summary (Sharpe, win rate, slippage) |
