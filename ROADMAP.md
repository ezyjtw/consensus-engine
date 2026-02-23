# ArbSuite — Full Product Roadmap & Implementation Status

> Last updated: 2026-02-22
> Status tracked against the full product specification.

---

## 0. What ArbSuite is

**ArbSuite** is an institutional-grade, multi-venue yield and inefficiency capture platform with self-custody support, real-time risk controls, and paper trading validation.

### Primary outcomes

1. **Generate yield from:**
   - Funding / basis capture
   - Cross-venue arbitrage
   - Liquidity inefficiency capture (retail slippage / liquidation flows)
   - Capital efficiency via DeFi borrow/lend (conservative)

2. **Operate safely:**
   - No single-venue reliance (consensus price validation)
   - Strict transfer policies (allowlists + tamper detection)
   - Kill switch modes: Pause → Safe Mode → Flatten All

3. **Build user trust:**
   - Paper trading with explainable PnL and risk attribution
   - Dashboard transparency and full audit trail
   - Onboarding path with graduation criteria before live trading

---

## 1. Current implementation status

### 1.1 At a glance

| Component | Status | V1 Required | Notes |
|---|---|---|---|
| **Market Data Service** | ❌ 0% | YES — BLOCKING | Nothing writes to `market:quotes`. All downstream engines are starved without this. |
| **Consensus Price Engine** | ✅ 95% | YES | Fully implemented. MAD outlier detection, trust model, circuit breaker, band computation, VWAP. |
| **Arb Opportunity Engine** | ✅ 95% | YES | Fully implemented. Quality gating, venue filtering, latency-buffered edge, cooldown, disjoint pairs. |
| **Funding Engine** | ❌ 0% | YES | Not started. |
| **Capital Allocator** | ❌ 0% | YES | Not started. |
| **Execution Router** | ❌ 0% | YES | Not started. Intents are emitted but never consumed. |
| **Risk Daemon** | ❌ 5% | YES | Kill switch key exists. No continuous risk metrics, no PAUSE/SAFE/FLATTEN logic. |
| **Ledger + Reconciliation** | ❌ 0% | YES (basic) | No persistent store. All state is in-memory or Redis only. |
| **Paper Trading Service** | ❌ 0% | YES | Not started. |
| **Rebalance + Transfer** | ❌ 0% | NO (V2) | Not started. |
| **Liquidity Inefficiency Engine** | ❌ 0% | NO (V2) | Not started. |
| **Collateral Manager (DeFi)** | ❌ 0% | NO (V2) | Not started. |
| **Gateway API** | ❌ 10% | YES | Dashboard exposes some endpoints. No positions/PnL/intents/risk endpoints. |
| **Dashboard UI** | 🟡 20% | YES | Real-time feed, kill switch, alerts, credentials. Missing all trading and PnL views. |
| **Postgres persistence** | ❌ 0% | YES (basic) | Not present. All data is ephemeral. |
| **Docker Compose** | ❌ 0% | YES | Two individual Dockerfiles exist; no compose stack. |

### 1.2 What is actually built and working

```
[nothing] ──────────────────────────────────────────────── market:quotes
                                                               │
                                                   ┌───────────▼────────────┐
                                                   │  Consensus Engine ✅   │
                                                   │  · MAD outlier detect  │
                                                   │  · Trust model         │
                                                   │  · Circuit breaker     │
                                                   │  · VWAP band           │
                                                   └──┬────────┬────────────┘
                                          consensus:  │        │ venue_anomalies
                                          updates     │        │ venue_status
                                                      │        │
                                          ┌───────────▼─────┐  └──► Dashboard ✅
                                          │  Arb Engine ✅  │       · Real-time SSE
                                          │  · Quality gate │       · Kill switch
                                          │  · Edge calc    │       · Credentials
                                          │  · Cooldown     │       · Alert webhooks
                                          └────────┬────────┘
                                                   │ trade:intents
                                                   │
                                              [nothing] ◄── Execution Router ❌
```

### 1.3 The single most critical gap

**The Market Data Service does not exist.** Every engine is built and ready to receive data, but nothing connects to exchange APIs and publishes to `market:quotes`. Fixing this is the prerequisite for any live testing.

---

## 2. System architecture — full specification

### Service A: Market Data Service

**Status:** ❌ Not started — V1 BLOCKER

**Runs as:** `cmd/market-data/`

**Subscriptions:** Exchange WebSocket connections (per venue)

**Publishes:** `market:quotes`

**Responsibilities:**
- Maintain best bid/ask, mark/index, funding rate, top-of-book depth summary
- Optional full orderbook snapshots (limited depth, e.g. top 10 levels)
- Feed health events: detect staleness, reconnects, sequence gaps
- Unified output format regardless of exchange differences

**Input payload per exchange:** Exchange-specific WebSocket → normalize to:
```json
{
  "tenant_id": "default",
  "venue": "binance",
  "symbol": "BTC-PERP",
  "ts_ms": 1700000000000,
  "best_bid": 100000.0,
  "best_ask": 100010.0,
  "mark": 100005.0,
  "index": 100003.0,
  "bid_depth_1pct": 250.0,
  "ask_depth_1pct": 220.0,
  "orderbook": {
    "bids": [[99990.0, 1.2], [99980.0, 2.0]],
    "asks": [[100010.0, 1.1], [100020.0, 1.9]]
  },
  "fee_bps_taker": 4.0,
  "feed_health": {
    "ws_connected": true,
    "last_msg_ts_ms": 1700000000000
  }
}
```

**Supported venues (V1):** binance, okx, bybit, deribit
**Optional venues:** htx, gate
**Symbols:** BTC-PERP, ETH-PERP (+ spot equivalents for sanity checks)

**Implementation notes:**
- One goroutine per venue per symbol
- On disconnect: attempt reconnect with exponential backoff; mark feed_health.ws_connected=false and continue publishing last-known quote with updated ts_ms so staleness filter fires
- Funding rates: separate REST poll every 30s or from funding WS channel if available
- Do NOT apply business logic here — just normalize and emit

**Config file:** `configs/market_data.yaml`
```yaml
venues:
  binance:
    ws_url: "wss://fstream.binance.com/ws"
    symbols: ["BTCUSDT", "ETHUSDT"]
    symbol_map: {"BTCUSDT": "BTC-PERP", "ETHUSDT": "ETH-PERP"}
  okx:
    ws_url: "wss://ws.okx.com:8443/ws/v5/public"
    symbols: ["BTC-USDT-SWAP", "ETH-USDT-SWAP"]
  bybit:
    ws_url: "wss://stream.bybit.com/v5/public/linear"
    symbols: ["BTCUSDT", "ETHUSDT"]
  deribit:
    ws_url: "wss://www.deribit.com/ws/api/v2"
    symbols: ["BTC-PERPETUAL", "ETH-PERPETUAL"]

reconnect_backoff_ms: [1000, 2000, 4000, 8000, 16000]
orderbook_depth: 10
funding_poll_interval_s: 30

redis:
  addr: "localhost:6379"
  output_stream: "market:quotes"
```

---

### Service B: Consensus Price Engine

**Status:** ✅ 95% complete

**What's implemented:** See `internal/consensus/` — full MAD outlier detection, trust model with dynamic penalties, circuit breaker state machine, VWAP-based executable prices, band computation via percentiles, quality scoring, all three output streams.

**Remaining gaps:**
- Mark/Index divergence as a blacklist trigger (Quote struct has these fields but engine ignores them)
- Repeated feed disconnect counter (staleness is tracked; disconnect events are not counted)
- No Postgres persistence for trust history or anomaly log

---

### Service C: Arb Opportunity Engine

**Status:** ✅ 95% complete

**What's implemented:** See `internal/arb/` — quality gating, venue health filtering, size ladder iteration, latency-buffered net edge calculation, both-direction pair checking, `pickBest` (best + one disjoint pair), in-memory cooldown, observability counters.

**Remaining gaps:**
- No depth-based hedge preference (currently always `SIMULTANEOUS_OR_HEDGE_FIRST`; bid/ask depth fields exist in `VenueMetrics` but are not used to choose sequencing)
- No integration with Capital Allocator (intents flow to `trade:intents` but no consumer)

---

### Service D: Funding Engine

**Status:** ❌ Not started — V1 required

**Runs as:** `cmd/funding-engine/`

**Inputs:** `market:quotes` (funding rate field), optionally `consensus:updates`

**Outputs:** `trade:intents` (strategy: `FUNDING_CARRY` or `FUNDING_DIFFERENTIAL`)

**Responsibilities:**
- Rank venues by net funding yield after fees
- Strategies:
  1. **Classic carry (spot/perp hedge):** Buy spot, short perp on same venue. Collect funding. Requires spot balance or DeFi borrow.
  2. **Cross-venue funding differential:** Long perp on low-funding venue, short perp on high-funding venue. Delta-neutral.
  3. **Funding regime model (V1 simple):** Trend + OI proxy + momentum. Gate strategy size by regime confidence.
- Emit intents only when annualised net yield > configured threshold

**Config file:** `configs/policies/funding_engine.yaml`
```yaml
symbols:
  - BTC-PERP
  - ETH-PERP

# Minimum annualised funding yield to enter (net of fees)
min_annual_yield_pct:
  HIGH: 8.0
  MED: 12.0

# Maximum position size for funding strategies
max_notional_usd: 50000

# Minimum funding rate differential between venues for cross-venue strategy
min_differential_bps_per_8h: 5.0

# Rebalance intent if hedge drifts beyond this notional
hedge_drift_unwind_usd: 500

# Simple regime gate: if 24h volatility > threshold, reduce size by factor
volatility_gate:
  vol_threshold_pct: 5.0
  size_reduction_factor: 0.5

venues:
  - binance
  - okx
  - bybit
  - deribit
```

**Key output fields in TradeIntent (funding-specific):**
```json
{
  "strategy": "FUNDING_CARRY",
  "symbol": "BTC-PERP",
  "legs": [
    {"venue": "binance", "action": "BUY",  "market": "SPOT", "notional_usd": 10000},
    {"venue": "binance", "action": "SELL", "market": "PERP", "notional_usd": 10000}
  ],
  "expected": {
    "funding_rate_8h_bps": 3.5,
    "annual_yield_pct_net": 11.2,
    "fees_usd_est": 8.0
  }
}
```

---

### Service E: Liquidity Inefficiency Engine

**Status:** ❌ Not started — V2

**Inputs:** `market:quotes` (needs depth + trade prints), `consensus:updates`

**Outputs:** `trade:intents` (strategy: `SPREAD_CAPTURE` or `LIQUIDATION_CONTRA`)

**Trigger signals:**
- Thin-book detection: `ask_depth_1pct < threshold` or `bid_depth_1pct < threshold`
- Spread blowout: spread > N × normal spread (rolling baseline)
- Mark/index divergence on a single venue
- Imbalance: bid_depth / ask_depth > X or < 1/X
- Liquidation cascade proxy: fast 1-minute move + spread widening + depth collapse simultaneously

**Safety gates (strict):** Consensus quality must be HIGH. Risk daemon must be RUNNING. Venue must be OK. These signals can occur simultaneously with bad data — the consensus layer is the guard.

---

### Service F: Capital Allocator

**Status:** ❌ Not started — V1 required

**Runs as:** `cmd/capital-allocator/`

**Inputs:**
- `trade:intents` (from all strategy engines)
- Positions/balances state (from Ledger service)
- Risk state (from Risk Daemon)

**Outputs:** `trade:intents:approved` (approved and size-adjusted intents)

**Responsibilities:**
- Act as the traffic controller between strategy engines and the Execution Router
- For each incoming intent:
  1. Check risk daemon state is RUNNING
  2. Check per-strategy notional cap not exceeded
  3. Check per-venue notional cap not exceeded
  4. Check available margin on relevant venues
  5. Size-adjust if necessary (scale down to fit caps)
  6. Approve or reject with reason
- Allocate capital across competing intents by expected net return × fill probability × risk score
- Throttle all intents under poor conditions (LOW quality, high volatility)

**Config file:** `configs/policies/allocator.yaml`
```yaml
per_strategy_max_usd:
  CROSS_VENUE_ARB: 100000
  FUNDING_CARRY: 200000
  FUNDING_DIFFERENTIAL: 150000
  SPREAD_CAPTURE: 50000
  LIQUIDATION_CONTRA: 25000

per_venue_max_usd:
  binance: 200000
  okx: 150000
  bybit: 150000
  deribit: 100000

# Gate all new intents when margin utilisation exceeds this on any venue
margin_utilisation_gate_pct: 75.0

# Gate by consensus quality
min_quality_for_arb: MED
min_quality_for_funding: MED
min_quality_for_liquidity: HIGH
```

---

### Service G: Execution Router

**Status:** ❌ Not started — V1 required

**Runs as:** `cmd/execution-router/`

**Inputs:** `trade:intents:approved`

**Outputs:** `execution:events`, `position:updates`, `order:updates`

**Responsibilities:**
- Place orders on exchanges using stored API credentials
- Respect ALL constraints in the intent: `max_slippage_bps`, `expires_ms`, `price_limit`, `max_age_ms`
- Hedge sequencing per intent's `hedge_preference`:
  - `SIMULTANEOUS_OR_HEDGE_FIRST`: attempt simultaneous; fall back to hedge-first
  - If leg A fills and leg B fails: immediately attempt to unwind leg A or hedge with best available
- Partial fill handling:
  - If leg fills partially: adjust leg B size to match
  - If re-hedge fails: emit `HEDGE_FAILED` alert, enter SAFE MODE
- Order lifecycle:
  - NEW → PENDING_SUBMIT → SUBMITTED → PARTIAL | FILLED | REJECTED | EXPIRED
  - Every state transition is written to `execution:events` and the audit log
- Hard limits:
  - Max 3 retries per leg
  - Must abandon and unwind if `expires_ms` is exceeded
  - Never submit if kill switch is active

**Key data contract:**
```json
{
  "event_type": "ORDER_FILLED",
  "intent_id": "uuid",
  "leg_index": 0,
  "venue": "binance",
  "symbol": "BTC-PERP",
  "action": "BUY",
  "requested_notional_usd": 10000,
  "filled_notional_usd": 10000,
  "filled_price": 99892.5,
  "slippage_bps_actual": 2.1,
  "slippage_bps_allowed": 8.0,
  "fees_usd_actual": 3.99,
  "ts_ms": 1700000000789,
  "latency_signal_to_fill_ms": 145
}
```

**Partial fill and hedge drift management:**
- Hedge drift = notional of unhedged leg × time exposed
- Emit `HEDGE_DRIFT` alert when drift exceeds configured threshold
- Risk Daemon subscribes to this and can trigger SAFE MODE

---

### Service H: Risk Daemon

**Status:** ❌ 5% — V1 required (kill switch key exists; no daemon logic)

**Runs as:** `cmd/risk-daemon/`

**Inputs:** Positions truth pulls (REST polls), `execution:events`, `venue_status_updates`, `consensus:updates`

**Outputs:** `risk:state`, `risk:alerts`, control actions (PAUSE / SAFE_MODE / FLATTEN)

**Continuous risk metrics:**

| Metric | Threshold (example) | Action |
|---|---|---|
| Net delta per asset | > $5,000 | WARN |
| Margin utilisation per venue | > 70% | WARN; > 85% SAFE MODE |
| Liquidation distance | < 20% | SAFE MODE |
| Hedge drift (unhedged notional × seconds) | > 500 USD×s | SAFE MODE |
| Feed staleness / venue blacklist count | ≥ 2 core venues | SAFE MODE |
| Drawdown from peak equity | > 3% | PAUSE; > 5% SAFE MODE |
| Execution error rate (rolling 5m) | > 10% | PAUSE |
| Reconciliation divergence | > $50 | PAUSE + alert |

**Control modes:**

```
RUNNING  ──────────────────────► PAUSED
    │                              │  (no new trades)
    │                              │
    ▼                              ▼
SAFE MODE ◄────────────── (hedge maintenance only)
    │
    ▼
FLATTEN ALL
    │  1. Cancel open orders
    │  2. Close perp positions (market)
    │  3. Restore delta neutrality
    │  4. Pull margin buffers to cold wallet
    │  5. Repay DeFi borrow (if enabled)
    │  6. Halt
    ▼
HALTED
```

**Independence requirement:** The Risk Daemon must be able to execute FLATTEN independently — it must NOT depend on the Execution Router being alive. It connects directly to exchange REST APIs for emergency order placement.

**Config file:** `configs/policies/risk.yaml`
```yaml
max_net_delta_usd: 5000
max_margin_utilisation_pct: 70.0
safe_mode_margin_utilisation_pct: 85.0
min_liquidation_distance_pct: 20.0
max_hedge_drift_usd_seconds: 500
max_drawdown_pct: 3.0
safe_mode_drawdown_pct: 5.0
max_error_rate_5m_pct: 10.0
max_reconciliation_divergence_usd: 50.0
min_core_venues_for_safe_mode: 2
position_truth_poll_interval_s: 30
```

---

### Service I: Ledger + Reconciliation Service

**Status:** ❌ Not started — V1 required (basic version)

**Runs as:** `cmd/ledger/`

**Inputs:** All event streams (`execution:events`, `position:updates`, funding payments, transfers, alerts)

**Outputs:** PnL snapshots, discrepancy alerts, reporting API endpoints

**Responsibilities:**
- Immutable append-only record of every significant event
- Compute (per tenant, per venue, per strategy):
  - Realised PnL
  - Unrealised PnL (mark-to-consensus)
  - Funding income attribution
  - Fee cost attribution
- Periodic truth pull reconciliation:
  - Compare internal position/balance state vs exchange REST API truth
  - Flag divergences and emit alert
- Paper trading PnL tracked separately with same schema under `PAPER_` prefix

**Postgres schema (minimum tables):**
```sql
-- Core accounting
CREATE TABLE trade_intents      (id uuid, tenant_id text, strategy text, symbol text, payload jsonb, ts timestamptz);
CREATE TABLE orders             (id uuid, intent_id uuid, venue text, symbol text, action text, status text, payload jsonb, ts timestamptz);
CREATE TABLE fills              (id uuid, order_id uuid, price float8, notional float8, fees float8, slippage_bps float8, ts timestamptz);
CREATE TABLE positions_snapshots(id uuid, tenant_id text, venue text, symbol text, notional float8, entry_price float8, unrealised_pnl float8, ts timestamptz);
CREATE TABLE balances_snapshots (id uuid, tenant_id text, venue text, asset text, total float8, available float8, ts timestamptz);
CREATE TABLE funding_payments   (id uuid, tenant_id text, venue text, symbol text, amount_usd float8, rate_bps float8, ts timestamptz);

-- Risk and control
CREATE TABLE risk_state_snapshots (id uuid, tenant_id text, mode text, metrics jsonb, ts timestamptz);
CREATE TABLE alerts               (id uuid, tenant_id text, source text, severity text, message text, payload jsonb, ts timestamptz);
CREATE TABLE venue_status_history (id uuid, tenant_id text, venue text, symbol text, from_state text, to_state text, reason text, ts timestamptz);
CREATE TABLE audit_log            (id uuid, tenant_id text, actor text, action text, payload jsonb, ts timestamptz);

-- Transfers and compliance
CREATE TABLE rebalance_proposals (id uuid, tenant_id text, status text, payload jsonb, proposed_at timestamptz, approved_at timestamptz);
CREATE TABLE transfers           (id uuid, tenant_id text, proposal_id uuid, asset text, amount float8, destination text, tx_hash text, status text, ts timestamptz);
CREATE TABLE config_hashes       (id uuid, tenant_id text, file_name text, sha256 text, ts timestamptz);

-- DeFi
CREATE TABLE defi_positions      (id uuid, tenant_id text, protocol text, collateral_usd float8, borrow_usd float8, health_factor float8, ts timestamptz);

-- Multi-tenancy
CREATE TABLE tenants             (id text PRIMARY KEY, name text, config jsonb, created_at timestamptz);
CREATE TABLE venues              (id text PRIMARY KEY, name text, region text, is_approved bool, config jsonb);
```

All tables include `tenant_id` for multi-tenancy isolation. All event writes are INSERT-only (no UPDATE/DELETE for immutability).

---

### Service J: Rebalance + Transfer Service

**Status:** ❌ Not started — V2

**Inputs:** Balances/margin requirements + policies

**Outputs:** `rebalance:proposals`, `transfer:events`, alerts

**Institutional-grade transfer safety:**

**Strict allowlist:**
```yaml
# configs/policies/address_book.yaml
addresses:
  - id: "binance-hot"
    asset: "USDT"
    chain: "TRX"
    destination: "binance"
    address: "TXxxxxxxxxxxxxxxxxxxxxxx"
    memo: ""
    max_single_transfer_usd: 50000
  - id: "cold-wallet-1"
    asset: "BTC"
    chain: "BTC"
    destination: "self-custody"
    address: "bc1qxxxxxxxxxxxxxxxx"
    memo: ""
    max_single_transfer_usd: 100000
```

No runtime addresses ever. Only pre-approved entries.

**Policy tamper detection:**
- Hash `address_book.yaml`, `venue_registry.yaml`, `transfer_policy.yaml` on startup
- Store hash in `config_hashes` Postgres table
- If hash changes without explicit admin action: disable all withdrawals and emit CRITICAL alert

**Velocity limits (per config):**
```yaml
per_transfer_cap_usd: 50000
daily_cap_usd: 200000
transfers_per_day: 10
new_address_cooloff_hours: 72
```

**V1: Manual approval required for every transfer.**
Approve via dashboard with 2FA confirmation. Full audit record for every approval/rejection.

---

### Service K: Collateral Manager (DeFi Looping)

**Status:** ❌ Not started — V2

**Protocols:** Aave V3, Compound V3 (conservative selection)

**Target:** 40–50% borrow ratio. Hard cap: 60%. Health factor target: ≥ 2.0.

**Auto-deleverage triggers:**
- Health factor < 1.5 → repay 10% of borrow
- Health factor < 1.3 → repay to target
- Risk daemon FLATTEN → repay all and withdraw collateral

**Integration with kill switch:** Unwind path must include DeFi repayment in the flatten sequence.

---

### Service L: Gateway API

**Status:** ❌ 10% — V1 required

**Current:** Dashboard exposes kill switch, credential storage, alert config, and real-time SSE feed.

**Missing endpoints (to add):**

```
# Positions and PnL
GET  /api/positions                    # Current positions by venue + symbol
GET  /api/pnl                          # PnL summary (today, week, all-time)
GET  /api/pnl/attribution              # PnL broken down by strategy + venue
GET  /api/equity-curve                 # Historical equity snapshots

# Opportunities
GET  /api/intents                      # Recent intents (approved + rejected)
GET  /api/intents/rejected             # Rejected intents with reasons

# Execution
GET  /api/orders                       # Recent orders
GET  /api/orders/{id}                  # Single order detail + fill events

# Funding
GET  /api/funding/rates                # Current funding rates by venue
GET  /api/funding/pnl                  # Funding attribution

# Risk
GET  /api/risk/state                   # Current risk daemon state + metrics
GET  /api/risk/history                 # Historical risk snapshots

# Rebalancing (V2)
GET  /api/rebalance/proposals          # Pending proposals
POST /api/rebalance/proposals/{id}/approve
POST /api/rebalance/proposals/{id}/reject

# Transfers (V2)
GET  /api/transfers                    # Transfer history
POST /api/transfers/{id}/approve       # With 2FA

# Audit
GET  /api/audit                        # Immutable audit log

# System mode
GET  /api/mode                         # Current system mode
POST /api/mode/pause                   # Pause trading
POST /api/mode/safe                    # Safe mode
POST /api/mode/flatten                 # Flatten all

# Paper trading toggle
GET  /api/paper/mode                   # demo | shadow | live
PUT  /api/paper/mode                   # Switch mode
GET  /api/paper/metrics                # Paper trading KPI metrics
```

---

## 3. Paper Trading — full specification

### 3.1 Modes

**Mode A — Pure simulation (default for new users)**
- Uses live market data from the full pipeline
- Fills simulated at:
  ```
  fill_buy  = consensus_mid × (1 + sim_slippage_bps/10000)
  fill_sell = consensus_mid × (1 - sim_slippage_bps/10000)
  ```
- Latency model: configurable `sim_latency_ms` delay between signal and fill check
- During the latency window: check if price moved adversely beyond `adverse_selection_bps`; if so, count as adverse selection event
- No real orders ever submitted

**Mode B — Shadow trading (recommended "confidence mode")**
- Full pipeline runs: real signals → intents → execution decisions
- Execution Router writes "simulated orders" instead of real exchange calls
- Compares simulated fill price against live market at fill time
- Best for validating timing, latency, and logic before going live
- Particularly useful for detecting: would the intent have expired before we got there?

**Mode C — Exchange testnet (optional)**
- Where supported (Binance Testnet, Deribit Testnet)
- Useful for API validation; market realism may be limited

### 3.2 Paper trading Redis namespaces

```
demo:positions:{tenant_id}:{symbol}     # current virtual position
demo:fills:{tenant_id}                  # Redis stream of simulated fills
demo:pnl:{tenant_id}                    # Redis stream of P&L snapshots
demo:metrics:{tenant_id}               # KPI metrics (Sharpe, win rate, etc.)

live:positions:{tenant_id}:{symbol}     # production equivalent
live:fills:{tenant_id}
live:pnl:{tenant_id}
```

`TRADING_MODE=demo|shadow|live` env var controls which path the Execution Router uses.

### 3.3 Paper trading truth model

Every simulated fill must record:

```json
{
  "intent_id": "uuid",
  "strategy": "CROSS_VENUE_ARB",
  "symbol": "BTC-PERP",
  "ts_signal_ms": 1700000000123,
  "ts_fill_simulated_ms": 1700000000268,
  "latency_ms": 145,
  "edge_at_signal_bps": 14.2,
  "edge_at_fill_bps": 11.8,
  "edge_captured_bps": 11.8,
  "adverse_selection_occurred": false,
  "fill_price_buy": 99891.0,
  "fill_price_sell": 100095.0,
  "fees_assumed_usd": 8.0,
  "slippage_assumed_bps": 4.0,
  "net_pnl_usd": 20.4,
  "intent_expired": false,
  "mode": "SHADOW"
}
```

### 3.4 KPI metrics (must implement)

| Metric | Definition |
|---|---|
| Sharpe proxy | Mean(daily PnL) / StdDev(daily PnL) × √252 |
| Win rate | % of fills with net_pnl_usd > 0 |
| Avg edge captured vs predicted | Mean(edge_captured_bps / edge_at_signal_bps) |
| Fill ratio | % of intents that reached simulated fill before expiry |
| Adverse selection rate | % of fills where price moved > X bps against position within 1s of fill |
| Venue anomaly avoidance rate | % of updates where a BLACKLISTED/OUTLIER venue would have been used if not filtered |
| Missed edge due to latency | Mean(edge_at_signal_bps - edge_at_fill_bps) when > 0 |
| Slippage sensitivity | Scatter of actual vs assumed slippage |

### 3.5 Onboarding path (new users)

```
Week 1–2: Pure simulation (Mode A)
    Requirements to advance:
    ├── Max drawdown < 2%
    ├── Hedge drift never exceeded 60s
    ├── No policy violations (no rejected intents due to risk limits)
    └── Fill ratio > 70%

Week 3: Shadow trading (Mode B)
    Requirements to advance:
    ├── Sharpe proxy > 1.0
    ├── Win rate > 55%
    └── Adverse selection rate < 15%

Week 4+: Live small
    ├── Max order size: $2,000 per leg
    ├── Max daily notional: $20,000
    └── Elevated after 2 weeks clean live performance

Full live:
    └── Standard position limits from allocator config
```

---

## 4. Dashboard — target state

### 4.1 Current state vs target

| Page / Component | Current | Target |
|---|---|---|
| Real-time consensus feed (SSE) | ✅ Done | ✅ |
| Kill switch control | ✅ Done | ✅ |
| Exchange credentials | ✅ Done | ✅ |
| Alert webhook config | ✅ Done | ✅ |
| PAPER / LIVE mode toggle | ❌ | Required V1 |
| System mode badge (RUNNING / PAUSED / SAFE / FLATTENING) | ❌ | Required V1 |
| Overview: equity curve | ❌ | Required V1 |
| Overview: daily/weekly PnL | ❌ | Required V1 |
| Overview: top strategies + capital | ❌ | Required V1 |
| Positions & Exposure: by venue/symbol | ❌ | Required V1 |
| Positions & Exposure: net delta per asset | ❌ | Required V1 |
| Positions & Exposure: hedge drift monitor | ❌ | Required V1 |
| Opportunities: live ranked intents | ❌ | Required V1 |
| Opportunities: rejection reasons | ❌ | Required V1 |
| Execution: orders/fills with slippage | ❌ | Required V1 |
| Execution: latency metrics | ❌ | Required V1 |
| Funding: rates by venue | ❌ | Required V1 |
| Funding: PnL attribution | ❌ | Required V1 |
| Paper trading: confidence score | ❌ | Required V1 |
| Paper trading: missed edge / slippage sensitivity | ❌ | Required V1 |
| Rebalancing: venue balances + proposals | ❌ | V2 |
| DeFi Collateral: health factor + deleverage log | ❌ | V2 |
| Alerts & Audit: full event timeline | ❌ | Required V1 |

### 4.2 Dashboard tech stack

**Current:** Single `index.html` embedded as Go constant in `frontend.go`.

**Target V1:** React (or Next.js) with Tailwind. Communicates via existing SSE stream + Gateway API REST endpoints. Toggle between PAPER and LIVE data views. System mode badge always visible in top bar.

---

## 5. Institutional controls — non-negotiable checklist

### 5.1 Trade gating (every intent must pass ALL gates before execution)

- [ ] Consensus quality ≥ configured minimum
- [ ] All leg venues: status = OK (or WARN if explicitly allowed per strategy)
- [ ] Risk daemon state = RUNNING
- [ ] Net expected edge > min_edge_bps_net[quality] after fees + latency buffers
- [ ] Hedge drift risk within limits
- [ ] Notional ≤ per-strategy cap AND per-venue cap
- [ ] Cooldown elapsed for this symbol+pair
- [ ] Intent not expired (ts_ms + ttl_ms > now)
- [ ] Kill switch not active

### 5.2 Automatic circuit breakers

| Trigger | Action |
|---|---|
| Venue deviation > 50bps for > 1500ms | BLACKLIST venue for 60s |
| Feed stale > 750ms | Exclude from consensus; staleness penalty on trust |
| 2+ core venues BLACKLISTED simultaneously | System → SAFE MODE |
| Spread > 25bps on a venue | Trust penalty |
| Execution error rate > 10% over 5m | PAUSE |
| Reconciliation divergence > $50 | PAUSE + alert |
| Hedge drift > threshold | SAFE MODE |
| Drawdown > 3% from peak | PAUSE |
| Drawdown > 5% from peak | SAFE MODE |

### 5.3 Kill switch modes (three levels)

```
POST /api/kill           → PAUSE:      No new intents. Existing orders can complete.
POST /api/mode/safe      → SAFE MODE:  Only hedge maintenance. No new strategies.
POST /api/mode/flatten   → FLATTEN:    Full unwind sequence (see Risk Daemon spec).
```

Flatten sequence (always executed by Risk Daemon independently):
1. Cancel all open orders across all venues
2. Close all perp positions (market orders, accept slippage)
3. Restore delta neutrality on any residual spot imbalance
4. Pull margin back toward configured reserve levels
5. Repay DeFi borrow (if Collateral Manager enabled)
6. Set system mode to HALTED
7. Write audit record for every action

---

## 6. V1 → V3 Roadmap

### V1 — Close the money loop (current priority)

**Goal:** End-to-end flow: live data → consensus → arb/funding intents → paper execution → PnL

| # | Component | Blocks |
|---|---|---|
| 1 | **Market Data Service** (exchange WS connectors) | Everything |
| 2 | **Paper Trading Service** (pure simulation) | Dashboard PnL; user trust |
| 3 | **Ledger** (basic Postgres + append-only events) | PnL; reconciliation |
| 4 | **Docker Compose** (full local stack) | Dev velocity; demos |
| 5 | **Funding Engine** (basic carry + differential) | Yield expansion |
| 6 | **Execution Router** (two-leg safe execution, paper mode first) | Live trading |
| 7 | **Risk Daemon** (continuous metrics + PAUSE/SAFE/FLATTEN) | Safety |
| 8 | **Capital Allocator** (basic caps + quality gating) | Position sizing |
| 9 | **Gateway API** (positions, PnL, intents, risk endpoints) | Dashboard |
| 10 | **Dashboard V1 rebuild** (equity, positions, opportunities, paper metrics) | User trust |

### V2 — Yield expansion + automation

- Liquidity Inefficiency Engine (spread capture + liquidation contra)
- Smarter execution: IOC/limit choice, depth-sensitive sequencing
- Automated small rebalances with strict policy enforcement
- Funding regime forecasting (OI + momentum model)
- Full Postgres migration for all historical data
- Prometheus + Grafana observability stack

### V3 — Institutional polish + white-label readiness

- Multi-tenant UI branding + isolated API keys per tenant
- RBAC: admin / trader / viewer / auditor roles
- Advanced reporting: client-ready PDF/CSV exports
- Optional DEX spot leg routing via 1inch/Paraswap (with MEV protection)
- Optional L2 transfers (Arbitrum, Optimism) for gas efficiency
- SOC2-style audit trail export

---

## 7. Deployment stack

### V1 target: Docker Compose

Missing: `docker-compose.yaml`

Required services:
```yaml
services:
  redis:          # Redis 7, streams enabled
  postgres:       # Postgres 16
  market-data:    # cmd/market-data
  consensus:      # cmd/consensus-engine     (Dockerfile.consensus-engine exists)
  arb-engine:     # cmd/arb-opportunity-engine
  funding-engine: # cmd/funding-engine
  execution:      # cmd/execution-router
  risk-daemon:    # cmd/risk-daemon
  ledger:         # cmd/ledger
  paper-trader:   # cmd/paper-trader
  allocator:      # cmd/capital-allocator
  gateway:        # cmd/gateway (or extend dashboard)
  dashboard:      # cmd/dashboard             (Dockerfile exists)
```

### Environment separation

| Environment | Trading mode | Description |
|---|---|---|
| `local` | `demo` | Docker Compose, paper only, fake data OK |
| `staging` | `shadow` | Real exchange WebSocket feeds, shadow execution |
| `production` | `live_small` then `live` | Strict mode, all controls enforced |

---

## 8. Next two specs to produce (in priority order)

### Spec priority 1: Execution Router

Two-leg safe execution with:
- Partial fill handling and leg re-hedging
- Hedge drift tracking and emergency unwind
- Kill switch hooks
- Audit record for every order action
- Paper mode flag (writes simulated orders instead of real)

### Spec priority 2: Paper Trading & Shadow Execution

Fill simulator with:
- Latency model
- Adverse selection detection
- Confidence metric computation
- Dashboard KPI outputs
- Onboarding graduation criteria enforcement

---

*This document is the living spec for ArbSuite. All service specs above are coding-LLM-ready.*
