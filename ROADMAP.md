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
| Execution Router | `cmd/execution-router` | **Done** | 203 + 1377 | Paper executor (latency + slippage simulation, adverse selection, per-leg fills). Live executor (progressive limit→IOC, cancel/replace, partial fill recovery, hedge drift, emergency unwind, fill reconciliation, idempotency, depth-based hedge sequencing). |
| Risk Daemon | `cmd/risk-daemon` | **Done** | 192 + 787 | Full mode machine (RUNNING→PAUSED→SAFE→FLATTEN→HALTED). Drawdown, error rate, hedge drift, ADL risk, liquidation clusters, venue deleveraging. 5 incident playbooks. |
| Ledger | `cmd/ledger` | **Done** | 174 + 618 | Postgres (pgx/v5) auto-migrate. Fills, execution events, risk state/alerts, audit log, PnL summary, KPI, PnL attribution. |
| Liquidity Engine | `cmd/liquidity-engine` | **Done** | 162 + 306 | Spread blowout, thin book, mark-index divergence, imbalance, cascade proxy. System mode check. |
| Transfer Policy | `cmd/transfer-policy` | **Done** | 95 + 872 | Allowlist enforcement, SHA-256 tamper detection, velocity limits, dual approval gate, region/jurisdiction constraints. HTTP `/check` endpoint. Dashboard approval UI. |
| Treasury | `cmd/treasury` | **Done** | 85 + 853 | Deposit detection, fiat→USDC conversion, multi-venue distribution, profit sweeps, balance reconciliation. Kill switch + system mode + transfer-policy enforcement. |
| Dashboard | `cmd/dashboard` | **Done** | 149 + 2490 | 13-tab mobile-first UI. 30+ REST endpoints. SSE streaming. RBAC (4 roles). API key management. Tenant branding. CSV/audit exports. Transfer approval workflow. PnL attribution drill-down. Mandatory auth in non-dev. |

**Total Go code:** ~22,000+ lines across 12 services + 18 internal packages.

### 1.2 Infrastructure

| Component | Status | Notes |
|---|---|---|
| Docker Compose | **Done** | All 12 services + Redis 7 + Postgres 16 |
| CI (GitHub Actions) | **Done** | `go vet` + `golangci-lint` + `go test -race -cover` + all Docker builds + compose smoke test |
| Exchange adapters | **Done** | 5 exchanges: Coinbase, Binance, OKX, Bybit, Deribit. REST clients with AmendOrder + ADLDetector optional interfaces. |
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
    → dashboard (13-tab UI + 30+ API endpoints + SSE)
```

---

## 2. Target Specification

This section describes what "100% complete" means for each scoring dimension.

### Score 1 — V1 Paper/Shadow Platform (100%)

"Compose up" runs, dashboard is usable on mobile/desktop, paper/shadow results are explainable and reconciled, and failure modes are handled safely.

| # | Requirement | Status | Gap |
|---|---|---|---|
| 1 | All services run end-to-end via `docker compose up` | **Done** | — |
| 2 | Paper fills simulated with latency + slippage model | **Done** | Per-venue latency profiles, depth-based slippage, probabilistic partial fills all implemented |
| 3 | Shadow mode tracks predicted vs realized edge | **Done** | `ShadowMetrics` tracker: predicted/realized/missed edge, capture ratio, slippage sensitivity |
| 4 | PnL attribution per strategy/venue/symbol | **Done** | `pnl_attribution` table with per-venue breakdown, fee/funding/slippage separation. API: `/api/pnl/by-venue`, `/api/pnl/by-strategy` |
| 5 | Position/balance truth model in paper | **Done** | Redis-backed paper positions updated from fills |
| 6 | Risk daemon authoritative over all services | **Done** | All services check `kill:switch` + `risk:mode`. Mode hierarchy enforced. |
| 7 | Hedge drift enforcement in paper | **Done** | LiveExecutor tracks unhedged exposure time, aborts + emergency unwind if exceeded |
| 8 | Fault injection tests (staleness, venue blacklist, Redis disconnect, execution errors) | **Done** | 12 fault injection tests covering staleness, blacklist, expired intents, missing prices, drawdown, error rate, ADL, deleveraging, playbook lifecycle, quality gating |
| 9 | Deterministic integration test harness (replay mode) | **Done** | `internal/integration/replay_test.go` + `fault_test.go`: pipeline verification without Redis |
| 10 | Dashboard operator cockpit (mobile-first) | **Done** | System mode, kill switch, PnL, risk lights, exposure summary all present |
| 11 | Activity timeline (black box recorder) | **Done** | Merged feed: anomalies → intents → approvals → fills → risk actions |
| 12 | Paper/Shadow/Live toggle with graduation guardrails | **Done** | Mode toggle with enforced min paper days (7), min shadow days (14), Sharpe >= 0.5, drawdown <= 5%, confidence score thresholds, fill count minimums |
| 13 | Versioned event schemas for all streams | **Done** | `schema_version` field on all stream structs |
| 14 | Event schema documentation | **Done** | `docs/schemas/events.md` |

### Score 2 — Live Readiness (100%)

You can run live on 1-2 exchanges safely, reconcile to venue truth, recover from partial fills, and automatically derisk from incidents.

| # | Requirement | Status | Gap |
|---|---|---|---|
| 15 | Full order lifecycle per exchange (place/cancel/amend/reduce-only/post-only/idempotency) | **Done** | PlaceOrder, CancelOrder, GetOrderStatus implemented. `Amender` optional interface for amend-capable exchanges. `IdempotencyKey` on all order requests prevents duplicate placement. |
| 16 | Venue constraints normalization (tick/lot/min notional/rate limits) | **Done** | `VenueConstraints` with RoundPrice/RoundQty on all 5 adapters |
| 17 | Partial-fill recovery (live) — hedge residual immediately | **Done** | LiveExecutor adjusts second leg, emergency unwind on failure |
| 18 | Hedge sequencing logic (simultaneous vs hedge-first based on depth) | **Done** | `orderLegsByDepth()` uses venue constraints as depth proxy to execute thinner-book side first |
| 19 | Cancel/replace strategy + timeouts | **Done** | Progressive LIMIT→wider LIMIT→IOC with bounded retries and TTL expiry |
| 20 | Trade/fill reconciliation (pull from exchange, compare to internal) | **Done** | Async post-fill verification (qty, price, fee divergence) |
| 21 | Balance/position reconciliation (periodic truth pulls) | **Done** | `StartPeriodicReconciliation()` runs every 30s in LIVE mode, reconciles positions and balances across all venues |
| 22 | ADL awareness (detect unexpected position reduction) | **Done** | `ADLDetector` interface on exchange adapters. `LiveExecutor.DetectADLEvents()` polls for ADL events. Risk daemon tracks ADL risk % and activates playbook. |
| 23 | Incident playbooks (maintenance mode, volatility spike, API degradation) | **Done** | 5 playbooks: VENUE_MAINTENANCE, VOLATILITY_SPIKE, API_DEGRADATION, ADL_EVENT, LIQUIDATION_CASCADE. Auto-resolve + manual resolution support. |
| 24 | Live micro-size graduation harness (hard caps, conservative thresholds) | **Done** | `GraduationHarness` with 4-week ramp schedule (Week 1: $5k/order, $25k/daily → Week 4: $40k/order, $200k/daily). Per-order + daily hard caps enforced in `LiveExecutor`. |

### Score 3 — Institutional Transfer Controls (100%)

Every transfer is policy-checked, approved, capped, region-compliant, tamper-resistant, and fully audited.

| # | Requirement | Status | Gap |
|---|---|---|---|
| 25 | Treasury calls transfer-policy before every withdrawal/sweep | **Done** | Fail-closed: unreachable = deny |
| 26 | Enforced destination allowlists (per chain/asset/venue + memo support) | **Done** | In transfer-policy engine |
| 27 | Transfer proposal → approval → execution workflow | **Done** | Dashboard API: `GET /api/transfers/pending`, `POST /api/transfers/approve`, `POST /api/transfers/deny`. Approval events published to Redis streams. Audit logged. |
| 28 | Velocity limits (per-transfer, daily, count, new address cooling) | **Done** | Configured in transfer policy YAML |
| 29 | Policy hash sealing (tamper detection → pause + disable withdrawals) | **Done** | SHA-256 hash checked on startup |
| 30 | Region/jurisdiction constraints (block restricted flows) | **Done** | `VenueRegion` struct with per-venue blocked regions. 5 venues configured (Binance, OKX, Bybit, Deribit, Coinbase) with US/UK/APAC restrictions. |
| 31 | Immutable audit log (who approved, when, what, why) | **Done** | Postgres append-only + CSV export endpoint |
| 32 | Dual approval for large transfers | **Done** | Two-person rule above configurable threshold ($25k default). Requester cannot self-approve. Approval expiry (24h default). |

---

## 3. Documentation

| Document | Path | Contents |
|---|---|---|
| Graduation to LIVE | `docs/graduation.md` | Mode hierarchy, graduation checklist, confidence scoring, micro-live ramp, kill switch procedure |
| Failure Modes & Playbooks | `docs/failure-modes.md` | Risk state machine, 5 incident playbooks, reconciliation mismatch handling, kill switch behavior |
| Testing & Verification | `docs/testing.md` | Unit tests, replay harness, fault injection, shadow confidence, graduation tests, compose smoke test |
| Event Schemas | `docs/schemas/events.md` | Versioned event schemas for all Redis streams |

---

## 4. Version Milestones

### V1 — Paper/Shadow Platform

**Goal:** End-to-end paper/shadow trading with explainable PnL, risk controls, and a usable dashboard.

**Status: Complete.** All 14 requirements met. Paper fill calibration, shadow confidence metrics, PnL attribution drill-down, fault injection tests, deterministic replay harness, graduation guardrails, and CI smoke test all implemented.

### V2 — Live Trading

**Goal:** Safe live execution on 1-2 venues with reconciliation and incident handling.

**Status: Complete.** All 10 requirements met. Order idempotency, amend interface, depth-based hedge sequencing, periodic position reconciliation, ADL event detection, 5 incident playbooks, and micro-live graduation harness all implemented.

### V3 — Institutional Polish

**Goal:** Multi-tenant white-label platform with full compliance controls.

**Status: Complete.** All 8 requirements met. Dashboard transfer approval workflow, region/jurisdiction constraints, and dual approval for large transfers all implemented alongside existing RBAC, multi-tenant branding, API keys, audit trail, and CSV/SOC2 exports.
