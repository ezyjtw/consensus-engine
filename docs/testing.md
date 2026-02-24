# Testing and Verification

> How to run tests, replay harness, fault injection, and compose smoke tests.

---

## Unit Tests

```bash
# Run all tests
go test ./...

# With race detector and coverage
go test -race -cover -coverprofile=coverage.out ./...

# Single package
go test ./internal/consensus/...
go test ./internal/execution/...
go test ./internal/risk/...
```

---

## Deterministic Replay Harness

The replay harness in `internal/integration/` wires components directly (no Redis) to verify the full pipeline deterministically.

### Running replay tests

```bash
go test -v ./internal/integration/...
```

### What the replay tests verify

| Test | What It Proves |
|---|---|
| `TestReplayConsensusToExecution` | Full pipeline: quotes → consensus → arb detection → execution. Verifies fill prices, fees, and PnL calculation. |
| `TestReplayVenueBlacklistPropagates` | When consensus blacklists a venue, arb engine excludes it from opportunities and consensus quality degrades appropriately. |

### Adding new replay scenarios

1. Create recorded quote data in the test file using `makeReplayQuote()`
2. Feed quotes through `consensus.NewEngine().Compute()`
3. Feed results through `arb.NewEngine().Process()`
4. Feed intents through `execution.NewPaperExecutor().Execute()`
5. Assert outputs at each stage

---

## Fault Injection Tests

The fault injection harness in `internal/integration/fault_test.go` simulates failure scenarios:

### Running fault tests

```bash
go test -v ./internal/integration/ -run TestFault
```

### Fault scenarios covered

| Test | Scenario | Expected Behavior |
|---|---|---|
| `TestFaultAllQuotesStale` | All venue quotes are stale (>750ms) | Consensus quality degrades to LOW |
| `TestFaultSingleFreshQuote` | Only 1 of 4 venues has fresh data | Consensus still produces mid, quality not HIGH |
| `TestFaultBlacklistedVenueNotUsedInConsensus` | Venue is BLACKLISTED with off-price | Consensus not pulled by blacklisted venue |
| `TestFaultExpiredIntent` | Intent TTL has passed | Fill marked as `intent_expired: true` |
| `TestFaultNoConsensusPrice` | Price cache empty for symbol | Executor returns nil (safe skip) |
| `TestFaultDrawdownEscalation` | Loss exceeds `max_drawdown_pct` | Risk daemon transitions to PAUSED |
| `TestFaultErrorRateEscalation` | Error rate exceeds threshold | Risk daemon transitions to PAUSED |
| `TestFaultADLRiskEscalation` | ADL risk exceeds 40% | Risk daemon transitions to PAUSED |
| `TestFaultVenueDelevSafeMode` | 2+ deleveraging events | Risk daemon transitions to SAFE |
| `TestFaultPlaybookActivation` | ADL risk triggers playbook | `ADL_EVENT` playbook is active |
| `TestFaultPlaybookResolution` | Manual playbook resolution | Playbook removed from active list |
| `TestFaultLowQualityBlocksArb` | LOW quality consensus update | Arb engine emits 0 intents |

---

## Shadow Confidence Tests

```bash
go test -v ./internal/execution/ -run TestShadow
```

| Test | What It Proves |
|---|---|
| `TestShadowMetricsEmpty` | Empty metrics return zero values safely |
| `TestShadowMetricsRecordAndReport` | Predicted/realized edge, missed edge, capture ratio computed correctly |
| `TestShadowMetricsNilFill` | Nil fills don't panic |
| `TestShadowMetricsCapAt1000` | Rolling buffer caps at 1000 records |

---

## Graduation Harness Tests

```bash
go test -v ./internal/execution/ -run TestGraduation
```

| Test | What It Proves |
|---|---|
| `TestGraduationCurrentLimits` | Week 1 caps match config |
| `TestGraduationRampSchedule` | Progressive weekly multiplier works (2x/week) |
| `TestGraduationEligiblePaperToShadow` | Min paper days enforced |
| `TestGraduationEligibleShadowToLive` | Min shadow days, Sharpe, drawdown all enforced |

---

## Compose Smoke Test

The compose smoke test runs the full 12-service stack and validates the data plane end-to-end.

### Running locally

```bash
export DASHBOARD_MASTER_KEY=$(openssl rand -hex 32)
docker compose up -d
sleep 10
./scripts/smoke-test.sh
docker compose down -v
```

### What the smoke test checks

1. Redis connectivity
2. Dashboard health endpoint
3. Market quotes flowing (`market:quotes` stream)
4. Consensus output (`consensus:updates`, `consensus:anomalies`)
5. Strategy intents generated
6. Execution fills produced
7. Risk daemon state published
8. Ledger lag within bounds
9. Venue state cache populated
10. API endpoints responding

### CI integration

The smoke test runs as a GitHub Actions job after the build step:

```yaml
smoke-test:
  name: Compose Smoke Test
  needs: [build]
  steps:
    - docker compose up -d --build --wait
    - ./scripts/smoke-test.sh
    - docker compose down -v
```

---

## Test Coverage Summary

| Package | Test Files | Coverage Focus |
|---|---|---|
| `internal/consensus` | `engine_test.go` | MAD, trust scoring, circuit breaker, VWAP bands |
| `internal/execution` | `paper_test.go`, `shadow_test.go`, `graduation_test.go` | Paper fills, shadow metrics, graduation ramp |
| `internal/arb` | `engine_test.go` | Quality gating, edge computation, cooldown |
| `internal/risk` | `daemon_test.go` | State machine transitions, metrics, playbooks |
| `internal/transfer` | `policy_test.go` | Allowlist, velocity, tamper detection, dual approval |
| `internal/store` | `store_test.go` | In-memory quote/status cache |
| `internal/exchange` | `exchange_test.go` | Venue constraints, price/qty rounding |
| `internal/dex` | `router_test.go` | DEX routing, price quoting |
| `internal/l2` | `bridge_test.go` | L2 bridge transfers |
| `internal/treasury` | `treasury_test.go` | Deposit detection, sweep, reconciliation |
| `internal/integration` | `replay_test.go`, `fault_test.go` | Full pipeline replay, fault injection |
