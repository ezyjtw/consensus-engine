package ledger

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/execution"
	"github.com/ezyjtw/consensus-engine/internal/risk"
)

// DB wraps a pgx connection pool and provides append-only write methods.
type DB struct {
	pool *pgxpool.Pool
}

// Connect opens a connection pool to Postgres and applies the schema.
func Connect(ctx context.Context, dsn string) (*DB, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("pgx connect: %w", err)
	}
	db := &DB{pool: pool}
	if err := db.applySchema(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return db, nil
}

func (db *DB) applySchema(ctx context.Context) error {
	_, err := db.pool.Exec(ctx, Schema)
	return err
}

// Close shuts down the connection pool.
func (db *DB) Close() {
	db.pool.Close()
}

// WriteIntent persists an approved intent to trade_intents.
func (db *DB) WriteIntent(ctx context.Context, intent arb.TradeIntent) error {
	payload, _ := json.Marshal(intent)
	_, err := db.pool.Exec(ctx,
		`INSERT INTO trade_intents (id, tenant_id, strategy, symbol, payload, ts)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (id) DO NOTHING`,
		intent.IntentID, intent.TenantID, intent.Strategy, intent.Symbol,
		string(payload), time.UnixMilli(intent.TsMs).UTC(),
	)
	return err
}

// WriteFill persists a simulated fill.
func (db *DB) WriteFill(ctx context.Context, fill *execution.SimulatedFill) error {
	avgPrice := (fill.FillPriceBuy + fill.FillPriceSell) / 2
	_, err := db.pool.Exec(ctx,
		`INSERT INTO fills
		 (id, intent_id, strategy, symbol, price, notional, fees, slippage_bps, net_pnl_usd, mode, tenant_id, ts)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		newUUID(), fill.IntentID, fill.Strategy, fill.Symbol,
		avgPrice,
		fill.NetPnLUSD+fill.FeesAssumedUSD,
		fill.FeesAssumedUSD,
		fill.SlippageAssumedBps,
		fill.NetPnLUSD,
		fill.Mode,
		fill.TenantID,
		time.UnixMilli(fill.TsFillSimulatedMs).UTC(),
	)
	return err
}

// WriteExecutionEvent persists an execution event as an order record.
func (db *DB) WriteExecutionEvent(ctx context.Context, ev execution.ExecutionEvent) error {
	payload, _ := json.Marshal(ev)
	_, err := db.pool.Exec(ctx,
		`INSERT INTO orders
		 (id, intent_id, venue, symbol, action, status, payload, ts)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		newUUID(), ev.IntentID, ev.Venue, ev.Symbol, ev.Action, ev.EventType,
		string(payload), time.UnixMilli(ev.TsMs).UTC(),
	)
	return err
}

// WriteRiskState persists a risk state snapshot.
func (db *DB) WriteRiskState(ctx context.Context, state risk.State) error {
	payload, _ := json.Marshal(state)
	_, err := db.pool.Exec(ctx,
		`INSERT INTO risk_state_snapshots (tenant_id, mode, metrics, ts)
		 VALUES ($1, $2, $3, $4)`,
		state.TenantID, string(state.Mode), string(payload),
		time.UnixMilli(state.TsMs).UTC(),
	)
	return err
}

// WriteAlert persists a risk alert.
func (db *DB) WriteAlert(ctx context.Context, alert risk.Alert) error {
	payload, _ := json.Marshal(alert)
	_, err := db.pool.Exec(ctx,
		`INSERT INTO alerts (tenant_id, source, severity, message, payload, ts)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		alert.TenantID, alert.Source, alert.Severity, alert.Message,
		string(payload), time.UnixMilli(alert.TsMs).UTC(),
	)
	return err
}

// AuditLog appends an immutable audit entry.
func (db *DB) AuditLog(ctx context.Context, tenantID, actor, action string, payload interface{}) error {
	data, _ := json.Marshal(payload)
	_, err := db.pool.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, actor, action, payload)
		 VALUES ($1, $2, $3, $4)`,
		tenantID, actor, action, string(data),
	)
	return err
}

// ── Query helpers for the Gateway API ─────────────────────────────────────

// RecentFills returns the N most recent fills for a tenant as generic maps.
func (db *DB) RecentFills(ctx context.Context, tenantID string, limit int) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id::text, intent_id::text, strategy, symbol, price, notional, fees,
		        slippage_bps, net_pnl_usd, mode, ts
		 FROM fills WHERE tenant_id = $1
		 ORDER BY ts DESC LIMIT $2`,
		tenantID, limit,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}

// PnLSummary returns cumulative PnL grouped by strategy.
func (db *DB) PnLSummary(ctx context.Context, tenantID string) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT strategy,
		        COALESCE(SUM(net_pnl_usd),0) AS total_pnl,
		        COUNT(*)                      AS fill_count,
		        COALESCE(SUM(fees),0)         AS total_fees
		 FROM fills WHERE tenant_id = $1
		 GROUP BY strategy`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}

// RecentAlerts returns the N most recent alerts for a tenant.
func (db *DB) RecentAlerts(ctx context.Context, tenantID string, limit int) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id::text, source, severity, message, payload::text, ts
		 FROM alerts WHERE tenant_id = $1
		 ORDER BY ts DESC LIMIT $2`,
		tenantID, limit,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}

// collectRows turns pgx rows into a slice of column-name → value maps.
func collectRows(rows pgx.Rows) []map[string]interface{} {
	defer rows.Close()
	fields := rows.FieldDescriptions()
	var result []map[string]interface{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			continue
		}
		row := make(map[string]interface{}, len(fields))
		for i, f := range fields {
			row[string(f.Name)] = vals[i]
		}
		result = append(result, row)
	}
	return result
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// WriteVenueStatus appends a venue state transition to venue_status_history.
func (db *DB) WriteVenueStatus(ctx context.Context, su consensus.VenueStatusUpdate, prevState string) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO venue_status_history (tenant_id, venue, symbol, from_state, to_state, reason, ts)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		su.TenantID, string(su.Venue), string(su.Symbol),
		prevState, string(su.Status), su.Reason,
		time.UnixMilli(su.TsMs).UTC(),
	)
	return err
}

// LatestVenueStates returns the most recent state for every venue/symbol pair.
// Used by the consensus engine to restore trust state after a restart.
func (db *DB) LatestVenueStates(ctx context.Context, tenantID string) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT DISTINCT ON (tenant_id, venue, symbol)
		        tenant_id, venue, symbol, to_state AS state, reason, ts
		 FROM venue_status_history
		 WHERE tenant_id = $1
		 ORDER BY tenant_id, venue, symbol, ts DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}
