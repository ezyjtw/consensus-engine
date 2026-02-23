package ledger

import (
	"context"
	"encoding/json"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/auth"
)

// ── API key management ─────────────────────────────────────────────────────

// CreateAPIKey inserts a new hashed API key record and returns its UUID.
func (db *DB) CreateAPIKey(ctx context.Context, tenantID, name string, role auth.Role, keyPrefix, keyHash string) (string, error) {
	id := newUUID()
	_, err := db.pool.Exec(ctx,
		`INSERT INTO api_keys (id, tenant_id, name, key_prefix, key_hash, role)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		id, tenantID, name, keyPrefix, keyHash, string(role),
	)
	if err != nil {
		return "", err
	}
	return id, nil
}

// ValidateAPIKey looks up a key by hash, updates last_used_at, and returns the APIKey.
// Returns nil, nil if the key does not exist (caller should return 401).
func (db *DB) ValidateAPIKey(ctx context.Context, keyHash string) (*auth.APIKey, error) {
	row := db.pool.QueryRow(ctx,
		`UPDATE api_keys SET last_used_at = NOW()
		 WHERE key_hash = $1
		 RETURNING id::text, tenant_id, name, key_prefix, role`,
		keyHash,
	)
	var key auth.APIKey
	var roleStr string
	if err := row.Scan(&key.ID, &key.TenantID, &key.Name, &key.KeyPrefix, &roleStr); err != nil {
		return nil, nil // not found → 401
	}
	key.Role = auth.Role(roleStr)
	return &key, nil
}

// ListAPIKeys returns all API key metadata for a tenant (key hashes are never returned).
func (db *DB) ListAPIKeys(ctx context.Context, tenantID string) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id::text, name, key_prefix, role, created_at, last_used_at
		 FROM api_keys WHERE tenant_id = $1
		 ORDER BY created_at DESC`,
		tenantID,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}

// DeleteAPIKey removes an API key by UUID, scoped to the tenant.
func (db *DB) DeleteAPIKey(ctx context.Context, tenantID, keyID string) error {
	_, err := db.pool.Exec(ctx,
		`DELETE FROM api_keys WHERE id = $1 AND tenant_id = $2`,
		keyID, tenantID,
	)
	return err
}

// ── Tenant branding ────────────────────────────────────────────────────────

// GetTenantBranding retrieves branding for a tenant, returning defaults if not configured.
func (db *DB) GetTenantBranding(ctx context.Context, tenantID string) (map[string]interface{}, error) {
	row := db.pool.QueryRow(ctx,
		`SELECT id, name, logo_url, primary_color, accent_color FROM tenants WHERE id = $1`,
		tenantID,
	)
	var id, name, logoURL, primaryColor, accentColor string
	if err := row.Scan(&id, &name, &logoURL, &primaryColor, &accentColor); err != nil {
		return map[string]interface{}{
			"id":            tenantID,
			"name":          "ArbSuite",
			"logo_url":      "",
			"primary_color": "#3b82f6",
			"accent_color":  "#f97316",
		}, nil
	}
	return map[string]interface{}{
		"id":            id,
		"name":          name,
		"logo_url":      logoURL,
		"primary_color": primaryColor,
		"accent_color":  accentColor,
	}, nil
}

// UpsertTenantBranding creates or updates tenant branding configuration.
func (db *DB) UpsertTenantBranding(ctx context.Context, tenantID, name, logoURL, primaryColor, accentColor string) error {
	_, err := db.pool.Exec(ctx,
		`INSERT INTO tenants (id, name, logo_url, primary_color, accent_color, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (id) DO UPDATE SET
		   name          = EXCLUDED.name,
		   logo_url      = EXCLUDED.logo_url,
		   primary_color = EXCLUDED.primary_color,
		   accent_color  = EXCLUDED.accent_color,
		   updated_at    = NOW()`,
		tenantID, name, logoURL, primaryColor, accentColor,
	)
	return err
}

// ── Enhanced audit logging ─────────────────────────────────────────────────

// AuditLogRich appends an immutable audit entry with IP address and role.
func (db *DB) AuditLogRich(ctx context.Context, tenantID, actor, role, ipAddr, action string, payload interface{}) error {
	data, _ := json.Marshal(payload)
	_, err := db.pool.Exec(ctx,
		`INSERT INTO audit_log (tenant_id, actor, role, ip_address, action, payload)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, actor, role, ipAddr, action, string(data),
	)
	return err
}

// ExportAuditLog returns audit log entries within a time range for export.
func (db *DB) ExportAuditLog(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id::text,
		        actor,
		        COALESCE(role, '')       AS role,
		        COALESCE(ip_address, '') AS ip_address,
		        action,
		        payload::text            AS payload,
		        ts
		 FROM audit_log
		 WHERE tenant_id = $1
		   AND ts >= $2
		   AND ts <= $3
		 ORDER BY ts DESC
		 LIMIT $4`,
		tenantID, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}

// ── Fill export for reporting ──────────────────────────────────────────────

// ExportFills returns fills within a time range for CSV export.
func (db *DB) ExportFills(ctx context.Context, tenantID string, from, to time.Time, limit int) ([]map[string]interface{}, error) {
	rows, err := db.pool.Query(ctx,
		`SELECT id::text, intent_id::text, strategy, symbol,
		        price, notional, fees, slippage_bps, net_pnl_usd, mode, ts
		 FROM fills
		 WHERE tenant_id = $1
		   AND ts >= $2
		   AND ts <= $3
		 ORDER BY ts DESC
		 LIMIT $4`,
		tenantID, from, to, limit,
	)
	if err != nil {
		return nil, err
	}
	return collectRows(rows), nil
}
