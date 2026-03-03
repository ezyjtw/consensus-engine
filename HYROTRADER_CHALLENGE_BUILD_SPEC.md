# HyroTrader Challenge Build Specification

## Fork & Build Instructions for Claude Code

**Source Repository:** `https://github.com/ezyjtw/consensus-engine`
**Target:** Fork into a new repository called `hyrotrader-challenge-engine`
**Language:** Go (matching existing codebase)
**Objective:** Modify the existing 9-service crypto trading system to pass a HyroTrader Two-Step $100k challenge using delta-neutral strategies, then operate profitably on the funded account.

-----

## 1. Context & Background

### 1.1 What Is This Project?

This is an existing Go microservices-based crypto trading system with 9 services:

| Service | Purpose |
|---|---|
| `consensus-engine` | Multi-venue price consensus using weighted trust scores |
| `arb-engine` | Cross-exchange and intra-exchange arbitrage detection and execution |
| `funding-engine` | Funding rate monitoring and arbitrage (long spot + short perp) |
| `liquidity-engine` | Liquidity analysis and optimal venue selection |
| `execution-router` | Smart order routing across venues |
| `capital-allocator` | Dynamic capital distribution across strategies and exchanges |
| `risk-daemon` | Portfolio-level risk monitoring and circuit breakers |
| `market-data` | WebSocket connections to exchanges, normalised data feeds |
| `ledger` | PnL tracking, position accounting, audit trail |

The system uses Docker containers orchestrated via docker-compose, communicates internally via Redis Streams, and stores data in PostgreSQL.

### 1.2 What Is the HyroTrader Challenge?

HyroTrader is a crypto prop trading firm. We are attempting their **Two-Step challenge on a $100,000 account** (fee: $999, refundable on first payout). The challenge involves:

**Phase 1 (Challenge):**

- Profit target: 10% ($10,000)
- Maximum daily drawdown: 5% ($5,000) — trailing from highest equity that day, resets at UTC midnight
- Maximum overall drawdown: 10% ($10,000) — from starting balance
- Minimum trading days: 10 (at least one trade opened per day)
- No time limit
- No single trade can account for >40% of total profit (profit distribution rule)
- Stop-loss required on every trade within 5 minutes of opening
- Max risk per trade: 3% of initial balance (stop-loss distance × position size ≤ $3,000)
- Low-cap altcoin restriction: no more than 5% of balance in tokens with <$100M market cap

**Phase 2 (Verification):**

- Profit target: 5% ($5,000)
- Same drawdown rules as Phase 1
- Minimum trading days: 5
- No time limit

**Funded Account (post-challenge):**

- Same daily drawdown (5%) and max overall drawdown (10%) rules
- Profit distribution rule (40%) drops — no longer applies
- NEW rule: Maximum exposure 25% of initial balance (total margin across open positions)
- NEW rule: Cumulative open position value cannot exceed 2× account balance
- Profit split starts at 70%, scales +5% every 4 months to max 90%
- Payouts in USDT/USDC within 12-24 hours
- Account can scale up to 10× original size (max $1,000,000)

### 1.3 Trading Platform

- **Exchange:** Bybit (connected via API)
- **Available instruments:** All Bybit spot and perpetual futures pairs
- **Leverage:** Up to 1:100 (we will use 2-3× maximum)
- **Trading hours:** 24/7 (crypto never closes)

-----

## 2. Architecture Changes Required

### 2.1 New Service: `challenge-daemon`

**This is the most critical new component.** It acts as a compliance layer between the existing strategy services and the execution-router, enforcing all HyroTrader rules before any order reaches Bybit.

**Location:** `cmd/challenge-daemon/main.go` and `internal/challenge/`

**Responsibilities:**

1. **Pre-trade rule validation** — intercept every order request from arb-engine, funding-engine, etc. before it reaches execution-router. Check against all active challenge rules. Reject or modify non-compliant orders.

2. **Daily drawdown tracking** — track equity in real-time. The daily drawdown is calculated from the HIGHEST equity point during the current UTC day (including unrealised PnL). This is the most dangerous rule because mark-to-market fluctuations on delta-neutral positions can temporarily show large unrealised swings on individual legs.

   Implementation:

   ```
   - Track `daily_high_water_mark` — highest equity seen since UTC 00:00
   - Track `current_equity` — updated every tick
   - `daily_drawdown = daily_high_water_mark - current_equity`
   - SOFT HALT at daily_drawdown >= 3% ($3,000) — stop opening new positions
   - HARD HALT at daily_drawdown >= 4% ($4,000) — flatten all optional positions
   - BREACH at daily_drawdown >= 5% ($5,000) — challenge failed
   - Reset daily_high_water_mark at UTC 00:00:00 each day
   ```

3. **Overall drawdown tracking** — track equity vs starting balance ($100,000).

   ```
   - SOFT HALT at overall_drawdown >= 7% — reduce position sizes by 50%
   - HARD HALT at overall_drawdown >= 8.5% — flatten everything except core funding arb
   - BREACH at overall_drawdown >= 10% — challenge failed
   ```

4. **Stop-loss enforcement** — every order that opens a position MUST have a corresponding stop-loss order placed within 5 minutes. The challenge-daemon should:
   - Automatically attach a stop-loss to every order before forwarding to execution-router
   - For delta-neutral legs: set wide emergency stop-losses at 2.5% per leg (these should never trigger during normal operations but satisfy the rule)
   - Monitor that stop-losses remain active — if Bybit cancels one (e.g., due to position change), immediately replace it
   - If a stop-loss triggers on one leg of a hedged pair, IMMEDIATELY close the other leg

5. **Per-trade risk enforcement** — stop-loss distance × position size (including commissions) cannot exceed 3% of initial balance ($3,000).

   ```
   max_risk = 0.03 * initial_balance  // $3,000
   allowed_position_size = max_risk / stop_loss_distance
   ```

6. **Profit distribution tracking (Phase 1 & 2 only)** — no single closed trade can account for >40% of total accumulated profit.

   ```
   - Track all closed trade PnLs
   - Before closing a profitable trade, calculate: would this trade's PnL exceed 40% of (total_profit + this_trade_PnL)?
   - If yes, consider partial closing or flag for manual review
   - This rule DROPS on the funded account — implement as a toggleable flag
   ```

7. **Minimum trading day counter** — ensure at least one qualifying trade is opened per calendar day. If no trade has been opened by 22:00 UTC, open a minimal position (e.g., $100 of BTC spot) to register a trading day.

8. **Low-cap restriction** — maintain a list of Bybit tokens with <$100M market cap (update daily via Bybit or CoinGecko API). Block any order that would allocate >5% of balance to these tokens.

9. **Funded account mode** — toggleable via config. When active:
   - Disable profit distribution rule (40% check)
   - Enable maximum exposure rule (total margin ≤ 25% of initial balance)
   - Enable cumulative position value rule (total notional ≤ 2× balance)

**Config structure** (`configs/challenge_config.yaml`):

```yaml
challenge:
  enabled: true
  mode: "phase1"  # phase1 | phase2 | funded
  initial_balance: 100000
  daily_drawdown_soft_pct: 3.0
  daily_drawdown_hard_pct: 4.0
  daily_drawdown_breach_pct: 5.0
  overall_drawdown_soft_pct: 7.0
  overall_drawdown_hard_pct: 8.5
  overall_drawdown_breach_pct: 10.0
  max_risk_per_trade_pct: 3.0
  profit_distribution_max_pct: 40.0
  min_trading_days: 10
  stop_loss_deadline_seconds: 300
  stop_loss_default_pct: 2.5
  low_cap_threshold_usd: 100000000
  low_cap_max_allocation_pct: 5.0
  funded_max_margin_exposure_pct: 25.0
  funded_max_notional_multiplier: 2.0
  safety_trade_time_utc: "22:00"
  safety_trade_size_usd: 100
```

### 2.2 Modify: `risk-daemon`

Add a **challenge mode** that tightens all existing risk controls:

**New risk controls for challenge mode:**

| Control | Normal Mode | Challenge Mode |
|---|---|---|
| Daily drawdown halt | -5% of NAV | -3% soft, -4% hard |
| Peak-to-trough halt | -15% | -7% soft, -8.5% hard |
| Per-position max | 10% of portfolio | 8% of portfolio |
| Stale data pause | >5 seconds | >3 seconds |
| Position sizing | 0.25× Kelly | 0.15× Kelly |
| Margin ratio alert | <150% | <250% |
| Margin ratio emergency | <120% | <200% |

**Critical addition — leg mismatch detector:**

- Every 30 seconds, scan all open positions
- For every spot position, verify a matching perp position exists (and vice versa)
- If any position is unhedged for >60 seconds, ALERT
- If any position is unhedged for >120 seconds, auto-close the orphaned leg
- Log every mismatch event to the ledger with full details

**Critical addition — equity calculator for HyroTrader:**

- HyroTrader calculates equity across your Bybit account (spot + futures combined)
- Your risk-daemon equity calculation MUST match HyroTrader's methodology exactly
- Track: spot balances, unrealised PnL on futures, realised PnL, funding payments received/paid, trading fees paid
- Poll Bybit's account endpoint every 5 seconds to verify your internal equity tracking matches Bybit's reported equity
- If discrepancy >0.1%, log a warning and use Bybit's number as ground truth

### 2.3 Modify: `funding-engine`

This is the primary revenue generator. Optimise for the challenge:

**Strategy: Single-exchange funding rate arbitrage on Bybit**

- Long spot BTC/ETH + Short perpetual BTC/ETH on Bybit
- Collect funding payments every 8 hours (Bybit schedule: 00:00, 08:00, 16:00 UTC)
- Delta-neutral — price movements cancel out between legs

**Implementation requirements:**

1. **Entry logic:**
   - Monitor funding rates via Bybit API (`GET /v5/market/funding/history`)
   - Enter position when predicted next funding rate > 0.01% (annualised ~13%)
   - Enter BOTH legs simultaneously (within 1 second of each other)
   - Use market orders for speed, limit orders only if spread is acceptable
   - Target allocation: 50-60% of account to funding arb

2. **Exit logic:**
   - Exit when predicted funding rate drops below 0.005% for 3 consecutive cycles
   - Exit when funding rate turns negative for 2 consecutive cycles
   - Exit both legs simultaneously
   - Never hold an unhedged position

3. **Position sizing for challenge:**
   - Maximum 60% of account in funding arb ($60,000 per leg)
   - Scale position based on funding rate magnitude:
     - Rate > 0.03%: use full 60% allocation
     - Rate 0.01-0.03%: use 40% allocation
     - Rate < 0.01%: use 20% allocation or exit

4. **Multi-asset support:**
   - Run funding arb on BTC and ETH simultaneously
   - Can add SOL if funding rate is exceptionally high (>0.05%)
   - Split allocation: 60% BTC, 40% ETH (adjust based on relative funding rates)

5. **Funding payment tracking:**
   - Log every funding payment: timestamp, symbol, rate, payment amount
   - Track cumulative funding income separately from trading PnL
   - This data feeds into the ledger service

### 2.4 Modify: `arb-engine`

Adapt for intra-Bybit opportunities:

**Strategy: Intra-exchange basis capture**

- When Bybit spot-perp spread widens beyond threshold, enter a basis trade
- Long spot + short perp when perp is at premium
- Short spot (if possible) + long perp when perp is at discount
- Close when spread normalises

**Implementation requirements:**

1. **Spread monitoring:**
   - Calculate real-time spread: `(perp_price - spot_price) / spot_price * 100`
   - Track rolling average spread over 1h, 4h, 24h windows
   - Generate signal when current spread > 2 standard deviations from 24h mean

2. **Entry/exit thresholds:**
   - Enter when annualised basis > 15% AND spread is widening
   - Exit when spread returns to within 0.5 standard deviations of mean
   - Time-based exit: close any basis trade open >7 days (avoid holding through regime changes)

3. **Allocation:**
   - Maximum 25% of account ($25,000) in basis trades
   - This allocation is SEPARATE from funding arb allocation
   - Combined funding arb + basis should not exceed 80% of account

### 2.5 New Feature: Momentum Overlay (Optional — Higher Risk)

**Strategy: Liquidation cascade detection**

- Monitor aggregate leverage, open interest, and funding rate extremes across Bybit
- When conditions suggest an imminent liquidation cascade, take a small directional position

**Implementation requirements:**

1. **Signal generation (in market-data service):**
   - Track Bybit open interest for BTC and ETH (`GET /v5/market/open-interest`)
   - Track aggregate long/short ratio (`GET /v5/market/account-ratio`)
   - Track funding rate trend (rising/falling over last 24h)
   - CASCADE_LONG signal: funding rate > 0.05% AND long/short ratio > 2.0 AND open interest at 30-day high — shorts will get squeezed if price drops
   - CASCADE_SHORT signal: funding rate < -0.02% AND long/short ratio < 0.5 AND open interest at 30-day high — longs will get squeezed if price rises

2. **Execution rules:**
   - Maximum allocation: 10% of account ($10,000) per cascade trade
   - Hard stop-loss: 2% of account ($2,000) per trade
   - Take-profit: 3% of account ($3,000) — 1.5:1 reward/risk
   - Maximum 1 cascade trade open at any time
   - Maximum 2 cascade trades per week
   - DO NOT open cascade trades if daily drawdown has already reached -2%

3. **This feature should be DISABLED by default** — enable via config flag. It introduces directional risk and should only be activated after the funding arb is proven stable on the challenge.

### 2.6 Modify: `execution-router`

**Bybit-specific adaptations:**

1. **API integration:**
   - Use Bybit V5 API exclusively
   - Unified endpoint: `https://api.bybit.com`
   - Testnet endpoint: `https://api-testnet.bybit.com`
   - WebSocket for market data: `wss://stream.bybit.com/v5/public/linear`
   - WebSocket for private data: `wss://stream.bybit.com/v5/private`

2. **Order types needed:**
   - Market orders (for urgent entries/exits)
   - Limit orders (for non-urgent entries)
   - Stop-loss orders (mandatory for every position)
   - Take-profit orders (for cascade trades)
   - Conditional orders (for cascade entries)

3. **Atomic execution for delta-neutral entries:**
   - When opening a funding arb position, execute both legs in rapid sequence:
     1. Place spot buy order
     2. Immediately place perp short order
     3. If either fails, immediately cancel/close the other
     4. Maximum acceptable time between legs: 2 seconds
     5. If >2 seconds between fills, close everything and retry
   - Log execution quality: time between legs, slippage on each leg, net entry cost

4. **Position reconciliation:**
   - Every 60 seconds, fetch all open positions from Bybit API
   - Compare with internal position tracking in ledger
   - If mismatch detected, log alert and update internal state to match Bybit
   - This catches any edge cases where orders filled but confirmation was lost

### 2.7 Modify: `capital-allocator`

**Challenge-mode allocation strategy:**

```
Total Account: $100,000

Funding Rate Arbitrage: 50-60% ($50,000-$60,000)
  |-- BTC funding arb: 60% of allocation
  |-- ETH funding arb: 40% of allocation

Basis Trading: 15-25% ($15,000-$25,000)
  |-- Intra-Bybit spot-perp basis

Momentum Overlay: 0-10% ($0-$10,000)
  |-- Disabled by default, enable after proving stability

Cash Reserve: 10-15% ($10,000-$15,000)
  |-- Buffer for margin, drawdown protection, unexpected opportunities
```

The capital-allocator should dynamically adjust these based on:

- Current drawdown level (reduce allocations as drawdown increases)
- Funding rate environment (increase funding arb allocation when rates are high)
- Volatility regime (reduce all allocations during extreme vol)

### 2.8 Modify: `market-data`

**Bybit-specific data feeds needed:**

1. **Real-time (WebSocket):**
   - Order book updates (top 50 levels): `orderbook.50.BTCUSDT`
   - Trades: `publicTrade.BTCUSDT`
   - Tickers: `tickers.BTCUSDT` (includes funding rate countdown)
   - Klines: `kline.1.BTCUSDT` (1-minute candles)
   - Private: positions, orders, executions, wallet

2. **Polling (REST API, intervals noted):**
   - Funding rate history: every 1 hour (`/v5/market/funding/history`)
   - Open interest: every 5 minutes (`/v5/market/open-interest`)
   - Long/short ratio: every 15 minutes (`/v5/market/account-ratio`)
   - Account balance: every 5 seconds (`/v5/account/wallet-balance`)
   - Position list: every 10 seconds (`/v5/position/list`)

3. **Symbols to track:**
   - Primary: BTCUSDT (spot), BTCUSDT (linear perp)
   - Secondary: ETHUSDT (spot), ETHUSDT (linear perp)
   - Optional: SOLUSDT if high funding rate detected

### 2.9 Modify: `ledger`

**Additional tracking for the challenge:**

1. **Challenge progress dashboard data:**
   - Current phase (Phase 1 / Phase 2 / Funded)
   - Days traded so far
   - Current equity and PnL
   - Distance to profit target (%)
   - Daily drawdown used today (% and $)
   - Overall drawdown from start (% and $)
   - Largest single trade profit (for 40% rule tracking)
   - Funding payments collected (cumulative)
   - Estimated days to target at current rate

2. **Audit trail for every order:**
   - Timestamp, strategy_id, symbol, side, order_type, price, quantity
   - Fill price, slippage, fees paid
   - Stop-loss price set, stop-loss order ID
   - Challenge rule checks passed/failed
   - Matching hedge leg order ID

-----

## 3. Configuration & Deployment

### 3.1 Environment Configuration

```yaml
# configs/env.yaml
environment: "challenge"  # development | testnet | challenge | funded

bybit:
  api_key: "${BYBIT_API_KEY}"
  api_secret: "${BYBIT_API_SECRET}"
  testnet: false  # true for paper trading
  recv_window: 5000

services:
  market_data:
    websocket_reconnect_delay_ms: 1000
    max_reconnect_attempts: 10
    stale_data_threshold_ms: 3000

  challenge_daemon:
    enabled: true
    config_path: "configs/challenge_config.yaml"

  risk_daemon:
    challenge_mode: true
    equity_poll_interval_ms: 5000
    position_reconciliation_interval_ms: 60000
    leg_mismatch_alert_threshold_ms: 60000
    leg_mismatch_close_threshold_ms: 120000

  funding_engine:
    enabled: true
    entry_threshold_rate: 0.0001  # 0.01%
    exit_threshold_rate: 0.00005  # 0.005%
    max_allocation_pct: 60
    symbols: ["BTCUSDT", "ETHUSDT"]

  arb_engine:
    enabled: true
    intra_bybit_only: true  # disable cross-exchange for challenge
    basis_entry_std_dev: 2.0
    basis_exit_std_dev: 0.5
    max_allocation_pct: 25
    max_hold_days: 7

  momentum_overlay:
    enabled: false  # enable after proving stability
    max_allocation_pct: 10
    max_trades_per_week: 2
    stop_loss_pct: 2.0
    take_profit_pct: 3.0

  capital_allocator:
    cash_reserve_pct: 12
    rebalance_interval_minutes: 30
    drawdown_scaling: true  # reduce allocations as drawdown increases
```

### 3.2 Docker Compose Updates

Add the new challenge-daemon service to docker-compose.yaml:

```yaml
challenge-daemon:
  build:
    context: .
    dockerfile: Dockerfile.challenge-daemon
  depends_on:
    - redis
    - risk-daemon
    - execution-router
  environment:
    - CHALLENGE_CONFIG=/app/configs/challenge_config.yaml
    - REDIS_ADDR=redis:6379
  restart: unless-stopped
```

Ensure the execution-router ONLY accepts orders routed through the challenge-daemon when challenge mode is enabled. Direct orders from strategy services should be blocked.

### 3.3 Testing Requirements

**Before starting the real challenge, ALL of the following must pass:**

1. **Unit tests** for challenge-daemon:
   - Daily drawdown calculation (test with various equity curves including intraday reversals)
   - Overall drawdown calculation
   - Stop-loss attachment and enforcement
   - Profit distribution rule (40% check)
   - Per-trade risk limit enforcement
   - Minimum trading day counter
   - Low-cap restriction
   - Funded account mode toggle

2. **Integration tests** on Bybit testnet:
   - Full funding arb cycle: enter → collect funding → exit
   - Atomic execution: verify both legs fill within 2 seconds
   - Stop-loss trigger on one leg → automatic close of other leg
   - Drawdown halt trigger → all new orders rejected
   - Position reconciliation catches mismatches
   - WebSocket reconnection after disconnect

3. **Paper trading** for minimum 2 weeks:
   - Run the full system against Bybit testnet
   - Track simulated equity curve with challenge rules applied
   - Verify daily drawdown never exceeds 3% (soft limit)
   - Verify profit accumulation rate (estimate time to 10% target)
   - Verify stop-loss placement on every trade
   - Verify leg mismatch detector catches artificial mismatches
   - Verify safety trade fires at 22:00 UTC if no trades that day

4. **Stress tests:**
   - Simulate a 10% BTC price drop in 1 hour — verify delta-neutral positions stay hedged and drawdown stays within limits
   - Simulate Bybit API timeout during order placement — verify orphaned legs are detected and closed
   - Simulate negative funding rate for 48 hours — verify bot reduces/exits positions correctly
   - Simulate funding rate spike to 0.1% — verify bot scales up correctly without exceeding allocation limits

-----

## 4. Build Sequence (Priority Order)

Claude Code should implement these in this exact order:

### Phase A: Foundation (Do First)

1. Create `configs/challenge_config.yaml` with all parameters
2. Build `internal/challenge/` package with all rule-checking logic
3. Build `cmd/challenge-daemon/main.go` service
4. Write comprehensive unit tests for the challenge-daemon
5. Modify risk-daemon to add challenge mode with tightened thresholds
6. Add leg mismatch detector to risk-daemon
7. Add equity tracking that matches HyroTrader's methodology

### Phase B: Strategy Adaptation (Do Second)

1. Modify market-data service for Bybit V5 API (WebSocket + REST)
2. Modify execution-router for Bybit V5 API order placement
3. Add atomic execution logic (both legs within 2 seconds)
4. Modify funding-engine for single-exchange Bybit funding arb
5. Modify arb-engine for intra-Bybit basis trading only
6. Modify capital-allocator for challenge allocation strategy

### Phase C: Safety & Monitoring (Do Third)

1. Add position reconciliation (internal vs Bybit API)
2. Add automatic safety trade at 22:00 UTC
3. Add challenge progress tracking to ledger
4. Build a simple dashboard/logging output showing challenge progress
5. Update docker-compose.yaml with challenge-daemon service
6. Write integration tests against Bybit testnet

### Phase D: Optional Enhancements (Do Last, If Time Permits)

1. Add momentum overlay (cascade detection) — disabled by default
2. Add Prometheus metrics for challenge-specific monitoring
3. Add Telegram/Discord alerts for drawdown warnings and milestones
4. Add auto-scaling logic to capital-allocator based on drawdown level

-----

## 5. Critical Reminders

### DO:

- Use `shopspring/decimal` for ALL monetary calculations — never float64
- Use `context.Context` with timeouts on every Bybit API call
- Log everything with structured logging (`slog`) including: service, request_id, symbol, exchange, challenge_phase
- Test every rule-checking function with edge cases
- Keep the challenge-daemon stateless where possible — persist critical state (equity high-water mark, trading day count) in Redis with TTLs
- Handle Bybit API rate limits gracefully (10 requests/second for order endpoints)
- Implement exponential backoff on all API retries

### DO NOT:

- Never use float64 for money, prices, or quantities
- Never send orders directly to Bybit bypassing the challenge-daemon
- Never open a position without a corresponding stop-loss
- Never hold an unhedged position for more than 120 seconds
- Never exceed 3× leverage on any position
- Never commit API keys, secrets, or .env files
- Never trade low-cap altcoins (<$100M market cap) beyond 5% allocation
- Never assume Bybit's API response will be instant — always use timeouts
- Never modify a stop-loss to be wider than the initial placement unless the position has moved into profit

### IMPORTANT NUANCES:

- HyroTrader's daily drawdown is from the HIGHEST equity point that day, not from the day's opening equity. This is critical — a position that goes +$3,000 then back to flat counts as $3,000 of drawdown
- The stop-loss requirement applies to EVERY individual trade, including both legs of a delta-neutral pair. Each leg needs its own stop-loss
- During the challenge phases, the 40% profit distribution rule means we need many small consistent wins, not a few big trades. This naturally suits funding rate arbitrage
- The Bybit API connection is via YOUR personal Bybit account connected to HyroTrader via API. HyroTrader monitors the account externally. They see everything
- After passing the challenge, you start on a DEMO funded account and must withdraw profits 3 times before getting the full funded amount with real capital

-----

## 6. Expected Performance

### Conservative Estimate (Bear/Sideways Market):

- Funding arb: ~$900/month
- Basis trading: ~$200/month
- **Total gross: ~$1,100/month**
- After 70% split (funded): ~$770/month
- Time to pass Phase 1: ~8-10 weeks

### Base Case (Normal Market):

- Funding arb: ~$1,800/month
- Basis trading: ~$400/month
- **Total gross: ~$2,200/month**
- After 70% split (funded): ~$1,540/month
- Time to pass Phase 1: ~5-6 weeks

### Bull Market:

- Funding arb: ~$3,600/month
- Basis trading: ~$600/month
- **Total gross: ~$4,200/month**
- After 70% split (funded): ~$2,940/month
- Time to pass Phase 1: ~3-4 weeks

### Blowup Risk (Assuming Correctly Coded):

- Pure delta-neutral only: 2-3% probability of 10% drawdown breach
- With momentum overlay: 8-12% probability
- Primary risk is NOT the strategy — it's infrastructure failures (API timeouts, unhedged legs, stop-loss triggers on individual legs)

-----

## 7. Post-Challenge Roadmap

Once the challenge is passed and the funded account is active:

1. **Months 1-4:** Run pure delta-neutral at 70% split. Focus on consistency and scaling.
2. **Month 4:** Profit split increases to 75%. Consider enabling momentum overlay if funding arb is consistently profitable.
3. **Month 8:** Profit split at 80%. If account has grown via scaling (20% profit over 4 months), allocation increases proportionally.
4. **Month 12-16:** Profit split at 85-90%. Target account scale to $500k-$1M.
5. **Long-term:** At $500k+ with 90% split, base case monthly income is ~$8,000-$11,000/month.
