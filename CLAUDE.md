# CLAUDE.md

## Project Overview

**consensus-engine** is a multi-venue cryptocurrency price consensus service built in Go. It aggregates real-time price quotes from multiple crypto exchanges (Binance, OKX, Bybit, Deribit, etc.), computes trust-weighted consensus prices, detects venue anomalies via circuit-breaker logic, and powers an arbitrage/trading pipeline. The system runs as a set of microservices communicating over Redis Streams.

## Repository Structure

```
consensus-engine/
├── cmd/                          # Service entry points (one main.go per service)
│   ├── arb-opportunity-engine/   # Detects cross-venue arb opportunities
│   ├── capital-allocator/        # Position sizing via Kelly criterion
│   ├── consensus-engine/         # Core: trust-weighted price consensus
│   ├── dashboard/                # HTTP API + web UI (port 8080)
│   ├── execution-router/         # Paper/shadow/live trade execution
│   ├── funding-engine/           # Funding rate arbitrage
│   ├── ledger/                   # Postgres event writer
│   ├── liquidity-engine/         # Liquidity analysis
│   ├── market-data/              # WebSocket feed aggregator
│   ├── risk-daemon/              # Risk monitoring + kill switch
│   └── transfer-policy/          # Cross-venue transfer rules
├── internal/                     # Shared library packages (Go internal convention)
│   ├── allocator/                # Capital allocation engine
│   ├── arb/                      # Arb detection logic + cooldown
│   ├── auth/                     # RBAC (admin/trader/viewer/auditor roles)
│   ├── consensus/                # Core consensus: engine, math, trust, circuit breaker
│   ├── dashboard/                # HTTP server, SSE, gateway API, metrics, frontend
│   ├── dex/                      # DEX routing
│   ├── eventbus/                 # Redis Streams client + service-specific bus wrappers
│   ├── execution/                # Paper trading simulator
│   ├── funding/                  # Funding rate regime detection
│   ├── l2/                       # L2 bridge transfers
│   ├── ledger/                   # Postgres schema + writer
│   ├── liquidity/                # Liquidity scoring engine
│   ├── marketdata/               # Exchange WebSocket adapters (Binance, OKX, Bybit, Deribit)
│   ├── risk/                     # Risk daemon state machine
│   ├── store/                    # In-memory quote/status store
│   └── transfer/                 # Transfer policy engine
├── configs/
│   ├── market_data.yaml          # Exchange WebSocket URLs + symbol mappings
│   ├── execution_router.yaml     # Execution routing config
│   └── policies/                 # Per-service YAML policy files
│       ├── consensus_policy.yaml # Core consensus thresholds + Redis config
│       ├── arb_engine.yaml
│       ├── allocator.yaml
│       ├── funding_engine.yaml
│       ├── risk.yaml
│       ├── liquidity_engine.yaml
│       ├── dex_routing.yaml
│       ├── l2_transfers.yaml
│       └── transfer_policy.yaml
├── scripts/
│   └── smoke-test.sh             # End-to-end validation against running stack
├── .github/workflows/ci.yml      # CI: lint, test, build
├── docker-compose.yaml           # Full local stack (Redis + Postgres + all services)
├── Dockerfile.*                  # Per-service Dockerfiles (multi-stage, distroless)
├── go.mod / go.sum               # Go module: github.com/ezyjtw/consensus-engine
└── ROADMAP.md                    # Feature roadmap
```

## Tech Stack

- **Language**: Go 1.24
- **Module path**: `github.com/ezyjtw/consensus-engine`
- **HTTP**: Standard library `net/http` (Go 1.22+ routing patterns)
- **Message bus**: Redis Streams (via `github.com/redis/go-redis/v9`)
- **Database**: PostgreSQL 16 (via `github.com/jackc/pgx/v5`) for ledger/audit
- **WebSockets**: `github.com/gorilla/websocket` for exchange feeds
- **Config**: YAML files parsed with `gopkg.in/yaml.v3`
- **Containers**: Multi-stage Docker builds (`golang:1.24-alpine` -> `distroless/static-debian12:nonroot`)

## Build & Development Commands

### Build all services
```bash
for cmd in cmd/*/; do
  svc=$(basename "$cmd")
  CGO_ENABLED=0 GOOS=linux go build -trimpath -o "bin/$svc" "./$cmd"
done
```

### Build a single service
```bash
go build -o bin/consensus-engine ./cmd/consensus-engine
```

### Run tests
```bash
go test ./...                          # all tests
go test -race -cover ./...             # with race detector + coverage
go test ./internal/consensus/...       # single package
```

### Lint
```bash
go vet ./...
# CI also runs golangci-lint
```

### Run the full stack locally
```bash
DASHBOARD_MASTER_KEY=$(openssl rand -hex 32) docker compose up
```

### Run smoke tests (requires running stack)
```bash
./scripts/smoke-test.sh
```

### Build Docker images
```bash
docker build -f Dockerfile.consensus-engine -t consensus-engine .
```

## Architecture

### Data Flow

```
Exchange WebSockets → market-data → Redis Stream (market:quotes)
    → consensus-engine → Redis Streams (consensus:updates, consensus:anomalies, consensus:status)
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

### Key Redis Keys

- `kill:switch` — when set, consensus-engine and execution-router halt
- `risk:mode` — current risk daemon mode (RUNNING/REDUCE_ONLY/HALT)
- `consensus:venue_state:{tenant}:{venue}:{symbol}` — cached venue state for restart recovery

### Core Consensus Algorithm (`internal/consensus/`)

1. Collect live quotes from all venues (stale quotes filtered by `stale_ms`)
2. Compute mid-price per venue, then median and MAD across all venues
3. Detect outliers via robust z-score and basis-point deviation thresholds
4. Run circuit-breaker state machine per venue: OK → WARN → BLACKLISTED (with TTL recovery)
5. Compute trust scores: base trust * penalty factors (outlier, staleness, spread, state)
6. Normalize trust weights and produce weighted consensus mid/buy/sell prices
7. Compute confidence bands (P25/P75 of effective prices) with quorum-based widening
8. Publish consensus update, anomalies, and status transitions

### Multi-Tenancy

Services support multi-tenancy via `tenant_id` field on all messages. The default tenant is `"default"`, configured via `TENANT_ID` env var.

## Code Conventions

### Project Layout
- Follows standard Go project layout: `cmd/` for binaries, `internal/` for private packages
- Each service has exactly one `main.go` under `cmd/<service-name>/`
- Shared logic lives in `internal/` packages; business logic is separate from transport

### Naming
- Types use PascalCase (`ConsensusUpdate`, `VenueAnomaly`)
- Custom string types for domain identifiers: `type Venue string`, `type Symbol string`
- Enum-like constants: `StateOK`, `StateWarn`, `StateBlacklisted`
- Config structs use `yaml` struct tags matching snake_case YAML keys

### Configuration Pattern
- Each service loads a YAML policy file from `configs/policies/`
- Redis connection settings in YAML can be overridden via env vars: `REDIS_ADDR`, `REDIS_PASSWORD`, `REDIS_TLS`
- Service-specific env vars: `DASHBOARD_MASTER_KEY`, `DASHBOARD_AUTH_TOKEN`, `PORT`, `TENANT_ID`, `POSTGRES_DSN`, `TRADING_MODE`

### Error Handling
- `log.Fatalf` for startup failures (missing required config, Redis connection failure)
- `log.Printf` for runtime errors that can be retried (publish failures, read errors)
- Graceful shutdown via `signal.NotifyContext` with `SIGINT`/`SIGTERM`

### Testing
- Test files sit alongside the code they test (`engine_test.go` next to `engine.go`)
- Test helpers are file-scoped (e.g., `testPolicy()`, `makeQuote()`)
- Tests reference spec sections in comments (e.g., `// spec §10 test 1: ...`)
- No external test frameworks — standard `testing` package only

### Docker
- All Dockerfiles use multi-stage builds: `golang:1.24-alpine` builder → `distroless/static-debian12:nonroot` runtime
- Build flags: `CGO_ENABLED=0 GOOS=linux -trimpath -ldflags='-s -w'`
- Each service has its own `Dockerfile.<service-name>`

### Authentication (Dashboard)
- RBAC with 4 roles: `admin > trader > viewer > auditor`
- API keys are SHA-256 hashed before storage
- Legacy single-token auth via `DASHBOARD_AUTH_TOKEN` env var
- Dev mode: no auth token set = automatic admin access
- Bearer token via `Authorization` header or `?token=` query param

## CI Pipeline (`.github/workflows/ci.yml`)

Three jobs run on push/PR to `main`:
1. **Lint**: `go vet ./...` + `golangci-lint`
2. **Test**: `go test -race -cover -coverprofile=coverage.out ./...`
3. **Build** (depends on lint + test): builds all `cmd/*/` binaries and verifies all Dockerfiles build

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `REDIS_ADDR` | No | `localhost:6379` | Redis address (overrides YAML config) |
| `REDIS_PASSWORD` | No | `""` | Redis password |
| `REDIS_TLS` | No | `false` | Set `"true"` to enable TLS |
| `TENANT_ID` | No | `default` | Multi-tenant identifier |
| `DASHBOARD_MASTER_KEY` | Yes (dashboard) | — | 32-byte hex key for credential encryption |
| `DASHBOARD_AUTH_TOKEN` | No | `""` | Legacy single-token auth |
| `PORT` | No | `8080` | Dashboard HTTP port |
| `POSTGRES_DSN` | No | — | Postgres connection string for ledger |
| `TRADING_MODE` | No | `PAPER` | `PAPER`, `SHADOW`, or `LIVE` |

## Common Tasks

### Adding a new exchange adapter
1. Create `internal/marketdata/<exchange>.go` implementing the WebSocket adapter
2. Add venue config to `configs/market_data.yaml`
3. Add base trust weight in `configs/policies/consensus_policy.yaml`
4. Add to `knownExchanges` in `internal/dashboard/server.go` if dashboard integration needed

### Adding a new service
1. Create `cmd/<service-name>/main.go` with signal handling boilerplate
2. Create corresponding `internal/<domain>/` package for business logic
3. If it consumes/produces Redis Streams, add bus wrapper in `internal/eventbus/`
4. Add `Dockerfile.<service-name>` (copy existing pattern)
5. Add service block to `docker-compose.yaml`
6. Add config YAML under `configs/` or `configs/policies/`

### Modifying consensus thresholds
Edit `configs/policies/consensus_policy.yaml`. Key parameters:
- `outlier_bps_warn` / `outlier_bps_blacklist` — deviation thresholds
- `stale_ms` — quote staleness cutoff
- `min_core_quorum` — minimum venues for quality rating
- `base_trust` — per-venue trust weights

## HyroTrader Challenge Build Spec

The file `HYROTRADER_CHALLENGE_BUILD_SPEC.md` in the repo root is the **source of truth** for the HyroTrader Two-Step $100k challenge integration. It covers:

- Challenge rules (Phase 1, Phase 2, Funded account)
- New `challenge-daemon` service architecture and config
- Modifications to `risk-daemon`, `funding-engine`, `arb-engine`, `execution-router`, `capital-allocator`, `market-data`, and `ledger`
- Config structures (`configs/challenge_config.yaml`)
- Testing requirements (unit, integration, paper trading, stress)
- Phased build sequence (A→D) with exact priority ordering

**Key safety note:** The daily drawdown calculation from the *highest equity point* (not opening equity) is the single most dangerous rule. Unit tests for this MUST cover scenarios where delta-neutral positions show temporary unrealised swings on individual legs.

When implementing challenge features, follow the build sequence in the spec: start with Phase A items 1-5 (challenge-daemon built and tested) before touching any strategy code.
