package ledger

// Schema is the full Postgres DDL for the ArbSuite ledger.
// Applied on startup via CREATE TABLE IF NOT EXISTS (idempotent).
const Schema = `
CREATE TABLE IF NOT EXISTS trade_intents (
    id          UUID PRIMARY KEY,
    tenant_id   TEXT NOT NULL,
    strategy    TEXT NOT NULL,
    symbol      TEXT NOT NULL,
    payload     JSONB NOT NULL,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS orders (
    id          UUID PRIMARY KEY,
    intent_id   UUID NOT NULL,
    venue       TEXT NOT NULL,
    symbol      TEXT NOT NULL,
    action      TEXT NOT NULL,
    status      TEXT NOT NULL,
    payload     JSONB NOT NULL,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS fills (
    id              UUID PRIMARY KEY,
    order_id        UUID,
    intent_id       UUID NOT NULL,
    strategy        TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    price           DOUBLE PRECISION NOT NULL,
    notional        DOUBLE PRECISION NOT NULL,
    fees            DOUBLE PRECISION NOT NULL,
    slippage_bps    DOUBLE PRECISION NOT NULL,
    net_pnl_usd     DOUBLE PRECISION NOT NULL,
    mode            TEXT NOT NULL,
    tenant_id       TEXT NOT NULL,
    ts              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS positions_snapshots (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       TEXT NOT NULL,
    venue           TEXT NOT NULL,
    symbol          TEXT NOT NULL,
    notional        DOUBLE PRECISION NOT NULL,
    entry_price     DOUBLE PRECISION NOT NULL,
    unrealised_pnl  DOUBLE PRECISION NOT NULL DEFAULT 0,
    mode            TEXT NOT NULL DEFAULT 'PAPER',
    ts              TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS risk_state_snapshots (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    mode        TEXT NOT NULL,
    metrics     JSONB NOT NULL,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS alerts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    source      TEXT NOT NULL,
    severity    TEXT NOT NULL,
    message     TEXT NOT NULL,
    payload     JSONB NOT NULL,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS venue_status_history (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    venue       TEXT NOT NULL,
    symbol      TEXT NOT NULL,
    from_state  TEXT NOT NULL,
    to_state    TEXT NOT NULL,
    reason      TEXT,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS audit_log (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    payload     JSONB NOT NULL,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS funding_payments (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   TEXT NOT NULL,
    venue       TEXT NOT NULL,
    symbol      TEXT NOT NULL,
    amount_usd  DOUBLE PRECISION NOT NULL,
    rate_bps    DOUBLE PRECISION NOT NULL,
    ts          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── V3: RBAC API keys ──────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS api_keys (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id    TEXT        NOT NULL,
    name         TEXT        NOT NULL,
    key_prefix   TEXT        NOT NULL,      -- first ~11 chars for display
    key_hash     TEXT        NOT NULL UNIQUE, -- SHA-256 of full key
    role         TEXT        NOT NULL,      -- admin/trader/viewer/auditor
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ
);

-- ── V3: Tenant branding ────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS tenants (
    id            TEXT        PRIMARY KEY,
    name          TEXT        NOT NULL,
    logo_url      TEXT        NOT NULL DEFAULT '',
    primary_color TEXT        NOT NULL DEFAULT '#3b82f6',
    accent_color  TEXT        NOT NULL DEFAULT '#f97316',
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── V3: Enhanced audit_log (add ip_address + role columns) ────────────────
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS ip_address TEXT;
ALTER TABLE audit_log ADD COLUMN IF NOT EXISTS role       TEXT;

-- Indexes for common query patterns.
CREATE INDEX IF NOT EXISTS idx_fills_tenant_ts      ON fills (tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_fills_strategy       ON fills (strategy, ts DESC);
CREATE INDEX IF NOT EXISTS idx_alerts_severity      ON alerts (severity, ts DESC);
CREATE INDEX IF NOT EXISTS idx_risk_snapshots_ts    ON risk_state_snapshots (tenant_id, ts DESC);
CREATE INDEX IF NOT EXISTS idx_positions_venue      ON positions_snapshots (tenant_id, venue, ts DESC);
CREATE INDEX IF NOT EXISTS idx_api_keys_tenant      ON api_keys (tenant_id);
CREATE INDEX IF NOT EXISTS idx_audit_log_tenant_ts  ON audit_log (tenant_id, ts DESC);
`
