# Consensus Engine — Complete Project Breakdown

## Project Overview
**consensus-engine** is a multi-venue cryptocurrency price consensus service built in Go 1.24. It aggregates real-time price quotes from exchanges (Binance, OKX, Bybit, Deribit, HTX, Gate), computes trust-weighted consensus prices, detects venue anomalies via circuit-breaker logic, and powers an arbitrage/funding-rate trading pipeline. Runs as microservices over Redis Streams with a single-page dashboard on port 8080.

---

## What We Built (Sessions 1–3)

### A. Funding Rate Carry Strategy Hardening

**Fix #1 — Round-Trip Cost Yield Calculation** (`internal/funding/engine.go`)
- Yield calculations now deduct round-trip costs (entry + exit fees + slippage) before comparing against thresholds
- `netYield = grossYield - (2 * feeRate + slippageBps/10000)`

**Fix #2 — OKX Funding Rate Timestamp** (`internal/marketdata/okx.go`)
- Corrected timestamp parsing so stale-rate detection no longer misfires on OKX

**Fix #3 — Event-Driven Exit Evaluation** (`internal/funding/engine.go`, `cmd/funding-engine/main.go`)
- Added `ExitSignalC chan struct{}` (buffered 1) to Engine
- `signFlipped(a, b float64) bool` — detects positive↔negative funding rate inversions
- `UpdateQuote` fires signal on rate sign change
- Main loop has `case <-engine.ExitSignalC:` for immediate exit evaluation instead of waiting for periodic timer

**Maker/Taker Fee Differentiation** (`internal/execution/`)
- Paper trading simulator distinguishes maker vs taker fees

**Test Suite** (`internal/funding/engine_test.go`)
- 32 tests, all passing
- Fixed 3 flaky tests (yield threshold, time-dependent scheduler, rate differential)
- Added 2 new tests for exit signal channel

**Commit**: `fix: harden funding carry strategy for automated paper trading`

---

### B. Strategy Management Backend

**New file: `internal/dashboard/strategy.go`** (406 lines)

Types:
```go
type StrategyConfig struct {
    ID          string            `json:"id"`
    Name        string            `json:"name"`
    Description string            `json:"description"`
    Enabled     bool              `json:"enabled"`
    Venues      []string          `json:"venues"`       // per-strategy exchange selection
    CapitalUSD  float64           `json:"capital_usd"`  // locked capital
    Params      map[string]string `json:"params"`       // strategy-specific tuning params
    Stage       string            `json:"stage"`        // OBSERVE/PAPER/CONSERVATIVE/FULL
    UpdatedMs   int64             `json:"updated_ms"`
}

type StrategyLogEntry struct {
    TsMs     int64  `json:"ts_ms"`
    Level    string `json:"level"`    // INFO, WARN, ERROR, TRADE
    Strategy string `json:"strategy"`
    Message  string `json:"message"`
}
```

5 default strategies:

| ID | Name | Default Venues |
|---|---|---|
| `funding_carry` | Funding Rate Carry | binance |
| `funding_reverse` | Reverse Carry | binance |
| `funding_differential` | Funding Differential | binance, okx |
| `cross_venue_arb` | Cross-Venue Arbitrage | binance, okx, bybit |
| `basis_convergence` | Basis Convergence | binance, deribit |

Redis-backed store methods:
- `GetStrategies` / `GetStrategy` / `SaveStrategy` — CRUD on `strategy:config:{id}` keys
- `GetTotalLockedCapital` — sum across enabled strategies
- `AppendStrategyLog` / `GetStrategyLogs` — Redis streams per strategy (capped 1000)

6 API endpoints:

| Method | Path | Auth | Purpose |
|---|---|---|---|
| GET | `/api/strategies/config` | viewer+ | List all |
| GET | `/api/strategies/config/{id}` | viewer+ | Get one |
| PUT | `/api/strategies/config/{id}` | trader+ | Update config |
| POST | `/api/strategies/{id}/toggle` | trader+ | Enable/disable |
| GET | `/api/strategies/{id}/logs` | viewer+ | Strategy logs |
| GET | `/api/strategies/capital` | viewer+ | Capital summary |

---

### C. Complete Dashboard UI Overhaul

**File: `internal/dashboard/static/index.html`** (2,736 lines, embedded via `go:embed`)

**Design**: Gaming-launcher inspired (Steam/Battle.net/Riot). Dark theme (`#0a0e1a`), cyan accent (`#00d4ff`), glassmorphism cards, collapsible sidebar, pulse-glow animations.

**15 pages** with sidebar navigation:

| Page | Content |
|---|---|
| **Strategy Hub** (default landing) | Capital summary bar (total/deployed/available/active), strategy card grid with enable toggle, stage badge, venue pills, allocation bar, configure button |
| **Dashboard** | Risk mode card with PAUSE/SAFE/FLATTEN/RESUME, trading mode card (PAPER/SHADOW/LIVE with graduation guardrails), manual trade deployment, PnL summary, risk cockpit RAG grid, confidence meter, live price feed table, recent activity |
| **Live Feed** | Consensus summary with metrics (consensus price, confidence, venues, quality), per-venue table (bid/ask/mid/spread/deviation/state), anomaly list |
| **P&L** | KPI metrics row (total PnL, fills, win rate, Sharpe, avg slippage), equity curve SVG chart, fills table |
| **Positions** | Open positions table (symbol, venue, side, size, entry, mark, PnL, age) |
| **Funding** | Funding rates table (venue, symbol, rate, annualized, next reset, trend) |
| **Orders** | Orders/intents table (time, symbol, venues, side, notional, edge, status) |
| **Timeline** | Chronological activity feed (arb fills, anomalies, mode changes, kills, manual trades) |
| **Risk** | Risk state machine visualization, risk history timeline, risk alerts |
| **Connections** | Per-exchange API credential cards (configure/mask), connection status dots |
| **Kill Switch** | Kill switch status display, activate with reason, deactivate, pulse-red animation when active |
| **Alerts** | Webhook URL config, toggle conditions (quality low, venue blacklisted, anomaly high/medium), deviation threshold, test button, alert log |
| **Audit** | Audit log table with export CSV, filtered by date range |
| **Config** (admin) | Funding stage management per symbol (OBSERVE→PAPER→CONSERVATIVE→FULL), branding customization |
| **API Keys** (admin) | Create/delete API keys with role assignment |

**Strategy detail overlay** (slide-in panel with 3 tabs):
- **Config tab**: Enable toggle, stage selection (4 buttons), venue checkbox grid, capital allocation slider, strategy-specific parameter editor, save button
- **Logs tab**: Real-time log viewer (INFO/WARN/ERROR/TRADE levels, color-coded)
- **Performance tab**: PnL, fill count, win rate stats

**78 JavaScript functions** covering:
- Auth (login, token storage, role-based visibility)
- SSE (real-time event stream with reconnect)
- Navigation (sidebar page switching with data loading per page)
- Strategy management (load, render cards, toggle, open detail, save config, load logs)
- All data page loaders (mode, trading mode, PnL, equity curve, positions, funding, orders, timeline, risk, connections, alerts, kill, audit, API keys, stages, branding)
- Client-side price fetcher (Binance, Bybit, OKX, Deribit REST APIs for initial price display)
- Utility functions (fmtPrice, fmtUSD, fmtTime, escHtml, setRAG, statusBadge, etc.)

**Commit**: `feat: add strategy management backend and gaming-inspired dashboard UI`

---

## Full API Surface

The dashboard server exposes these API groups:

**Core** (server.go): `/`, `/metrics`, `/api/events` (SSE), `/api/connections`, `/api/alerts`, `/api/kill`, `/api/config/stages`

**Gateway** (gateway.go, 100+ endpoints):
- Risk: `/api/mode`, `/api/risk/state`, `/api/risk/history`, `/api/risk/alerts`
- Trading: `/api/paper/mode`, `/api/paper/equity`, `/api/paper/confidence`, `/api/trade/manual`
- Data: `/api/prices`, `/api/pnl`, `/api/pnl/attribution`, `/api/pnl/by-venue`, `/api/pnl/by-strategy`, `/api/metrics/kpi`
- Positions: `/api/positions`, `/api/intents`, `/api/orders`
- Funding: `/api/funding/rates`
- Analytics: `/api/equity-curve`, `/api/timeline`
- Auth: `/api/auth/me`, `/api/auth/keys`
- Reports: `/api/reports/fills`, `/api/reports/pnl`, `/api/reports/tax/uk/*`
- Audit: `/api/audit`, `/api/audit/export`
- Branding: `/api/tenant/branding`
- Advanced (not yet in UI): `/api/pipeline/latency`, `/api/regime`, `/api/opportunities`, `/api/opportunities/missed`, `/api/inventory`, `/api/slippage-curves`, `/api/leader-stats`, `/api/optimizer/params`, `/api/venue-scores`, `/api/yield/*`, `/api/onchain/*`, `/api/bridge/*`, `/api/keeper/*`, `/api/dex/*`, `/api/defi/*`, `/api/maker-rebate/*`, `/api/triangular/*`

**Strategy** (strategy.go): `/api/strategies/config`, `/api/strategies/{id}/toggle`, `/api/strategies/{id}/logs`, `/api/strategies/capital`

---

## Architecture

```
Exchange WebSockets → market-data → Redis Stream (market:quotes)
  → consensus-engine → Redis Streams (consensus:updates/anomalies/status)
    → arb-opportunity-engine → Redis Stream (trade:intents)
    → funding-engine → Redis Stream (trade:intents)
      → capital-allocator → Redis Stream (trade:intents:approved)
        → execution-router → Redis Stream (execution:events, demo:fills)
  → risk-daemon (monitors all streams, can activate kill:switch)
  → ledger (persists all events to Postgres)
  → dashboard (serves web UI + Gateway API on :8080)
```

### Key Redis Streams

| Stream | Producer | Consumer(s) |
|---|---|---|
| `market:quotes` | market-data | consensus-engine |
| `consensus:updates` | consensus-engine | arb-engine, funding-engine, dashboard |
| `consensus:anomalies` | consensus-engine | risk-daemon, dashboard |
| `consensus:status` | consensus-engine | ledger, dashboard |
| `trade:intents` | arb-engine, funding-engine | capital-allocator |
| `trade:intents:approved` | capital-allocator | execution-router |
| `execution:events` | execution-router | ledger, risk-daemon |
| `risk:state` | risk-daemon | dashboard, ledger |
| `risk:alerts` | risk-daemon | dashboard, ledger |
| `strategy:logs:{id}` | funding-engine, dashboard | dashboard |

### Key Redis Keys

| Key | Purpose |
|---|---|
| `kill:switch` | When set, consensus-engine and execution-router halt |
| `risk:mode` | Current risk daemon mode (RUNNING/PAUSED/SAFE/FLATTEN/HALTED) |
| `trading:mode` | Current trading mode (PAPER/SHADOW/LIVE) |
| `paper:equity` | Paper trading equity state |
| `strategy:config:{id}` | Per-strategy configuration JSON |
| `dashboard:conn:{exchange}` | Encrypted exchange API credentials |
| `dashboard:alert:config` | Alert webhook + threshold config |
| `config:funding:stages` | Per-symbol funding stage overrides |

---

## Repository Structure

```
consensus-engine/
├── cmd/                          # Service entry points
│   ├── arb-opportunity-engine/   # Cross-venue arb detection
│   ├── capital-allocator/        # Kelly criterion position sizing
│   ├── consensus-engine/         # Core trust-weighted price consensus
│   ├── dashboard/                # HTTP API + web UI (:8080)
│   ├── execution-router/         # Paper/shadow/live execution
│   ├── funding-engine/           # Funding rate carry strategies
│   ├── ledger/                   # Postgres event writer
│   ├── liquidity-engine/         # Liquidity analysis
│   ├── market-data/              # WebSocket feed aggregator
│   ├── risk-daemon/              # Risk monitoring + kill switch
│   └── transfer-policy/          # Cross-venue transfer rules
├── internal/
│   ├── allocator/                # Capital allocation engine
│   ├── arb/                      # Arb detection + cooldown
│   ├── auth/                     # RBAC (admin/trader/viewer/auditor)
│   ├── consensus/                # Core: engine, math, trust, circuit breaker
│   ├── dashboard/                # HTTP server, SSE, gateway, strategy mgmt
│   │   ├── server.go             # Routes, auth middleware, CRUD handlers
│   │   ├── gateway.go            # 100+ API handlers for all data
│   │   ├── strategy.go           # Strategy CRUD, capital locking, logs
│   │   ├── sse.go                # Redis→SSE real-time event bridge
│   │   ├── store.go              # Redis config store (connections, alerts, kill)
│   │   ├── metrics.go            # Prometheus text exposition
│   │   ├── alerts.go             # Webhook alert delivery
│   │   ├── frontend.go           # go:embed for static/index.html
│   │   └── static/index.html     # 2,736-line gaming-inspired SPA
│   ├── execution/                # Paper trading simulator (maker/taker fees)
│   ├── funding/                  # Funding engine (carry, reverse, differential)
│   │   ├── engine.go             # ExitSignalC, signFlipped, yield deduction
│   │   └── engine_test.go        # 32 tests
│   ├── marketdata/               # Exchange WebSocket adapters
│   └── ...                       # 20+ other internal packages
├── configs/
│   ├── market_data.yaml          # Exchange WebSocket URLs + symbols
│   └── policies/                 # Per-service YAML configs
├── docker-compose.yaml           # Full local stack
├── Dockerfile.*                  # Per-service multi-stage builds
└── go.mod                        # github.com/ezyjtw/consensus-engine
```

---

## Tech Stack

- **Language**: Go 1.24
- **HTTP**: Standard library `net/http` (Go 1.22+ routing patterns)
- **Message bus**: Redis Streams (`github.com/redis/go-redis/v9`)
- **Database**: PostgreSQL 16 (`github.com/jackc/pgx/v5`) for ledger/audit
- **WebSockets**: `github.com/gorilla/websocket` for exchange feeds
- **Config**: YAML (`gopkg.in/yaml.v3`)
- **Containers**: Multi-stage Docker (golang:1.24-alpine → distroless/static)
- **Frontend**: Single HTML file, vanilla JS, no build tools, embedded via `go:embed`

---

## What Could Come Next

1. **Wire advanced API endpoints into UI** — ~40 endpoints exist (yield overview, on-chain txs, bridge transfers, DEX pools, DeFi risk, triangular arb, maker rebate, pipeline latency, regime state, optimizer params, venue scores, slippage curves, leader stats, keeper stats) without UI pages
2. **Strategy-specific P&L tracking** — Route fills to originating strategy for per-strategy attribution
3. **Strategy templates / cloning** — Create custom strategy instances from templates
4. **Live WebSocket data on strategy cards** — Show real-time funding rates / arb spreads on cards
5. **Capital rebalancing** — Auto-redistribute unallocated capital based on performance
6. **Backtesting** — Historical simulation before enabling strategies
7. **Multi-user collaboration** — Multiple users with different roles managing different strategies
8. **Mobile-responsive refinements** — The CSS has basic responsive breakpoints but could be more polished for mobile
9. **Notification channels** — Telegram/Discord integration beyond just webhooks
10. **Performance dashboards** — Latency percentiles, throughput graphs, system health monitoring
