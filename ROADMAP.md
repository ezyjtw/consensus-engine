# ArbSuite — Product Roadmap

> Last updated: 2026-02-24
> Module: `github.com/ezyjtw/consensus-engine`

---

## 1. Current State

### 1.1 Services — what is built and running

| Service | Binary | Status | Lines | Notes |
|---|---|---|---|---|
| Market Data | `cmd/market-data` | **Done** | 126 + 1648 | Binance/OKX/Bybit/Deribit WebSocket adapters. Publishes `market:quotes`. |
| Consensus Engine | `cmd/consensus-engine` | **Done** | 184 + 1251 | MAD outlier detection, trust model, circuit breaker, VWAP bands, system mode check. |
| Arb Engine | `cmd/arb-opportunity-engine` | **Done** | 387 + 1950 | Quality gating, venue filtering, latency-buffered edge, cooldown, disjoint pairs. Basis, cascade, correlation, DEX-CEX sub-strategies. |
| Funding Engine | `cmd/funding-engine` | **Done** | 181 + 1479 | Carry + differential strategies, regime detection (EWA/momentum/StdDev), volatility gate, system mode check. |
| Capital Allocator | `cmd/capital-allocator` | **Done** | 185 + 547 | Quality gating, system mode gating, per-strategy/venue notional caps, fractional Kelly sizing. |
| Execution Router | `cmd/execution-router` | **Done** | 203 + 1377 | Paper executor (latency + slippage simulation, adverse selection, per-leg fills). Live executor (progressive limit→IOC, cancel/replace, partial fill recovery, hedge drift, emergency unwind, fill reconciliation). |
| Risk Daemon | `cmd/risk-daemon` | **Done** | 192 + 787 | Full mode machine (RUNNING→PAUSED→SAFE→FLATTEN→HALTED). Drawdown, error rate, hedge drift, ADL risk, liquidation clusters, venue deleveraging. |
| Ledger | `cmd/ledger` | **Done** | 174 + 618 | Postgres (pgx/v5) auto-migrate. Fills, execution events, risk state/alerts, audit log, PnL summary, KPI. |
| Liquidity Engine | `cmd/liquidity-engine` | **Done** | 162 + 306 | Spread blowout, thin book, mark-index divergence, imbalance, cascade proxy. System mode check. |
| Transfer Policy | `cmd/transfer-policy` | **Done** | 95 + 872 | Allowlist enforcement, SHA-256 tamper detection, velocity limits, manual approval gate. HTTP `/check` endpoint. |
| Treasury | `cmd/treasury` | **Done** | 85 + 853 | Deposit detection, fiat→USDC conversion, multi-venue distribution, profit sweeps, balance reconciliation. Kill switch + system mode + transfer-policy enforcement. |
| Dashboard | `cmd/dashboard` | **Done** | 149 + 2490 | 13-tab mobile-first UI. 24 REST endpoints. SSE streaming. RBAC (4 roles). API key management. Tenant branding. CSV/audit exports. Mandatory auth in non-dev. |

**Total Go code:** ~21,000 lines across 12 services + 18 internal packages.

### 1.2 Infrastructure

| Component | Status | Notes |
|---|---|---|
| Docker Compose | **Done** | All 12 services + Redis 7 + Postgres 16 |
| CI (GitHub Actions) | **Done** | `go vet` + `golangci-lint` + `go test -race -cover` + all Docker builds |
| Exchange adapters | **Done** | 5 exchanges: Coinbase, Binance, OKX, Bybit, Deribit. REST clients for orders/balances/constraints. |
| Multi-tenancy | **Done** | `tenant_id` on all events, isolated API keys, tenant branding |
| RBAC | **Done** | admin > trader > viewer > auditor. SHA-256 hashed API keys. |
| DEX routing | **Done** | 1inch Fusion + Paraswap (disabled by default) |
| L2 bridges | **Done** | Arbitrum/Optimism/Base (disabled by default) |

### 1.3 Data flow (working end-to-end)

```
Exchange WebSockets
    → market-data → market:quotes
        → consensus-engine → consensus:updates / consensus:anomalies / consensus:status
            → arb-engine ─────────┐
            → funding-engine ─────┤
            → liquidity-engine ───┤
                                  ▼
                          trade:intents
                              → capital-allocator → trade:intents:approved
                                  → execution-router → execution:events + demo:fills/live:fills
    → risk-daemon (monitors all streams, manages system mode + kill switch)
    → ledger (persists everything to Postgres)
    → treasury (deposit detection → conversion → distribution → sweeps)
    → dashboard (13-tab UI + 24 API endpoints + SSE)
```

---

## 2. Target Specification

This section describes what "100% complete" means for each scoring dimension.

### Score 1 — V1 Paper/Shadow Platform (100%)

"Compose up" runs, dashboard is usable on mobile/desktop, paper/shadow results are explainable and reconciled, and failure modes are handled safely.

| # | Requirement | Status | Gap |
|---|---|---|---|
| 1 | All services run end-to-end via `docker compose up` | **Done** | — |
| 2 | Paper fills simulated with latency + slippage model | **Done** | Needs calibration: per-venue latency profiles, depth-based slippage, probabilistic partial fills |
| 3 | Shadow mode tracks predicted vs realized edge | **Partial** | Shadow writes simulated fills but confidence metrics (missed edge, slippage sensitivity) not yet computed |
| 4 | PnL attribution per strategy/venue/symbol | **Partial** | Total PnL by strategy exists. Per-venue breakdown, fee/funding/slippage separation needed |
| 5 | Position/balance truth model in paper | **Done** | Redis-backed paper positions updated from fills |
| 6 | Risk daemon authoritative over all services | **Done** | All services check `kill:switch` + `risk:mode`. Mode hierarchy enforced. |
| 7 | Hedge drift enforcement in paper | **Done** | LiveExecutor tracks unhedged exposure time, aborts + emergency unwind if exceeded |
| 8 | Fault injection tests (staleness, venue blacklist, Redis disconnect, execution errors) | **Not started** | Need test harness |
| 9 | Deterministic integration test harness (replay mode) | **Not started** | Need recorded market data replay + single-command pipeline verification |
| 10 | Dashboard operator cockpit (mobile-first) | **Done** | System mode, kill switch, PnL, risk lights, exposure summary all present |
| 11 | Activity timeline (black box recorder) | **Done** | Merged feed: anomalies → intents → approvals → fills → risk actions |
| 12 | Paper/Shadow/Live toggle with graduation guardrails | **Partial** | Mode toggle exists. Graduation checklist (min paper time, Sharpe, drawdown) not enforced |
| 13 | Versioned event schemas for all streams | **Done** | `schema_version` field on all stream structs |
| 14 | Event schema documentation | **Done** | `docs/schemas/events.md` |

### Score 2 — Live Readiness (100%)

You can run live on 1-2 exchanges safely, reconcile to venue truth, recover from partial fills, and automatically derisk from incidents.

| # | Requirement | Status | Gap |
|---|---|---|---|
| 15 | Full order lifecycle per exchange (place/cancel/amend/reduce-only/post-only/idempotency) | **Partial** | PlaceOrder, CancelOrder, GetOrderStatus implemented. Amend not on all exchanges. Idempotency keys not used. |
| 16 | Venue constraints normalization (tick/lot/min notional/rate limits) | **Done** | `VenueConstraints` with RoundPrice/RoundQty on all 5 adapters |
| 17 | Partial-fill recovery (live) — hedge residual immediately | **Done** | LiveExecutor adjusts second leg, emergency unwind on failure |
| 18 | Hedge sequencing logic (simultaneous vs hedge-first based on depth) | **Partial** | Always simultaneous-or-hedge-first. Depth-based selection not implemented. |
| 19 | Cancel/replace strategy + timeouts | **Done** | Progressive LIMIT→wider LIMIT→IOC with bounded retries and TTL expiry |
| 20 | Trade/fill reconciliation (pull from exchange, compare to internal) | **Done** | Async post-fill verification (qty, price, fee divergence) |
| 21 | Balance/position reconciliation (periodic truth pulls) | **Partial** | Treasury reconciliation exists. Execution-side position reconciliation not yet periodic. |
| 22 | ADL awareness (detect unexpected position reduction) | **Partial** | Risk daemon tracks ADL risk %. Detection of actual ADL events not implemented. |
| 23 | Incident playbooks (maintenance mode, volatility spike, API degradation) | **Not started** | Risk daemon escalates but doesn't have venue-specific playbooks |
| 24 | Live micro-size graduation harness (hard caps, conservative thresholds) | **Not started** | Need per-order/daily hard caps for initial live period |

### Score 3 — Institutional Transfer Controls (100%)

Every transfer is policy-checked, approved, capped, region-compliant, tamper-resistant, and fully audited.

| # | Requirement | Status | Gap |
|---|---|---|---|
| 25 | Treasury calls transfer-policy before every withdrawal/sweep | **Done** | Fail-closed: unreachable = deny |
| 26 | Enforced destination allowlists (per chain/asset/venue + memo support) | **Done** | In transfer-policy engine |
| 27 | Transfer proposal → approval → execution workflow | **Partial** | Policy check exists. Dashboard approval UI not implemented. |
| 28 | Velocity limits (per-transfer, daily, count, new address cooling) | **Done** | Configured in transfer policy YAML |
| 29 | Policy hash sealing (tamper detection → pause + disable withdrawals) | **Done** | SHA-256 hash checked on startup |
| 30 | Region/jurisdiction constraints (block restricted flows) | **Not started** | Need venue registry with allowed regions |
| 31 | Immutable audit log (who approved, when, what, why) | **Done** | Postgres append-only + CSV export endpoint |
| 32 | Dual approval for large transfers | **Not started** | Two-person rule above threshold |

---

## 3. Remaining Work (Prioritized)

### Phase A — Universal Prerequisites

| Task | Items | Effort |
|---|---|---|
| ~~ROADMAP contradictions~~ | ~~#1~~ | ~~Done~~ |
| ~~Schema versioning~~ | ~~#13, #14~~ | ~~Done~~ |
| CI smoke test in compose | #2 | Add compose-based smoke test job to CI |
| Deterministic replay harness | #9 | Record market data, replay through pipeline, assert outputs |

### Phase B — V1 Paper/Shadow Completion

| Task | Items | Effort |
|---|---|---|
| Paper fill calibration | #2 | Per-venue latency profiles, depth-based slippage, probabilistic partial fills |
| Shadow confidence metrics | #3 | Compute edge predicted vs realized, missed-due-to-latency, slippage delta |
| PnL attribution drill-down | #4 | Per-venue, fee/funding/slippage separated |
| Fault injection tests | #8 | Simulate staleness, blacklists, Redis disconnect, execution errors |
| Graduation guardrails | #12 | Enforce min paper time + Sharpe + drawdown before mode upgrade |

### Phase C — Live Hardening

| Task | Items | Effort |
|---|---|---|
| Order idempotency + amend | #15 | Client-assigned order IDs, amend on supporting exchanges |
| Depth-based hedge sequencing | #18 | Use bid/ask depth to choose simultaneous vs hedge-first |
| Periodic position reconciliation | #21 | Execution-side balance truth pulls on timer |
| ADL event detection | #22 | Detect actual position reduction vs expected, auto-derisk |
| Incident playbooks | #23 | Venue maintenance, volatility spike, API degradation modes |
| Micro-live graduation harness | #24 | Per-order/daily hard caps, 2-4 week ramp schedule |

### Phase D — Institutional Transfer Controls

| Task | Items | Effort |
|---|---|---|
| Dashboard transfer approval UI | #27 | Propose → approve → execute in UI |
| Region/jurisdiction constraints | #30 | Venue registry with allowed regions, block restricted flows |
| Dual approval for large transfers | #32 | Two-person rule above configurable threshold |

---

## 4. Version Milestones

### V1 — Paper/Shadow Platform

**Goal:** End-to-end paper/shadow trading with explainable PnL, risk controls, and a usable dashboard.

**All V1 services are built and running.** Remaining work is calibration, testing, and polish (Phase A + B above).

### V2 — Live Trading

**Goal:** Safe live execution on 1-2 venues with reconciliation and incident handling.

**Core infrastructure is built** (LiveExecutor, exchange adapters, venue constraints, transfer-policy). Remaining work is hardening and graduation (Phase C above).

### V3 — Institutional Polish

**Goal:** Multi-tenant white-label platform with full compliance controls.

**Most V3 features are done:** RBAC, multi-tenant branding, API keys, audit trail, CSV/SOC2 exports, DEX routing, L2 bridges. Remaining: transfer approval UI, region constraints, dual approval (Phase D above).
