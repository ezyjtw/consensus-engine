package dashboard

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/yourorg/arbsuite/internal/ledger"
)

// Gateway extends the dashboard with the full V1 REST API surface.
// It holds a Redis client (shared with the Store) and an optional ledger DB.
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

// RegisterRoutes attaches all gateway endpoints to the server mux.
func (s *Server) RegisterGateway(gw *Gateway) {
	s.gw = gw

	// Risk / mode
	s.mux.HandleFunc("GET /api/mode", s.auth(gw.handleGetMode))
	s.mux.HandleFunc("POST /api/mode/pause", s.auth(gw.handleSetMode("PAUSED")))
	s.mux.HandleFunc("POST /api/mode/safe", s.auth(gw.handleSetMode("SAFE")))
	s.mux.HandleFunc("POST /api/mode/flatten", s.auth(gw.handleSetMode("FLATTEN")))
	s.mux.HandleFunc("GET /api/risk/state", s.auth(gw.handleGetRiskState))
	s.mux.HandleFunc("GET /api/risk/history", s.auth(gw.handleGetRiskHistory))
	s.mux.HandleFunc("GET /api/risk/alerts", s.auth(gw.handleGetRiskAlerts))

	// PnL
	s.mux.HandleFunc("GET /api/pnl", s.auth(gw.handleGetPnL))
	s.mux.HandleFunc("GET /api/pnl/attribution", s.auth(gw.handleGetPnLAttribution))

	// Positions (Redis-based, from execution router paper positions)
	s.mux.HandleFunc("GET /api/positions", s.auth(gw.handleGetPositions))

	// Intents
	s.mux.HandleFunc("GET /api/intents", s.auth(gw.handleGetIntents))

	// Orders / fills
	s.mux.HandleFunc("GET /api/orders", s.auth(gw.handleGetOrders))

	// Funding rates (from market:quotes latest per venue)
	s.mux.HandleFunc("GET /api/funding/rates", s.auth(gw.handleGetFundingRates))

	// Paper trading mode
	s.mux.HandleFunc("GET /api/paper/mode", s.auth(gw.handleGetPaperMode))
	s.mux.HandleFunc("PUT /api/paper/mode", s.auth(gw.handleSetPaperMode))

	// Health
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

// ── Positions (from Redis paper position store) ───────────────────────────

func (gw *Gateway) handleGetPositions(w http.ResponseWriter, r *http.Request) {
	// Scan all position keys for this tenant.
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

// ── Intents (recent from Redis stream snapshot) ───────────────────────────

func (gw *Gateway) handleGetIntents(w http.ResponseWriter, r *http.Request) {
	count, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if count == 0 {
		count = 50
	}
	// Read recent messages from the intents stream (non-destructive XREVRANGE).
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

// ── Orders (recent fills from Redis) ─────────────────────────────────────

func (gw *Gateway) handleGetOrders(w http.ResponseWriter, r *http.Request) {
	if gw.db == nil {
		// Fall back to Redis stream.
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

// ── Funding rates (latest per venue from Redis) ───────────────────────────

func (gw *Gateway) handleGetFundingRates(w http.ResponseWriter, r *http.Request) {
	// Read the very latest quotes from market:quotes stream — one per venue/symbol.
	msgs, err := gw.rdb.XRevRangeN(r.Context(), "market:quotes", "+", "-", 200).Result()
	if err != nil {
		jsonOK(w, map[string]interface{}{})
		return
	}
	// Deduplicate to latest per venue+symbol.
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
	// Return just funding rate fields.
	result := make(map[string]interface{})
	for k, q := range latest {
		fr := q["funding_rate"]
		result[k] = map[string]interface{}{
			"venue":        q["venue"],
			"symbol":       q["symbol"],
			"funding_rate": fr,
			"mark":         q["mark"],
			"ts_ms":        q["ts_ms"],
		}
	}
	jsonOK(w, result)
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
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if req.Mode != "PAPER" && req.Mode != "SHADOW" && req.Mode != "LIVE" {
		jsonErr(w, "mode must be PAPER, SHADOW, or LIVE", http.StatusBadRequest)
		return
	}
	if err := gw.rdb.Set(r.Context(), "trading:mode", req.Mode, 0).Err(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "updated", "mode": req.Mode})
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

// Ensure Server has a gw field. This is added here to avoid touching server.go.
// We extend Server using a package-level init trick: add the field via embedding helper.
// Actually we just use a pointer stored on Server — see server_gw.go for the field.

func init() {
	// Placeholder to ensure file compiles.
	_ = (*Gateway)(nil)
}
