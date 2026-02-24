# Yield Strategies

## Overview

The consensus-engine yield infrastructure captures risk-adjusted returns across
delta-neutral and near-neutral strategies spanning CEX, DEX, and DeFi protocols.

## Strategy Catalogue

### Tier 1 — CEX Delta-Neutral (Highest Priority)

| Strategy | Expected APY | Risk | Package |
|---|---|---|---|
| Funding Rate Carry | 15–40% | Low | `internal/funding` |
| Basis / Cash-and-Carry | 8–20% | Low | `internal/arb` |
| Cross-Venue Arb | Variable | Low | `internal/arb` |
| Triangular Arb | Variable | Low | `internal/triangular` |
| Maker Rebate Capture | 1–3% fee offset | Low | `internal/makerrebate` |

### Tier 2 — DEX / DeFi

| Strategy | Expected APY | Risk | Package |
|---|---|---|---|
| Stable-Stable LP | 3–8% | Medium | `internal/dexindex` |
| CEX-DEX Arb | Variable | Medium | `internal/dex` |
| Lending Yield | 2–6% | Low | `internal/treasuryyield` |
| Liquidation Keeping | Variable | Medium | `internal/keeper` |

### Tier 3 — Advanced

| Strategy | Expected APY | Risk | Package |
|---|---|---|---|
| Treasury Yield (T-Bills) | 4–5% | Very Low | `internal/treasuryyield` |
| Options Yield | 5–15% | High | Future |
| Cross-Chain Arb | Variable | Medium | `internal/bridge` |

## Architecture

```
┌─────────────┐    ┌──────────────┐    ┌──────────────────┐
│  CEX Market  │    │  DEX Indexer │    │  Lending Rates   │
│    Data      │    │  (on-chain)  │    │   (on-chain)     │
└──────┬───────┘    └──────┬───────┘    └────────┬─────────┘
       │                   │                     │
       ▼                   ▼                     ▼
┌──────────────────────────────────────────────────────────┐
│              Strategy Selection Layer                     │
│  (regime-aware, risk-adjusted, capacity-managed)          │
└──────────────────────────┬───────────────────────────────┘
                           │
       ┌───────────────────┼───────────────────┐
       ▼                   ▼                   ▼
┌─────────────┐    ┌──────────────┐    ┌──────────────┐
│  CEX Exec   │    │  On-chain    │    │  Treasury    │
│  Router     │    │  Execution   │    │  Yield       │
└─────────────┘    └──────────────┘    └──────────────┘
```

## Key Services

### `cmd/treasury-yield-router`
Routes idle capital across lending pools, tokenized T-bills, and staking
protocols. Optimises for risk-adjusted yield with configurable constraints:
- `max_risk_score`: protocol risk ceiling (0–100)
- `max_single_alloc_pct`: diversification limit
- `max_lock_days`: liquidity constraint
- `reserves_pct`: operational buffer

### `cmd/keeper-engine`
Monitors lending protocol positions for undercollateralisation and executes
profitable liquidations. Supports flash-loan-powered liquidations for
capital-efficient execution.

### `cmd/onchain-market-data`
Indexes DEX pool states across EVM chains, normalises quotes from Uniswap V2/V3,
Curve, and Balancer into a common format, and publishes to `dex:pool_state` stream.

### `cmd/onchain-execution`
Manages hot wallet nonces, builds transactions, runs simulations via eth_call,
and submits on-chain transactions with gas management and pending-tx tracking.

## Redis Streams

| Stream | Producer | Description |
|---|---|---|
| `dex:pool_state` | onchain-market-data | DEX pool snapshots |
| `dex:quotes` | onchain-market-data | Normalised DEX quotes |
| `lending:rates` | onchain-market-data | Lending protocol rates |
| `liquidation:candidates` | keeper-engine | Profitable liquidation targets |
| `onchain:tx_events` | onchain-execution | Transaction lifecycle events |
| `bridge:status` | bridge monitor | Cross-chain transfer status |
| `yield:allocations` | treasury-yield-router | Allocation changes |
| `keeper:events` | keeper-engine | Keeper execution events |

## Risk Controls

1. **Protocol risk scoring** — each DeFi protocol rated 0–100 based on audit
   count, TVL, age, incident history, and upgradeability
2. **Depeg detection** — continuous monitoring of stablecoin pegs with alerting
3. **Oracle sanity** — validates oracle feeds for staleness, deviation, and flatline
4. **Impermanent loss tracking** — real-time IL calculation for LP positions
5. **Bridge challenge windows** — monitors optimistic bridge challenge periods
6. **Position sizing** — max allocation per protocol, asset, and chain

## Dashboard Endpoints

| Endpoint | Description |
|---|---|
| `GET /api/yield/overview` | Portfolio-level yield metrics |
| `GET /api/yield/sources` | Available yield sources with rates |
| `GET /api/yield/portfolio` | Current allocations |
| `GET /api/keeper/stats` | Keeper performance statistics |
| `GET /api/keeper/candidates` | Current liquidation candidates |
| `GET /api/bridge/transfers` | Active bridge transfers |
| `GET /api/bridge/alerts` | Challenge window alerts |
| `GET /api/dex/pools` | Tracked DEX pools |
| `GET /api/defi/risk` | Protocol risk scores |
| `GET /api/defi/depeg` | Stablecoin depeg alerts |
| `GET /api/maker-rebate/report` | Fee tier optimisation report |
| `GET /api/triangular/opportunities` | Triangular arb opportunities |
