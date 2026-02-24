#!/usr/bin/env bash
# smoke-test.sh — validate the full ArbSuite data plane end-to-end
#
# Usage (local compose stack):
#   DASHBOARD_MASTER_KEY=$(openssl rand -hex 32) docker compose up -d
#   ./scripts/smoke-test.sh
#
# Usage (remote Redis):
#   REDIS_ADDR=host:port REDIS_PASSWORD=secret ./scripts/smoke-test.sh
#
# Exit code: 0 = all checks passed, 1 = one or more failures.

set -euo pipefail

REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
REDIS_PASSWORD="${REDIS_PASSWORD:-}"
DASHBOARD_URL="${DASHBOARD_URL:-http://localhost:8080}"
DASHBOARD_TOKEN="${DASHBOARD_AUTH_TOKEN:-}"    # empty = open access mode
WAIT_SECS="${WAIT_SECS:-15}"                  # how long to wait for streams to populate

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0

ok()   { echo -e "  ${GREEN}✓${NC} $1"; PASS=$((PASS+1)); }
fail() { echo -e "  ${RED}✗${NC} $1"; FAIL=$((FAIL+1)); }
info() { echo -e "  ${YELLOW}→${NC} $1"; }

rcli() {
  if [[ -n "$REDIS_PASSWORD" ]]; then
    redis-cli -u "redis://:${REDIS_PASSWORD}@${REDIS_ADDR}" "$@" 2>/dev/null
  else
    redis-cli -h "${REDIS_ADDR%%:*}" -p "${REDIS_ADDR##*:}" "$@" 2>/dev/null
  fi
}

# ── Prerequisite: redis-cli must be available ──────────────────────────────
if ! command -v redis-cli &>/dev/null; then
  echo "redis-cli not found — install redis-tools (apt) or redis (brew)"
  exit 1
fi

echo ""
echo "═══════════════════════════════════════════════════"
echo "  ArbSuite Smoke Test  —  $(date '+%Y-%m-%d %H:%M:%S')"
echo "  Redis: $REDIS_ADDR   Dashboard: $DASHBOARD_URL"
echo "═══════════════════════════════════════════════════"

# ── 1. Redis reachable ─────────────────────────────────────────────────────
echo ""
echo "1. Infrastructure"
PONG=$(rcli PING 2>/dev/null || true)
if [[ "$PONG" == "PONG" ]]; then
  ok "Redis reachable ($REDIS_ADDR)"
else
  fail "Redis not reachable ($REDIS_ADDR) — PONG=$PONG"
  echo ""; echo "Aborting: fix Redis first."; exit 1
fi

# ── 2. Dashboard API reachable ─────────────────────────────────────────────
if command -v curl &>/dev/null; then
  AUTH_HEADER=""
  [[ -n "$DASHBOARD_TOKEN" ]] && AUTH_HEADER="-H 'Authorization: Bearer ${DASHBOARD_TOKEN}'"
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    ${DASHBOARD_TOKEN:+-H "Authorization: Bearer $DASHBOARD_TOKEN"} \
    "${DASHBOARD_URL}/api/health" 2>/dev/null || echo "000")
  if [[ "$HTTP_CODE" == "200" ]]; then
    ok "Dashboard /api/health → 200"
  elif [[ "$HTTP_CODE" == "000" ]]; then
    info "Dashboard not reachable (skipping HTTP checks) — is it running?"
  else
    fail "Dashboard /api/health → HTTP $HTTP_CODE"
  fi
else
  info "curl not found — skipping HTTP checks"
fi

# ── 3. Wait for market:quotes to populate ─────────────────────────────────
echo ""
echo "2. Data plane — waiting up to ${WAIT_SECS}s for market data..."
for i in $(seq 1 $WAIT_SECS); do
  LEN=$(rcli XLEN market:quotes 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "market:quotes has $LEN messages (market-data service is publishing)"
    break
  fi
  sleep 1
  [[ $i -eq $WAIT_SECS ]] && fail "market:quotes still empty after ${WAIT_SECS}s — is market-data running?"
done

# ── 4. Consensus engine output ─────────────────────────────────────────────
echo ""
echo "3. Consensus engine output"
for STREAM in consensus:updates consensus:anomalies consensus:status; do
  LEN=$(rcli XLEN "$STREAM" 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "$STREAM has $LEN messages"
    # Show latest message fields
    LAST=$(rcli XREVRANGE "$STREAM" + - COUNT 1 2>/dev/null || true)
    if [[ -n "$LAST" ]]; then
      info "Latest: $(echo "$LAST" | grep -o '"[a-z_]*":[^,}]*' | head -4 | tr '\n' '  ' || echo "$LAST" | head -2 | tail -1)"
    fi
  else
    fail "$STREAM is empty — consensus engine may not be running or market:quotes not flowing"
  fi
done

# ── 5. Strategy engines: arb + funding intents ─────────────────────────────
echo ""
echo "4. Strategy engines"
for STREAM in "trade:intents" "trade:intents:approved"; do
  LEN=$(rcli XLEN "$STREAM" 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "$STREAM has $LEN messages"
  else
    info "$STREAM empty — ok if no arb/funding opportunities have triggered yet"
  fi
done

# ── 6. Execution fills ─────────────────────────────────────────────────────
echo ""
echo "5. Execution router"
for STREAM in "execution:events" "demo:fills"; do
  LEN=$(rcli XLEN "$STREAM" 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "$STREAM has $LEN messages"
  else
    info "$STREAM empty — ok if no intents have been approved yet"
  fi
done

# ── 7. Risk daemon ─────────────────────────────────────────────────────────
echo ""
echo "6. Risk daemon"
for STREAM in "risk:state" "risk:alerts"; do
  LEN=$(rcli XLEN "$STREAM" 2>/dev/null || echo 0)
  [[ "$LEN" -gt 0 ]] && ok "$STREAM has $LEN messages" || info "$STREAM empty (no alerts triggered)"
done
RISK_MODE=$(rcli GET "risk:mode" 2>/dev/null || echo "")
[[ -n "$RISK_MODE" ]] && ok "risk:mode = $RISK_MODE" || info "risk:mode key not set (daemon may not have started)"
KILL=$(rcli EXISTS "kill:switch" 2>/dev/null || echo 0)
[[ "$KILL" == "0" ]] && ok "kill:switch is NOT active (safe)" || fail "kill:switch is ACTIVE — system is halted"

# ── 8. Ledger consumer group lag ──────────────────────────────────────────
echo ""
echo "7. Ledger consumer group lag"
for STREAM in "execution:events" "demo:fills" "risk:alerts" "risk:state" "consensus:status"; do
  RAW=$(rcli XINFO GROUPS "$STREAM" 2>/dev/null || true)
  if [[ -z "$RAW" ]]; then
    info "$STREAM — no consumer groups yet"
    continue
  fi
  # XINFO GROUPS outputs field-value pairs; extract lag for the "ledger" group
  LAG=$(echo "$RAW" | awk '/^lag$/{getline; print; exit}' 2>/dev/null || echo "?")
  PEL=$(echo "$RAW" | awk '/^pel-count$/{getline; print; exit}' 2>/dev/null || echo "?")
  if [[ "$LAG" == "0" || "$LAG" == "?" ]]; then
    ok "$STREAM ledger group: lag=$LAG, pending=$PEL"
  else
    fail "$STREAM ledger group: lag=$LAG, pending=$PEL (ledger falling behind)"
  fi
done

# ── 9. Venue state cache (restart-recovery keys) ──────────────────────────
echo ""
echo "8. Venue state cache"
KEY_COUNT=$(rcli KEYS "consensus:venue_state:*" 2>/dev/null | wc -l | tr -d ' ')
if [[ "$KEY_COUNT" -gt 0 ]]; then
  ok "$KEY_COUNT venue state cache keys present (restart recovery ready)"
  # Print a sample
  SAMPLE=$(rcli KEYS "consensus:venue_state:*" 2>/dev/null | head -1)
  [[ -n "$SAMPLE" ]] && info "Sample: $SAMPLE = $(rcli GET "$SAMPLE" 2>/dev/null | head -c 120)…"
else
  info "No venue state cache keys yet — will appear after first venue state change"
fi

# ── 10. Dashboard gateway API spot-checks ─────────────────────────────────
if command -v curl &>/dev/null && [[ "$HTTP_CODE" == "200" ]]; then
  echo ""
  echo "9. Dashboard gateway API"
  for ENDPOINT in "/api/mode" "/api/risk/state" "/api/positions" "/api/funding/rates"; do
    CODE=$(curl -s -o /dev/null -w "%{http_code}" \
      ${DASHBOARD_TOKEN:+-H "Authorization: Bearer $DASHBOARD_TOKEN"} \
      "${DASHBOARD_URL}${ENDPOINT}" 2>/dev/null || echo "000")
    [[ "$CODE" == "200" ]] && ok "$ENDPOINT → $CODE" || fail "$ENDPOINT → $CODE"
  done
fi

# ── Summary ────────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════"
echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
echo "═══════════════════════════════════════════════════"
echo ""
[[ "$FAIL" -eq 0 ]] && exit 0 || exit 1
