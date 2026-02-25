#!/usr/bin/env bash
# seed-test-data.sh — publish synthetic market quotes into Redis so the
# consensus engine has data to process in CI / offline environments.
#
# This script publishes realistic quotes for all configured venues and
# symbols to the market:quotes stream. The consensus engine (which reads
# via XReadGroup with a consumer group created at "$") will pick up
# these messages as long as the engine is already running when the seed
# script executes.
#
# Usage:
#   ./scripts/seed-test-data.sh                        # defaults
#   REDIS_ADDR=host:port ./scripts/seed-test-data.sh   # custom Redis
#   ROUNDS=5 INTERVAL=0.5 ./scripts/seed-test-data.sh  # 5 rounds, 0.5s apart
#
# Typically called from CI after `docker compose up` and a short sleep.

set -euo pipefail

REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
REDIS_PASSWORD="${REDIS_PASSWORD:-}"
STREAM="${STREAM:-market:quotes}"
ROUNDS="${ROUNDS:-3}"
INTERVAL="${INTERVAL:-1}"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'

rcli() {
  if [[ -n "$REDIS_PASSWORD" ]]; then
    redis-cli -u "redis://:${REDIS_PASSWORD}@${REDIS_ADDR}" "$@" 2>/dev/null
  else
    redis-cli -h "${REDIS_ADDR%%:*}" -p "${REDIS_ADDR##*:}" "$@" 2>/dev/null
  fi
}

# Verify Redis is reachable.
PONG=$(rcli PING 2>/dev/null || true)
if [[ "$PONG" != "PONG" ]]; then
  echo "ERROR: Redis not reachable at $REDIS_ADDR"
  exit 1
fi

# Base prices for each symbol (approximate real-world values).
declare -A BASE_PRICES
BASE_PRICES[BTC-PERP]=95000.00
BASE_PRICES[ETH-PERP]=3200.00
BASE_PRICES[SOL-PERP]=180.00
BASE_PRICES[DOGE-PERP]=0.35
BASE_PRICES[XRP-PERP]=2.40
BASE_PRICES[AVAX-PERP]=38.00
BASE_PRICES[LINK-PERP]=22.00
BASE_PRICES[SUI-PERP]=4.20
BASE_PRICES[PEPE-PERP]=0.000022

# Venues with their taker fee bps.
VENUES=("binance" "okx" "bybit" "deribit")
FEE_BPS=("4.0" "5.0" "5.5" "5.0")

SYMBOLS=("BTC-PERP" "ETH-PERP" "SOL-PERP" "DOGE-PERP" "XRP-PERP" "AVAX-PERP" "LINK-PERP" "SUI-PERP" "PEPE-PERP")

echo -e "${YELLOW}Seeding $STREAM with synthetic quotes (${ROUNDS} rounds, ${INTERVAL}s interval)${NC}"

publish_quote() {
  local venue=$1 symbol=$2 base=$3 fee_bps=$4 round=$5
  local ts_ms
  ts_ms=$(date +%s%3N 2>/dev/null || python3 -c 'import time; print(int(time.time()*1000))')

  # Add tiny per-venue and per-round jitter to make quotes realistic.
  # Venue offsets: binance=0, okx=+0.01%, bybit=-0.005%, deribit=+0.02%
  local jitter="0"
  case "$venue" in
    binance) jitter="0" ;;
    okx)     jitter="0.0001" ;;
    bybit)   jitter="-0.00005" ;;
    deribit) jitter="0.0002" ;;
  esac

  # Use awk for floating point math (portable, no bc dependency).
  local bid ask mid spread_half
  spread_half=$(echo "$base" | awk '{printf "%.8f", $1 * 0.0002}')
  mid=$(echo "$base $jitter" | awk '{printf "%.8f", $1 * (1 + $2)}')
  bid=$(echo "$mid $spread_half" | awk '{printf "%.8f", $1 - $2}')
  ask=$(echo "$mid $spread_half" | awk '{printf "%.8f", $1 + $2}')

  local json
  json=$(cat <<EOJSON
{"schema_version":1,"tenant_id":"default","venue":"${venue}","symbol":"${symbol}","ts_ms":${ts_ms},"best_bid":${bid},"best_ask":${ask},"mark":${mid},"index":${mid},"bid_depth_1pct":5000000,"ask_depth_1pct":5000000,"fee_bps_taker":${fee_bps},"funding_rate":0.0001,"feed_health":{"ws_connected":true,"last_msg_ts_ms":${ts_ms}}}
EOJSON
  )

  rcli XADD "$STREAM" '*' data "$json" > /dev/null
}

total=0
for round in $(seq 1 "$ROUNDS"); do
  for sym in "${SYMBOLS[@]}"; do
    base="${BASE_PRICES[$sym]}"
    for i in "${!VENUES[@]}"; do
      publish_quote "${VENUES[$i]}" "$sym" "$base" "${FEE_BPS[$i]}" "$round"
      total=$((total + 1))
    done
  done
  echo -e "  ${GREEN}Round ${round}/${ROUNDS}${NC}: published ${#SYMBOLS[@]}x${#VENUES[@]} quotes (total: $total)"
  if [[ "$round" -lt "$ROUNDS" ]]; then
    sleep "$INTERVAL"
  fi
done

echo -e "${GREEN}Done: seeded $total quotes into $STREAM${NC}"
# Verify stream length.
LEN=$(rcli XLEN "$STREAM" 2>/dev/null || echo "?")
echo -e "  Stream length: $LEN"
