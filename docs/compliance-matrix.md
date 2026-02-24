# Compliance Matrix

## Overview

This document maps regulatory requirements and risk controls to their
implementations in the consensus-engine codebase.

## Risk Control Matrix

### Market Risk

| Control | Implementation | Package |
|---|---|---|
| Position limits | Max position USD per venue/symbol | `internal/allocator` |
| Drawdown limits | Auto-reduce at thresholds | `internal/risk` |
| Kill switch | Redis `kill:switch` key halts all activity | `internal/risk` |
| Regime detection | Auto-adjusts sizing in volatile regimes | `internal/regime` |
| Correlation limits | Cross-asset exposure monitoring | `internal/arb` |

### Operational Risk

| Control | Implementation | Package |
|---|---|---|
| Graduation gates | PAPER → SHADOW → LIVE with fill/score thresholds | `internal/dashboard` |
| Time-based gates | Min days in each mode before upgrade | `internal/dashboard` |
| Sharpe floor | Min 0.50 Sharpe proxy for LIVE | `internal/dashboard` |
| Transfer approvals | Human approval for cross-venue transfers | `internal/dashboard` |
| Transfer velocity | Max transfers per hour | `internal/transfer` |
| Nonce management | Gap-filling nonce manager prevents stuck txs | `internal/onchain` |

### Technology Risk

| Control | Implementation | Package |
|---|---|---|
| Circuit breakers | Per-venue OK → WARN → BLACKLISTED | `internal/consensus` |
| Stale data detection | Quotes expire after `stale_ms` | `internal/consensus` |
| Oracle sanity | Deviation, staleness, flatline checks | `internal/defirisk` |
| Fault injection tests | Simulated venue failures | `internal/integration` |
| Shadow metrics | Paper vs hypothetical-live divergence tracking | `internal/execution` |

### DeFi-Specific Risk

| Control | Implementation | Package |
|---|---|---|
| Protocol risk scoring | Audit/TVL/age/incident-based 0–100 score | `internal/defirisk` |
| Max risk score | Configurable ceiling (default: 40) | `internal/treasuryyield` |
| Depeg detection | Stablecoin peg monitoring with bps threshold | `internal/defirisk` |
| Impermanent loss | Real-time IL calculation for LP positions | `internal/defirisk` |
| Bridge monitoring | Challenge window tracking with alerts | `internal/bridge` |
| Max lock period | Configurable max lock days for yield sources | `internal/treasuryyield` |
| Single-source cap | Max allocation % per yield source | `internal/treasuryyield` |

### Audit Trail

| Requirement | Implementation | Package |
|---|---|---|
| SOC2 audit log | Immutable Postgres audit trail | `internal/ledger` |
| Rich audit events | Actor, role, IP, action, payload | `internal/dashboard` |
| CSV export | `/api/audit/export` endpoint | `internal/dashboard` |
| API key management | SHA-256 hashed keys with RBAC | `internal/auth` |
| Mode transitions | Logged to `audit:mode_transitions` stream | `internal/dashboard` |

## RBAC Model

| Role | Permissions |
|---|---|
| `admin` | Full access: API keys, branding, mode changes, all endpoints |
| `trader` | Trading operations: mode changes, transfer approvals, positions |
| `viewer` | Read-only: prices, PnL, metrics, equity curve |
| `auditor` | Audit trail: SOC2 export, audit log, reporting |

## Data Classification

| Data Type | Classification | Storage | Retention |
|---|---|---|---|
| API keys (hashed) | Sensitive | Postgres | Until deleted |
| Trade fills | Internal | Postgres + Redis | Indefinite |
| Market data | Public | Redis Streams | Rolling window |
| Private keys | Secret | Environment only | Never persisted |
| Audit log | Compliance | Postgres | 7 years |
| PnL attribution | Internal | Postgres | Indefinite |

## Regulatory Considerations

### MiCA (EU)
- Custody of crypto-assets: hot wallet management with nonce tracking
- Record keeping: full audit trail with SOC2 export
- Risk management: multi-layer risk controls, kill switch

### US Securities Law
- Trading modes: PAPER/SHADOW/LIVE graduation prevents unauthorised live trading
- Position limits: configurable per-venue, per-symbol limits

### AML/KYC
- Not applicable: system does not onboard customers
- Exchange accounts: managed externally with venue-level API keys
