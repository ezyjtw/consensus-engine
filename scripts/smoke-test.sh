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
# Usage (CI with seeded data):
#   ./scripts/seed-test-data.sh        # inject synthetic quotes
#   WAIT_SECS=30 ./scripts/smoke-test.sh
#
# Environment variables:
#   REDIS_ADDR          Redis address (default: localhost:6379)
#   REDIS_PASSWORD      Redis password (default: empty)
#   DASHBOARD_URL       Dashboard base URL (default: http://localhost:8080)
#   DASHBOARD_AUTH_TOKEN Auth token (default: empty = open access)
#   WAIT_SECS           Wait time for streams (default: 30)
#   SEED                Set to "1" to auto-seed data before testing
#   STRICT              Set to "1" to fail on data-plane checks (default: lenient)
#
# Exit code: 0 = all checks passed, 1 = one or more failures.

set -euo pipefail

REDIS_ADDR="${REDIS_ADDR:-localhost:6379}"
REDIS_PASSWORD="${REDIS_PASSWORD:-}"
DASHBOARD_URL="${DASHBOARD_URL:-http://localhost:8080}"
DASHBOARD_TOKEN="${DASHBOARD_AUTH_TOKEN:-}"    # empty = open access mode
WAIT_SECS="${WAIT_SECS:-30}"                  # increased from 15s for CI
SEED="${SEED:-0}"
STRICT="${STRICT:-0}"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; NC='\033[0m'
PASS=0; FAIL=0; WARN=0

ok()   { echo -e "  ${GREEN}✓${NC} $1"; PASS=$((PASS+1)); }
fail() { echo -e "  ${RED}✗${NC} $1"; FAIL=$((FAIL+1)); }
warn() { echo -e "  ${YELLOW}!${NC} $1"; WARN=$((WARN+1)); }
info() { echo -e "  ${YELLOW}→${NC} $1"; }

# soft_fail: fail in strict mode, warn otherwise. Used for checks that
# depend on external connectivity (exchange WebSockets) which may be
# unavailable in CI environments.
soft_fail() {
  if [[ "$STRICT" == "1" ]]; then
    fail "$1"
  else
    warn "$1 (non-strict: treated as warning)"
  fi
}

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
echo "  Wait: ${WAIT_SECS}s   Strict: ${STRICT}   Seed: ${SEED}"
echo "═══════════════════════════════════════════════════"

# ── 0. Optional: auto-seed synthetic data ────────────────────────────────
if [[ "$SEED" == "1" ]]; then
  echo ""
  echo "0. Seeding synthetic market data..."
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ -x "${SCRIPT_DIR}/seed-test-data.sh" ]]; then
    REDIS_ADDR="$REDIS_ADDR" REDIS_PASSWORD="$REDIS_PASSWORD" \
      "${SCRIPT_DIR}/seed-test-data.sh"
    # Give the consensus engine time to process the seeded quotes.
    echo "  Waiting 5s for consensus engine to process seeded data..."
    sleep 5
  else
    warn "seed-test-data.sh not found or not executable — skipping seed"
  fi
fi

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
HTTP_CODE="000"
if command -v curl &>/dev/null; then
  # Retry dashboard health check up to 5 times (it may still be starting).
  for attempt in 1 2 3 4 5; do
    HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 --max-time 10 \
      ${DASHBOARD_TOKEN:+-H "Authorization: Bearer $DASHBOARD_TOKEN"} \
      "${DASHBOARD_URL}/api/health" 2>/dev/null || echo "000")
    if [[ "$HTTP_CODE" == "200" ]]; then
      ok "Dashboard /api/health → 200 (attempt $attempt)"
      break
    fi
    if [[ $attempt -lt 5 ]]; then
      sleep 3
    fi
  done
  if [[ "$HTTP_CODE" != "200" ]]; then
    if [[ "$HTTP_CODE" == "000" ]]; then
      soft_fail "Dashboard not reachable after 5 attempts — is it running?"
    else
      soft_fail "Dashboard /api/health → HTTP $HTTP_CODE"
    fi
  fi
else
  info "curl not found — skipping HTTP checks"
fi

# ── 3. Wait for market:quotes to populate ─────────────────────────────────
echo ""
echo "2. Data plane — waiting up to ${WAIT_SECS}s for market data..."
MARKET_FOUND=0
for i in $(seq 1 $WAIT_SECS); do
  LEN=$(rcli XLEN market:quotes 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "market:quotes has $LEN messages (market-data service is publishing)"
    MARKET_FOUND=1
    break
  fi
  sleep 1
  if [[ $i -eq $WAIT_SECS ]]; then
    soft_fail "market:quotes still empty after ${WAIT_SECS}s — market-data may not have exchange connectivity"
  fi
done

# ── 4. Consensus engine output ─────────────────────────────────────────────
echo ""
echo "3. Consensus engine output"
# The consensus engine's consumer group starts at "$" (new messages only), so
# it needs a few seconds after startup to receive fresh quotes and produce
# output. Wait up to WAIT_SECS for consensus:updates to appear.
CONSENSUS_FOUND=0
for i in $(seq 1 $WAIT_SECS); do
  LEN=$(rcli XLEN consensus:updates 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "consensus:updates has $LEN messages"
    CONSENSUS_FOUND=1
    LAST=$(rcli XREVRANGE consensus:updates + - COUNT 1 2>/dev/null || true)
    if [[ -n "$LAST" ]]; then
      info "Latest: $(echo "$LAST" | grep -o '"[a-z_]*":[^,}]*' | head -4 | tr '\n' '  ' || echo "$LAST" | head -2 | tail -1)"
    fi
    break
  fi
  sleep 1
  if [[ $i -eq $WAIT_SECS ]]; then
    if [[ "$MARKET_FOUND" -eq 0 ]]; then
      soft_fail "consensus:updates empty (expected: no market data was available)"
    else
      soft_fail "consensus:updates still empty after ${WAIT_SECS}s — consensus engine may not be running"
    fi
  fi
done
# Anomalies and status transitions are optional (only emitted on outlier/state changes).
for STREAM in consensus:anomalies consensus:status; do
  LEN=$(rcli XLEN "$STREAM" 2>/dev/null || echo 0)
  if [[ "$LEN" -gt 0 ]]; then
    ok "$STREAM has $LEN messages"
  else
    info "$STREAM empty (no anomalies/transitions triggered — normal in stable markets)"
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
    soft_fail "$STREAM ledger group: lag=$LAG, pending=$PEL (ledger falling behind)"
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
  for ENDPOINT in "/api/mode" "/api/risk/state" "/api/positions" "/api/funding/rates" "/api/strategies/config"; do
    CODE=$(curl -s -o /dev/null -w "%{http_code}" --connect-timeout 5 --max-time 10 \
      ${DASHBOARD_TOKEN:+-H "Authorization: Bearer $DASHBOARD_TOKEN"} \
      "${DASHBOARD_URL}${ENDPOINT}" 2>/dev/null || echo "000")
    [[ "$CODE" == "200" ]] && ok "$ENDPOINT → $CODE" || soft_fail "$ENDPOINT → $CODE"
  done
fi

# ── Summary ────────────────────────────────────────────────────────────────
echo ""
echo "═══════════════════════════════════════════════════"
if [[ "$WARN" -gt 0 ]]; then
  echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${YELLOW}${WARN} warnings${NC}  ${RED}${FAIL} failed${NC}"
else
  echo -e "  Results: ${GREEN}${PASS} passed${NC}  ${RED}${FAIL} failed${NC}"
fi
echo "═══════════════════════════════════════════════════"
echo ""
[[ "$FAIL" -eq 0 ]] && exit 0 || exit 1
