package dashboard

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ezyjtw/consensus-engine/internal/auth"
	"github.com/ezyjtw/consensus-engine/internal/ledger"
)

// Gateway extends the dashboard with the full V1/V3 REST API surface.
type Gateway struct {
	rdb      *redis.Client
	db       *ledger.DB // nil if Postgres not configured
	tenantID string
}

func NewGateway(rdb *redis.Client, db *ledger.DB, tenantID string) *Gateway {
	if tenantID == "" {
		tenantID = "default"
	}
	return &Gateway{rdb: rdb, db: db, tenantID: tenantID}
}

// RegisterGateway attaches all gateway endpoints to the server mux and wires the DB.
func (s *Server) RegisterGateway(gw *Gateway) {
	s.gw = gw
	s.SetDB(gw.db)

	// Risk / mode
	s.mux.HandleFunc("GET /api/mode", s.auth(gw.handleGetMode))
	s.mux.HandleFunc("POST /api/mode/pause", s.authRole(auth.RoleTrader, gw.handleSetMode("PAUSED")))
	s.mux.HandleFunc("POST /api/mode/safe", s.authRole(auth.RoleTrader, gw.handleSetMode("SAFE")))
	s.mux.HandleFunc("POST /api/mode/flatten", s.authRole(auth.RoleTrader, gw.handleSetMode("FLATTEN")))
	s.mux.HandleFunc("POST /api/mode/running", s.authRole(auth.RoleTrader, gw.handleSetMode("RUNNING")))
	s.mux.HandleFunc("GET /api/risk/state", s.auth(gw.handleGetRiskState))
	s.mux.HandleFunc("GET /api/risk/history", s.auth(gw.handleGetRiskHistory))
	s.mux.HandleFunc("GET /api/risk/alerts", s.auth(gw.handleGetRiskAlerts))

	// PnL
	s.mux.HandleFunc("GET /api/pnl", s.auth(gw.handleGetPnL))
	s.mux.HandleFunc("GET /api/pnl/attribution", s.auth(gw.handleGetPnLAttribution))
	s.mux.HandleFunc("GET /api/metrics/kpi", s.auth(gw.handleGetKPI))

	// Positions (Redis-based paper positions)
	s.mux.HandleFunc("GET /api/positions", s.auth(gw.handleGetPositions))

	// Intents / orders
	s.mux.HandleFunc("GET /api/intents", s.auth(gw.handleGetIntents))
	s.mux.HandleFunc("GET /api/orders", s.auth(gw.handleGetOrders))

	// Live prices
	s.mux.HandleFunc("GET /api/prices", s.auth(gw.handleGetPrices))

	// Funding rates
	s.mux.HandleFunc("GET /api/funding/rates", s.auth(gw.handleGetFundingRates))

	// Equity curve (cumulative PnL time series)
	s.mux.HandleFunc("GET /api/equity-curve", s.auth(gw.handleGetEquityCurve))

	// Paper trading mode
	s.mux.HandleFunc("GET /api/paper/mode", s.auth(gw.handleGetPaperMode))
	s.mux.HandleFunc("PUT /api/paper/mode", s.authRole(auth.RoleTrader, gw.handleSetPaperMode))

	// ── V3: Identity ──────────────────────────────────────────────────────
	s.mux.HandleFunc("GET /api/auth/me", s.auth(gw.handleAuthMe))

	// ── V3: API key management (admin only) ───────────────────────────────
	s.mux.HandleFunc("GET /api/auth/keys", s.authRole(auth.RoleAdmin, gw.handleListAPIKeys))
	s.mux.HandleFunc("POST /api/auth/keys", s.authRole(auth.RoleAdmin, gw.handleCreateAPIKey))
	s.mux.HandleFunc("DELETE /api/auth/keys/{id}", s.authRole(auth.RoleAdmin, gw.handleDeleteAPIKey))

	// ── V3: Tenant branding ───────────────────────────────────────────────
	s.mux.HandleFunc("GET /api/tenant/branding", s.auth(gw.handleGetBranding))
	s.mux.HandleFunc("PUT /api/tenant/branding", s.authRole(auth.RoleAdmin, gw.handleSetBranding))

	// ── V3: Reporting — CSV exports ───────────────────────────────────────
	s.mux.HandleFunc("GET /api/reports/fills", s.auth(gw.handleReportFills))
	s.mux.HandleFunc("GET /api/reports/pnl", s.auth(gw.handleReportPnL))

	// ── V3: SOC2 audit trail export ───────────────────────────────────────
	s.mux.HandleFunc("GET /api/audit", s.authRole(auth.RoleAuditor, gw.handleGetAudit))
	s.mux.HandleFunc("GET /api/audit/export", s.authRole(auth.RoleAuditor, gw.handleExportAudit))

	// ── Activity timeline + paper confidence ──────────────────────────────
	s.mux.HandleFunc("GET /api/timeline", s.auth(gw.handleTimeline))
	s.mux.HandleFunc("GET /api/paper/confidence", s.auth(gw.handlePaperConfidence))

	// ── V3: Strategy state ───────────────────────────────────────────────
	s.mux.HandleFunc("GET /api/strategies", s.auth(gw.handleGetStrategies))

	// Health (public)
	s.mux.HandleFunc("GET /api/health", gw.handleHealth)
}

// ── Mode ──────────────────────────────────────────────────────────────────

func (gw *Gateway) handleGetMode(w http.ResponseWriter, r *http.Request) {
	mode := gw.rdb.Get(r.Context(), "risk:mode").Val()
	if mode == "" {
		mode = "RUNNING"
	}
	killActive := gw.rdb.Exists(r.Context(), "kill:switch").Val() > 0
	jsonOK(w, map[string]interface{}{
		"mode":        mode,
		"kill_switch": killActive,
		"ts_ms":       time.Now().UnixMilli(),
	})
}

func (gw *Gateway) handleSetMode(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := gw.rdb.Set(r.Context(), "risk:commanded_mode", target, 0).Err(); err != nil {
			jsonErr(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Audit log.
		if gw.db != nil {
			if key := auth.FromContext(r.Context()); key != nil {
				_ = gw.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
					auth.ClientIP(r), "set_mode:"+target, map[string]string{"mode": target})
			}
		}
		jsonOK(w, map[string]string{"status": "commanded", "mode": target})
	}
}

func (gw *Gateway) handleGetRiskState(w http.ResponseWriter, r *http.Request) {
	raw := gw.rdb.Get(r.Context(), "risk:state:json").Val()
	if raw == "" {
		jsonOK(w, map[string]interface{}{"mode": "RUNNING", "note": "risk daemon not yet connected"})
		return
	}
	var state map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		jsonErr(w, "malformed risk state", http.StatusInternalServerError)
		return
	}
	jsonOK(w, state)
}

func (gw *Gateway) handleGetRiskHistory(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, []interface{}{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 100
	}
	rows, err := gw.db.RecentAlerts(r.Context(), gw.tenantID, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, rows)
}

func (gw *Gateway) handleGetRiskAlerts(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, []interface{}{})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	rows, err := gw.db.RecentAlerts(r.Context(), gw.tenantID, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, rows)
}

// ── PnL ───────────────────────────────────────────────────────────────────

func (gw *Gateway) handleGetPnL(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, map[string]interface{}{"note": "postgres not connected", "total_pnl": 0})
		return
	}
	rows, err := gw.db.PnLSummary(r.Context(), gw.tenantID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"by_strategy": rows})
}

func (gw *Gateway) handleGetPnLAttribution(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, []interface{}{})
		return
	}
	rows, err := gw.db.PnLSummary(r.Context(), gw.tenantID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, rows)
}

// ── Positions ─────────────────────────────────────────────────────────────

func (gw *Gateway) handleGetPositions(w http.ResponseWriter, r *http.Request) {
	pattern := "paper:pos:" + gw.tenantID + ":*"
	keys, err := gw.rdb.Keys(r.Context(), pattern).Result()
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(keys) == 0 {
		jsonOK(w, []interface{}{})
		return
	}
	vals, err := gw.rdb.MGet(r.Context(), keys...).Result()
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var positions []interface{}
	for _, v := range vals {
		if v == nil {
			continue
		}
		var pos map[string]interface{}
		if err := json.Unmarshal([]byte(v.(string)), &pos); err == nil {
			positions = append(positions, pos)
		}
	}
	jsonOK(w, positions)
}

// ── Intents ───────────────────────────────────────────────────────────────

func (gw *Gateway) handleGetIntents(w http.ResponseWriter, r *http.Request) {
	count, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if count == 0 {
		count = 50
	}
	msgs, err := gw.rdb.XRevRangeN(r.Context(), "trade:intents:approved", "+", "-", count).Result()
	if err != nil {
		jsonOK(w, []interface{}{})
		return
	}
	var intents []interface{}
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var intent map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &intent); err == nil {
			intents = append(intents, intent)
		}
	}
	jsonOK(w, intents)
}

// ── Orders ────────────────────────────────────────────────────────────────

func (gw *Gateway) handleGetOrders(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		count, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
		if count == 0 {
			count = 50
		}
		msgs, err := gw.rdb.XRevRangeN(r.Context(), "execution:events", "+", "-", count).Result()
		if err != nil {
			jsonOK(w, []interface{}{})
			return
		}
		var events []interface{}
		for _, m := range msgs {
			raw, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			var ev map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &ev); err == nil {
				events = append(events, ev)
			}
		}
		jsonOK(w, events)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit == 0 {
		limit = 50
	}
	rows, err := gw.db.RecentFills(r.Context(), gw.tenantID, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, rows)
}

// ── Live prices ──────────────────────────────────────────────────────────

// handleGetPrices returns the latest price per venue+symbol from the
// market:quotes Redis stream. Serves as a REST fallback when the SSE
// connection has not delivered consensus events yet.
func (gw *Gateway) handleGetPrices(w http.ResponseWriter, r *http.Request) {
	msgs, err := gw.rdb.XRevRangeN(r.Context(), "market:quotes", "+", "-", 200).Result()
	if err != nil {
		jsonOK(w, map[string]interface{}{})
		return
	}
	latest := make(map[string]map[string]interface{})
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var q map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &q); err != nil {
			continue
		}
		venue, _ := q["venue"].(string)
		symbol, _ := q["symbol"].(string)
		key := venue + ":" + symbol
		if _, seen := latest[key]; !seen {
			latest[key] = map[string]interface{}{
				"venue":    venue,
				"symbol":   symbol,
				"best_bid": q["best_bid"],
				"best_ask": q["best_ask"],
				"mark":     q["mark"],
				"ts_ms":    q["ts_ms"],
			}
		}
	}
	jsonOK(w, latest)
}

// ── Funding rates ─────────────────────────────────────────────────────────

func (gw *Gateway) handleGetFundingRates(w http.ResponseWriter, r *http.Request) {
	msgs, err := gw.rdb.XRevRangeN(r.Context(), "market:quotes", "+", "-", 200).Result()
	if err != nil {
		jsonOK(w, map[string]interface{}{})
		return
	}
	latest := make(map[string]map[string]interface{})
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var q map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &q); err != nil {
			continue
		}
		venue, _ := q["venue"].(string)
		symbol, _ := q["symbol"].(string)
		key := venue + ":" + symbol
		if _, seen := latest[key]; !seen {
			latest[key] = q
		}
	}
	result := make(map[string]interface{})
	for k, q := range latest {
		result[k] = map[string]interface{}{
			"venue":        q["venue"],
			"symbol":       q["symbol"],
			"funding_rate": q["funding_rate"],
			"mark":         q["mark"],
			"ts_ms":        q["ts_ms"],
		}
	}
	jsonOK(w, result)
}

// ── Equity curve ──────────────────────────────────────────────────────────

// handleGetEquityCurve returns a time-series of cumulative PnL snapshots suitable
// for charting. It builds the curve from the demo:fills Redis stream, so it works
// in paper mode without Postgres. When Postgres is connected the fills table is
// used instead for a more complete and persistent history.
func (gw *Gateway) handleGetEquityCurve(w http.ResponseWriter, r *http.Request) {
	// How many recent fills to include in the curve (configurable via ?limit=).
	count, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if count <= 0 || count > 10000 {
		count = 500
	}

	type point struct {
		TsMs      int64   `json:"ts_ms"`
		CumPnL    float64 `json:"cum_pnl_usd"`
		FillPnL   float64 `json:"fill_pnl_usd"`
		Strategy  string  `json:"strategy"`
		FillCount int     `json:"fill_count"`
	}

	var points []point
	var cumPnL float64
	fillCount := 0

	// Prefer Postgres if available.
	if gw.db != nil {
		rows, err := gw.db.RecentFills(r.Context(), gw.tenantID, int(count))
		if err == nil {
			// rows are newest-first; reverse to get oldest-first for the curve.
			for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
				rows[i], rows[j] = rows[j], rows[i]
			}
			for _, row := range rows {
				fillCount++
				pnl, _ := row["net_pnl_usd"].(float64)
				cumPnL += pnl
				ts, _ := row["ts"].(int64)
				strategy, _ := row["strategy"].(string)
				points = append(points, point{
					TsMs:      ts,
					CumPnL:    cumPnL,
					FillPnL:   pnl,
					Strategy:  strategy,
					FillCount: fillCount,
				})
			}
			jsonOK(w, map[string]interface{}{
				"points":    points,
				"total_pnl": cumPnL,
				"source":    "postgres",
			})
			return
		}
	}

	// Fall back to Redis demo fills stream.
	stream := "demo:fills:" + gw.tenantID
	msgs, err := gw.rdb.XRangeN(r.Context(), stream, "-", "+", count).Result()
	if err != nil {
		jsonOK(w, map[string]interface{}{
			"points":    []interface{}{},
			"total_pnl": 0,
			"source":    "redis",
			"note":      "no fills data yet",
		})
		return
	}

	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var fill map[string]interface{}
		if json.Unmarshal([]byte(raw), &fill) != nil {
			continue
		}
		fillCount++
		pnl, _ := fill["net_pnl_usd"].(float64)
		cumPnL += pnl
		tsMs, _ := fill["ts_fill_simulated_ms"].(float64)
		if tsMs == 0 {
			tsMs, _ = fill["ts_ms"].(float64)
		}
		strategy, _ := fill["strategy"].(string)
		points = append(points, point{
			TsMs:      int64(tsMs),
			CumPnL:    cumPnL,
			FillPnL:   pnl,
			Strategy:  strategy,
			FillCount: fillCount,
		})
	}

	jsonOK(w, map[string]interface{}{
		"points":    points,
		"total_pnl": cumPnL,
		"source":    "redis",
	})
}

// ── Paper trading mode ────────────────────────────────────────────────────

func (gw *Gateway) handleGetPaperMode(w http.ResponseWriter, r *http.Request) {
	mode := gw.rdb.Get(r.Context(), "trading:mode").Val()
	if mode == "" {
		mode = "PAPER"
	}
	jsonOK(w, map[string]string{"mode": mode})
}

func (gw *Gateway) handleSetPaperMode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode  string `json:"mode"`
		Force bool   `json:"force"` // admin override — skip graduation checks
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Mode != "PAPER" && req.Mode != "SHADOW" && req.Mode != "LIVE" {
		jsonErr(w, "mode must be PAPER, SHADOW, or LIVE", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	currentMode := gw.rdb.Get(ctx, "trading:mode").Val()
	if currentMode == "" {
		currentMode = "PAPER"
	}

	// Graduation guardrails: enforce confidence thresholds before mode upgrade.
	// Downgrades (e.g. LIVE→PAPER) are always allowed.
	// Admin override via force=true bypasses checks.
	isUpgrade := modeRank(req.Mode) > modeRank(currentMode)
	if isUpgrade && !req.Force {
		score, fillCount, err := gw.computeConfidence(ctx)
		if err != nil {
			jsonErr(w, fmt.Sprintf("cannot evaluate graduation: %v", err), http.StatusInternalServerError)
			return
		}

		minFills := int64(50)
		switch req.Mode {
		case "SHADOW":
			if score < 50 {
				jsonErr(w, fmt.Sprintf(
					"confidence score %.0f < 50 required for SHADOW (use force=true to override)", score),
					http.StatusPreconditionFailed)
				return
			}
			if fillCount < minFills {
				jsonErr(w, fmt.Sprintf(
					"fill count %d < %d minimum for SHADOW graduation", fillCount, minFills),
					http.StatusPreconditionFailed)
				return
			}
		case "LIVE":
			if score < 80 {
				jsonErr(w, fmt.Sprintf(
					"confidence score %.0f < 80 required for LIVE (use force=true to override)", score),
					http.StatusPreconditionFailed)
				return
			}
			if fillCount < 200 {
				jsonErr(w, fmt.Sprintf(
					"fill count %d < 200 minimum for LIVE graduation", fillCount),
					http.StatusPreconditionFailed)
				return
			}
		}
	}

	if err := gw.rdb.Set(ctx, "trading:mode", req.Mode, 0).Err(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Audit log the mode transition.
	gw.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: "audit:mode_transitions",
		Values: map[string]interface{}{
			"from":      currentMode,
			"to":        req.Mode,
			"forced":    req.Force,
			"tenant_id": gw.tenantID,
			"ts_ms":     time.Now().UnixMilli(),
		},
	})

	jsonOK(w, map[string]interface{}{
		"status":    "updated",
		"mode":      req.Mode,
		"from":      currentMode,
		"forced":    req.Force,
		"tenant_id": gw.tenantID,
	})
}

// computeConfidence returns the composite confidence score and fill count.
// Extracted from handlePaperConfidence for reuse in graduation checks.
func (gw *Gateway) computeConfidence(ctx context.Context) (score float64, fillCount int64, err error) {
	if gw.db == nil {
		return 0, 0, fmt.Errorf("postgres not connected — confidence requires KPI data")
	}
	kpi, err := gw.db.KPISummary(ctx, gw.tenantID)
	if err != nil {
		return 0, 0, err
	}

	fillCount, _ = kpi["fill_count"].(int64)
	winRate, _ := kpi["win_rate_pct"].(float64)
	sharpe, _ := kpi["sharpe_proxy"].(float64)
	avgSlippage, _ := kpi["avg_slippage_bps"].(float64)

	fillScore := clamp(float64(fillCount)/2.0, 0, 100)
	winScore := clamp((winRate-50)/20*100, 0, 100)
	sharpeScore := clamp(sharpe/2*100, 0, 100)
	slippageScore := clamp(100-avgSlippage*10, 0, 100)

	score = (fillScore + winScore + sharpeScore + slippageScore) / 4
	return score, fillCount, nil
}

// modeRank returns the ordinal rank of a trading mode for upgrade detection.
func modeRank(mode string) int {
	switch mode {
	case "PAPER":
		return 0
	case "SHADOW":
		return 1
	case "LIVE":
		return 2
	default:
		return -1
	}
}

// ── KPI ───────────────────────────────────────────────────────────────────

func (gw *Gateway) handleGetKPI(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, map[string]interface{}{"note": "postgres not connected"})
		return
	}
	kpis, err := gw.db.KPISummary(r.Context(), gw.tenantID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, kpis)
}

// ── Health ────────────────────────────────────────────────────────────────

func (gw *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	redisOK := gw.rdb.Ping(r.Context()).Err() == nil
	pgOK := gw.db != nil
	status := "ok"
	if !redisOK {
		status = "degraded"
	}
	jsonOK(w, map[string]interface{}{
		"status":   status,
		"redis":    redisOK,
		"postgres": pgOK,
		"ts_ms":    time.Now().UnixMilli(),
	})
}

// ── V3: Identity ──────────────────────────────────────────────────────────

// handleAuthMe returns the current key's role, tenant, and display name.
func (gw *Gateway) handleAuthMe(w http.ResponseWriter, r *http.Request) {
	key := auth.FromContext(r.Context())
	if key == nil {
		jsonOK(w, map[string]interface{}{"role": "admin", "tenant_id": gw.tenantID, "name": "dev"})
		return
	}
	jsonOK(w, map[string]interface{}{
		"id":        key.ID,
		"tenant_id": key.TenantID,
		"name":      key.Name,
		"role":      string(key.Role),
	})
}

// ── V3: API key management ────────────────────────────────────────────────

func (gw *Gateway) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, []interface{}{})
		return
	}
	keys, err := gw.db.ListAPIKeys(r.Context(), gw.tenantID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, keys)
}

func (gw *Gateway) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonErr(w, "postgres required for API key management", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Name string    `json:"name"`
		Role auth.Role `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonErr(w, "name is required", http.StatusBadRequest)
		return
	}
	validRoles := map[auth.Role]bool{
		auth.RoleAdmin: true, auth.RoleTrader: true,
		auth.RoleViewer: true, auth.RoleAuditor: true,
	}
	if !validRoles[req.Role] {
		jsonErr(w, "role must be admin, trader, viewer, or auditor", http.StatusBadRequest)
		return
	}

	fullKey, prefix, keyHash, err := auth.GenerateKey()
	if err != nil {
		jsonErr(w, "key generation failed", http.StatusInternalServerError)
		return
	}
	id, err := gw.db.CreateAPIKey(r.Context(), gw.tenantID, req.Name, req.Role, prefix, keyHash)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Audit log.
	if actor := auth.FromContext(r.Context()); actor != nil {
		_ = gw.db.AuditLogRich(r.Context(), gw.tenantID, actor.Name, string(actor.Role),
			auth.ClientIP(r), "create_api_key", map[string]interface{}{"id": id, "name": req.Name, "role": req.Role})
	}

	// Return the full key only once — it cannot be retrieved again.
	jsonOK(w, map[string]interface{}{
		"id":         id,
		"name":       req.Name,
		"role":       string(req.Role),
		"key":        fullKey,
		"key_prefix": prefix,
		"warning":    "Store this key securely — it will not be shown again.",
	})
}

func (gw *Gateway) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonErr(w, "postgres required for API key management", http.StatusServiceUnavailable)
		return
	}
	keyID := r.PathValue("id")
	if keyID == "" {
		jsonErr(w, "missing key id", http.StatusBadRequest)
		return
	}
	if err := gw.db.DeleteAPIKey(r.Context(), gw.tenantID, keyID); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if actor := auth.FromContext(r.Context()); actor != nil {
		_ = gw.db.AuditLogRich(r.Context(), gw.tenantID, actor.Name, string(actor.Role),
			auth.ClientIP(r), "delete_api_key", map[string]string{"id": keyID})
	}
	jsonOK(w, map[string]string{"status": "deleted"})
}

// ── V3: Tenant branding ───────────────────────────────────────────────────

func (gw *Gateway) handleGetBranding(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, map[string]interface{}{
			"id": gw.tenantID, "name": "ArbSuite",
			"logo_url": "", "primary_color": "#3b82f6", "accent_color": "#f97316",
		})
		return
	}
	branding, err := gw.db.GetTenantBranding(r.Context(), gw.tenantID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, branding)
}

func (gw *Gateway) handleSetBranding(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonErr(w, "postgres required for branding management", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Name         string `json:"name"`
		LogoURL      string `json:"logo_url"`
		PrimaryColor string `json:"primary_color"`
		AccentColor  string `json:"accent_color"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = "ArbSuite"
	}
	if req.PrimaryColor == "" {
		req.PrimaryColor = "#3b82f6"
	}
	if req.AccentColor == "" {
		req.AccentColor = "#f97316"
	}
	if err := gw.db.UpsertTenantBranding(r.Context(), gw.tenantID, req.Name, req.LogoURL, req.PrimaryColor, req.AccentColor); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if actor := auth.FromContext(r.Context()); actor != nil {
		_ = gw.db.AuditLogRich(r.Context(), gw.tenantID, actor.Name, string(actor.Role),
			auth.ClientIP(r), "update_branding", req)
	}
	jsonOK(w, map[string]string{"status": "updated"})
}

// ── V3: CSV reporting ─────────────────────────────────────────────────────

// handleReportFills streams a CSV of fills optionally filtered by date range.
func (gw *Gateway) handleReportFills(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonErr(w, "postgres required for reports", http.StatusServiceUnavailable)
		return
	}
	from, to, limit := parseDateRange(r)
	rows, err := gw.db.ExportFills(r.Context(), gw.tenantID, from, to, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="fills_%s.csv"`,
		time.Now().Format("20060102")))
	wr := csv.NewWriter(w)
	_ = wr.Write([]string{"id", "intent_id", "strategy", "symbol", "price", "notional",
		"fees", "slippage_bps", "net_pnl_usd", "mode", "ts"})
	for _, row := range rows {
		_ = wr.Write([]string{
			fmt.Sprint(row["id"]), fmt.Sprint(row["intent_id"]),
			fmt.Sprint(row["strategy"]), fmt.Sprint(row["symbol"]),
			fmt.Sprint(row["price"]), fmt.Sprint(row["notional"]),
			fmt.Sprint(row["fees"]), fmt.Sprint(row["slippage_bps"]),
			fmt.Sprint(row["net_pnl_usd"]), fmt.Sprint(row["mode"]),
			fmt.Sprint(row["ts"]),
		})
	}
	wr.Flush()
}

// handleReportPnL streams a CSV of PnL by strategy.
func (gw *Gateway) handleReportPnL(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonErr(w, "postgres required for reports", http.StatusServiceUnavailable)
		return
	}
	rows, err := gw.db.PnLSummary(r.Context(), gw.tenantID)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="pnl_%s.csv"`,
		time.Now().Format("20060102")))
	wr := csv.NewWriter(w)
	_ = wr.Write([]string{"strategy", "total_pnl", "fill_count", "total_fees"})
	for _, row := range rows {
		_ = wr.Write([]string{
			fmt.Sprint(row["strategy"]),
			fmt.Sprint(row["total_pnl"]),
			fmt.Sprint(row["fill_count"]),
			fmt.Sprint(row["total_fees"]),
		})
	}
	wr.Flush()
}

// ── V3: SOC2 audit trail ──────────────────────────────────────────────────

// handleGetAudit returns recent audit log entries as JSON.
func (gw *Gateway) handleGetAudit(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, []interface{}{})
		return
	}
	from, to, limit := parseDateRange(r)
	rows, err := gw.db.ExportAuditLog(r.Context(), gw.tenantID, from, to, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, rows)
}

// handleExportAudit streams a SOC2-ready CSV of the audit trail.
func (gw *Gateway) handleExportAudit(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonErr(w, "postgres required for audit export", http.StatusServiceUnavailable)
		return
	}
	from, to, limit := parseDateRange(r)
	rows, err := gw.db.ExportAuditLog(r.Context(), gw.tenantID, from, to, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="audit_%s_to_%s.csv"`,
		from.Format("20060102"), to.Format("20060102")))
	wr := csv.NewWriter(w)
	_ = wr.Write([]string{"id", "ts", "actor", "role", "ip_address", "action", "payload"})
	for _, row := range rows {
		_ = wr.Write([]string{
			fmt.Sprint(row["id"]),
			fmt.Sprint(row["ts"]),
			fmt.Sprint(row["actor"]),
			fmt.Sprint(row["role"]),
			fmt.Sprint(row["ip_address"]),
			fmt.Sprint(row["action"]),
			fmt.Sprint(row["payload"]),
		})
	}
	wr.Flush()
}

// ── Helpers ───────────────────────────────────────────────────────────────

// parseDateRange extracts ?from=, ?to=, and ?limit= query params with safe defaults.
func parseDateRange(r *http.Request) (from, to time.Time, limit int) {
	to = time.Now().UTC()
	from = to.AddDate(0, -1, 0) // default: last 30 days

	if s := r.URL.Query().Get("from"); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			from = t.UTC()
		}
	}
	if s := r.URL.Query().Get("to"); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			to = t.Add(24*time.Hour - time.Second).UTC()
		}
	}
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50000 {
		limit = 10000
	}
	return
}

// ── Activity timeline ─────────────────────────────────────────────────────

// handleTimeline returns a merged, reverse-chronological activity feed drawn
// from multiple Redis streams. It combines:
//   - Risk alerts        (risk:alerts stream)
//   - Execution events   (execution:events stream)
//   - Consensus anomalies (consensus:anomalies stream)
//   - Mode changes       (risk:mode_changes stream key in Redis)
//
// Each event is tagged with a "kind" field for dashboard colouring.
func (gw *Gateway) handleTimeline(w http.ResponseWriter, r *http.Request) {
	count, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if count == 0 || count > 200 {
		count = 100
	}

	type event struct {
		Kind    string                 `json:"kind"`
		TsMs    int64                  `json:"ts_ms"`
		Payload map[string]interface{} `json:"payload"`
	}

	var events []event

	streams := []struct {
		key  string
		kind string
	}{
		{"risk:alerts", "risk_alert"},
		{"execution:events", "execution"},
		{"consensus:anomalies", "anomaly"},
		{"risk:mode_changes", "mode_change"},
	}

	for _, s := range streams {
		msgs, err := gw.rdb.XRevRangeN(r.Context(), s.key, "+", "-", count).Result()
		if err != nil {
			continue
		}
		for _, m := range msgs {
			raw, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			var payload map[string]interface{}
			if err := json.Unmarshal([]byte(raw), &payload); err != nil {
				continue
			}
			// Extract ts_ms from payload or fall back to stream ID.
			tsMs := int64(0)
			if v, ok := payload["ts_ms"].(float64); ok {
				tsMs = int64(v)
			}
			events = append(events, event{Kind: s.kind, TsMs: tsMs, Payload: payload})
		}
	}

	// Sort descending by ts_ms (simple insertion sort — small N).
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j].TsMs > events[j-1].TsMs; j-- {
			events[j], events[j-1] = events[j-1], events[j]
		}
	}
	// Truncate to requested count.
	if int64(len(events)) > count {
		events = events[:count]
	}

	jsonOK(w, events)
}

// ── Paper trading confidence ──────────────────────────────────────────────

// handlePaperConfidence computes a 0–100 confidence score for paper trading
// readiness based on KPI metrics. Returns sub-scores and the composite score.
func (gw *Gateway) handlePaperConfidence(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		jsonOK(w, map[string]interface{}{
			"score":   0,
			"note":    "postgres not connected — confidence requires KPI data",
			"details": map[string]interface{}{},
		})
		return
	}

	score, fillCount, err := gw.computeConfidence(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Fetch sub-scores for the detailed response.
	kpi, _ := gw.db.KPISummary(r.Context(), gw.tenantID)
	winRate, _ := kpi["win_rate_pct"].(float64)
	sharpe, _ := kpi["sharpe_proxy"].(float64)
	avgSlippage, _ := kpi["avg_slippage_bps"].(float64)

	fillScore := clamp(float64(fillCount)/2.0, 0, 100)
	winScore := clamp((winRate-50)/20*100, 0, 100)
	sharpeScore := clamp(sharpe/2*100, 0, 100)
	slippageScore := clamp(100-avgSlippage*10, 0, 100)

	currentMode := gw.rdb.Get(r.Context(), "trading:mode").Val()
	if currentMode == "" {
		currentMode = "PAPER"
	}

	jsonOK(w, map[string]interface{}{
		"score":        fmt.Sprintf("%.0f", score),
		"current_mode": currentMode,
		"details": map[string]interface{}{
			"fill_volume_score":   fillScore,
			"win_rate_score":      winScore,
			"sharpe_score":        sharpeScore,
			"slippage_discipline": slippageScore,
			"fill_count":          fillCount,
			"win_rate_pct":        winRate,
			"sharpe_proxy":        sharpe,
			"avg_slippage_bps":    avgSlippage,
		},
		"thresholds": map[string]interface{}{
			"min_score_for_shadow": 50,
			"min_fills_for_shadow": 50,
			"min_score_for_live":   80,
			"min_fills_for_live":   200,
		},
		"eligible_for_shadow": score >= 50 && fillCount >= 50,
		"eligible_for_live":   score >= 80 && fillCount >= 200,
	})
}

// ── Strategy state ─────────────────────────────────────────────────────

// handleGetStrategies returns a summary of all active strategies, their recent
// intent counts, and latest activity from Redis streams.
func (gw *Gateway) handleGetStrategies(w http.ResponseWriter, r *http.Request) {
	strategies := []string{
		"CROSS_VENUE_ARB", "BASIS_CONVERGENCE",
		"FUNDING_CARRY", "FUNDING_DIFFERENTIAL", "FUNDING_CARRY_REVERSE",
		"CASCADE_CONTRA", "CORRELATION_BREAK",
		"DEX_CEX_ARB", "L2_BRIDGE_ARB",
		"SPREAD_CAPTURE", "LIQUIDATION_CONTRA",
	}

	// Count recent intents per strategy from trade:intents stream.
	intentCounts := make(map[string]int)
	msgs, err := gw.rdb.XRevRangeN(r.Context(), "trade:intents:approved", "+", "-", 200).Result()
	if err == nil {
		for _, m := range msgs {
			raw, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			var intent map[string]interface{}
			if json.Unmarshal([]byte(raw), &intent) == nil {
				if strat, ok := intent["strategy"].(string); ok {
					intentCounts[strat]++
				}
			}
		}
	}

	var result []map[string]interface{}
	for _, s := range strategies {
		result = append(result, map[string]interface{}{
			"strategy":     s,
			"recent_intents": intentCounts[s],
		})
	}
	jsonOK(w, result)
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
