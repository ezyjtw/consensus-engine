# Redis Stream Event Schemas

> All events are published as JSON in the `data` field of Redis Stream entries.
> Every event includes `schema_version` (int) and `tenant_id` for multi-tenant isolation.
> Current schema version: **1**. Consumers should accept version 0 (legacy) or 1.

## Stream Map

| Stream | Producer | Consumer(s) | Go Type |
|---|---|---|---|
| `market:quotes` | market-data | consensus-engine | `consensus.Quote` |
| `consensus:updates` | consensus-engine | arb-engine, funding-engine, execution-router, dashboard | `consensus.ConsensusUpdate` |
| `consensus:anomalies` | consensus-engine | risk-daemon, dashboard | `consensus.VenueAnomaly` |
| `consensus:status` | consensus-engine | risk-daemon, ledger, dashboard | `consensus.VenueStatusUpdate` |
| `trade:intents` | arb-engine, funding-engine | capital-allocator | `arb.TradeIntent` |
| `trade:intents:approved` | capital-allocator | execution-router | `arb.TradeIntent` |
| `execution:events` | execution-router | risk-daemon, ledger, funding-engine | `execution.ExecutionEvent` |
| `demo:fills` | execution-router (PAPER) | capital-allocator, ledger, dashboard | `execution.SimulatedFill` |
| `live:fills` | execution-router (LIVE) | capital-allocator, ledger, dashboard | `execution.SimulatedFill` |
| `risk:state` | risk-daemon | dashboard, ledger | `risk.State` |
| `risk:alerts` | risk-daemon | dashboard, ledger | `risk.Alert` |
| `treasury:deposits` | treasury | ledger, dashboard | `treasury.DepositEvent` |
| `treasury:conversions` | treasury | ledger, dashboard | `treasury.ConversionEvent` |
| `treasury:distributions` | treasury | ledger, dashboard | `treasury.DistributionEvent` |
| `treasury:sweeps` | treasury | ledger, dashboard | `treasury.SweepEvent` |
| `treasury:reconciliation` | treasury | ledger, dashboard | `treasury.ReconciliationReport` |

## Redis Keys (Non-Stream)

| Key | Type | Writer | Description |
|---|---|---|---|
| `kill:switch` | exists/absent | dashboard | Kill switch — when set, all services pause |
| `risk:mode` | string | risk-daemon | Current mode: RUNNING / PAUSED / SAFE / FLATTEN / HALTED |
| `risk:state:json` | string (JSON) | risk-daemon | Full risk state snapshot |
| `risk:commanded_mode` | string | dashboard | Operator-requested mode transition |
| `consensus:venue_state:{tenant}:{venue}:{symbol}` | string (JSON) | consensus-engine | Cached venue state for restart recovery |
| `consensus:quality:{symbol}` | string | consensus-engine | Latest quality rating (HIGH/MED/LOW) |
| `paper:pos:{tenant}:{venue}:{symbol}:{market}` | string (JSON) | execution-router | Paper trading position state |
| `treasury:expected:{tenant}:{venue}` | string (float) | treasury | Expected balance per venue |

---

## Event Schemas

### market:quotes — `consensus.Quote`

```json
{
  "schema_version": 1,
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
  "funding_rate": 0.0001,
  "feed_health": {
    "ws_connected": true,
    "last_msg_ts_ms": 1700000000000
  }
}
```

### consensus:updates — `consensus.ConsensusUpdate`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "symbol": "BTC-PERP",
  "ts_ms": 1700000000100,
  "size_notional_usd": 10000.0,
  "consensus": {
    "mid": 100005.0,
    "buy_exec": 100010.0,
    "sell_exec": 100000.0,
    "band_low": 99990.0,
    "band_high": 100020.0,
    "quality": "HIGH"
  },
  "venues": [
    {
      "venue": "binance",
      "status": "OK",
      "trust": 0.95,
      "mid": 100005.0,
      "buy_exec": 100010.0,
      "sell_exec": 100000.0,
      "effective_buy": 100012.0,
      "effective_sell": 99998.0,
      "deviation_bps": 0.5,
      "flags": []
    }
  ]
}
```

### consensus:anomalies — `consensus.VenueAnomaly`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "symbol": "BTC-PERP",
  "venue": "okx",
  "ts_ms": 1700000000200,
  "anomaly_type": "OUTLIER",
  "severity": "WARN",
  "deviation_bps": 35.2,
  "consensus_mid": 100005.0,
  "venue_mid": 100040.0,
  "window_ms": 1500,
  "recommended_action": "reduce_trust"
}
```

### consensus:status — `consensus.VenueStatusUpdate`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "venue": "okx",
  "symbol": "BTC-PERP",
  "ts_ms": 1700000000300,
  "status": "WARN",
  "ttl_ms": 60000,
  "reason": "deviation 35.2bps > warn threshold 20bps"
}
```

### trade:intents — `arb.TradeIntent`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "intent_id": "uuid-v4",
  "strategy": "CROSS_VENUE_ARB",
  "symbol": "BTC-PERP",
  "ts_ms": 1700000000400,
  "expires_ms": 1700000003400,
  "legs": [
    {
      "venue": "binance",
      "action": "BUY",
      "type": "MARKET_OR_IOC",
      "market": "PERP",
      "notional_usd": 10000.0,
      "max_slippage_bps": 8.0,
      "price_limit": 100080.0
    },
    {
      "venue": "okx",
      "action": "SELL",
      "type": "MARKET_OR_IOC",
      "market": "PERP",
      "notional_usd": 10000.0,
      "max_slippage_bps": 8.0,
      "price_limit": 99920.0
    }
  ],
  "expected": {
    "edge_bps_gross": 14.2,
    "edge_bps_net": 6.2,
    "profit_usd_net": 6.20,
    "fees_usd_est": 8.0,
    "slippage_usd_est": 4.0
  },
  "constraints": {
    "min_quality": "MED",
    "require_venue_ok": true,
    "max_age_ms": 3000,
    "hedge_preference": "SIMULTANEOUS_OR_HEDGE_FIRST",
    "cooldown_key": "binance:okx:BTC-PERP"
  },
  "debug": {
    "consensus_band_low": 99990.0,
    "consensus_band_high": 100020.0,
    "buy_on": "binance",
    "sell_on": "okx",
    "buy_exec": 100010.0,
    "sell_exec": 100095.0
  }
}
```

### execution:events — `execution.ExecutionEvent`

```json
{
  "schema_version": 1,
  "event_type": "ORDER_FILLED",
  "intent_id": "uuid-v4",
  "leg_index": 0,
  "venue": "binance",
  "symbol": "BTC-PERP",
  "action": "BUY",
  "strategy": "CROSS_VENUE_ARB",
  "market": "PERP",
  "requested_notional_usd": 10000.0,
  "filled_notional_usd": 10000.0,
  "filled_price": 99892.5,
  "slippage_bps_actual": 2.1,
  "slippage_bps_allowed": 8.0,
  "fees_usd_actual": 3.99,
  "ts_ms": 1700000000789,
  "latency_signal_to_fill_ms": 145,
  "tenant_id": "default",
  "mode": "PAPER"
}
```

**Event types:** `ORDER_FILLED`, `ORDER_REJECTED`, `HEDGE_FAILED`, `LEG_PARTIAL`, `HEDGE_DRIFT`

### demo:fills / live:fills — `execution.SimulatedFill`

```json
{
  "schema_version": 1,
  "intent_id": "uuid-v4",
  "strategy": "CROSS_VENUE_ARB",
  "symbol": "BTC-PERP",
  "legs": [
    {
      "venue": "binance",
      "action": "BUY",
      "filled_notional_usd": 10000.0,
      "filled_price": 99891.0
    },
    {
      "venue": "okx",
      "action": "SELL",
      "filled_notional_usd": 10000.0,
      "filled_price": 100095.0
    }
  ],
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
  "mode": "PAPER",
  "tenant_id": "default"
}
```

### risk:state — `risk.State`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "mode": "RUNNING",
  "ts_ms": 1700000000500,
  "net_delta_usd": 150.0,
  "drawdown_pct": 0.8,
  "peak_equity_usd": 100000.0,
  "current_equity_usd": 99200.0,
  "hedge_drift_usd_sec": 0.0,
  "error_rate_5m_pct": 2.0,
  "blacklisted_venues": [],
  "reason": "",
  "adl_risk_pct": 0.0,
  "liq_cluster_risk": 0,
  "venue_delev_event_count": 0
}
```

**Risk modes:** `RUNNING` → `PAUSED` → `SAFE` → `FLATTEN` → `HALTED`

### risk:alerts — `risk.Alert`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "ts_ms": 1700000000600,
  "source": "risk-daemon",
  "severity": "WARN",
  "message": "Drawdown 3.2% exceeds threshold 3.0%",
  "metric": "drawdown_pct",
  "value": 3.2,
  "threshold": 3.0
}
```

**Severities:** `INFO`, `WARN`, `CRITICAL`

### treasury:deposits — `treasury.DepositEvent`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "deposit_id": "dep-123",
  "source": "coinbase",
  "asset": "GBP",
  "amount": 5000.0,
  "tx_id": "",
  "status": "DETECTED",
  "ts_ms": 1700000000700
}
```

### treasury:distributions — `treasury.DistributionEvent`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "deposit_id": "dep-123",
  "total_amount": 6500.0,
  "legs": [
    {
      "venue": "binance",
      "asset": "USDC",
      "amount": 3250.0,
      "network": "arbitrum",
      "withdraw_id": "w-abc",
      "status": "SENT"
    }
  ],
  "ts_ms": 1700000000800
}
```

### treasury:sweeps — `treasury.SweepEvent`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "from_venue": "binance",
  "to_venue": "coinbase",
  "asset": "USDC",
  "amount": 1500.0,
  "withdraw_id": "w-xyz",
  "status": "SENT",
  "ts_ms": 1700000000900
}
```

### treasury:reconciliation — `treasury.ReconciliationReport`

```json
{
  "schema_version": 1,
  "tenant_id": "default",
  "ts_ms": 1700000001000,
  "total_usd": 50000.0,
  "venues": [
    {
      "venue": "binance",
      "balance_usd": 25000.0,
      "position_usd": 10000.0,
      "expected_usd": 24000.0,
      "drift_usd": 1000.0,
      "drift_pct": 4.17
    }
  ],
  "healthy": true,
  "alerts": []
}
```
