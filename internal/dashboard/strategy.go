package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/auth"
	"github.com/redis/go-redis/v9"
)

const keyStrategyPrefix = "strategy:config:"

// StrategyConfig represents a user-configured strategy instance.
// Each strategy gets its own capital allocation, exchange set, and parameters.
type StrategyConfig struct {
	ID          string            `json:"id"`          // e.g. "funding_carry"
	Name        string            `json:"name"`        // display name e.g. "Funding Rate Carry"
	Description string            `json:"description"` // short description
	Enabled     bool              `json:"enabled"`
	Venues      []string          `json:"venues"`      // exchanges this strategy may use
	CapitalUSD  float64           `json:"capital_usd"` // locked capital for this strategy
	Params      map[string]string `json:"params"`      // strategy-specific parameters
	Stage       string            `json:"stage"`       // OBSERVE/PAPER/CONSERVATIVE/FULL
	UpdatedMs   int64             `json:"updated_ms"`
}

// StrategyLogEntry represents a single strategy log line.
type StrategyLogEntry struct {
	TsMs     int64  `json:"ts_ms"`
	Level    string `json:"level"`    // INFO, WARN, ERROR, TRADE
	Strategy string `json:"strategy"` // matches StrategyConfig.ID
	Message  string `json:"message"`
}

// defaultStrategies returns the built-in strategy definitions.
func defaultStrategies() []StrategyConfig {
	return []StrategyConfig{
		{
			ID:          "funding_carry",
			Name:        "Funding Rate Carry",
			Description: "Long spot + short perp on same venue to collect positive funding payments every 8h",
			Enabled:     false,
			Venues:      []string{"binance"},
			CapitalUSD:  0,
			Stage:       "OBSERVE",
			Params: map[string]string{
				"min_annual_yield_pct": "8.0",
				"max_notional_usd":    "50000",
				"max_slippage_bps":    "8",
				"cooldown_s":          "300",
				"eval_interval_s":     "30",
			},
		},
		{
			ID:          "funding_reverse",
			Name:        "Reverse Carry",
			Description: "Long perp + short spot when funding rate is negative — collect payments from the other side",
			Enabled:     false,
			Venues:      []string{"binance"},
			CapitalUSD:  0,
			Stage:       "OBSERVE",
			Params: map[string]string{
				"min_annual_yield_pct": "10.0",
				"max_notional_usd":    "25000",
			},
		},
		{
			ID:          "funding_differential",
			Name:        "Funding Differential",
			Description: "Cross-venue funding rate arbitrage — long low-rate venue, short high-rate venue",
			Enabled:     false,
			Venues:      []string{"binance", "okx"},
			CapitalUSD:  0,
			Stage:       "OBSERVE",
			Params: map[string]string{
				"min_differential_bps": "5.0",
				"max_notional_usd":     "30000",
			},
		},
		{
			ID:          "cross_venue_arb",
			Name:        "Cross-Venue Arbitrage",
			Description: "Buy on cheap venue, sell on expensive venue when price discrepancy exceeds threshold",
			Enabled:     false,
			Venues:      []string{"binance", "okx", "bybit"},
			CapitalUSD:  0,
			Stage:       "OBSERVE",
			Params: map[string]string{
				"min_edge_bps":     "3.0",
				"max_notional_usd": "25000",
			},
		},
		{
			ID:          "basis_convergence",
			Name:        "Basis Convergence",
			Description: "Trade the spot-futures basis when it deviates from fair value",
			Enabled:     false,
			Venues:      []string{"binance", "deribit"},
			CapitalUSD:  0,
			Stage:       "OBSERVE",
			Params: map[string]string{
				"min_basis_bps":    "10.0",
				"max_notional_usd": "30000",
			},
		},
	}
}

// ── Store methods ─────────────────────────────────────────────────────────

// GetStrategies returns all configured strategies. If none exist in Redis,
// returns default strategies.
func (s *Store) GetStrategies(ctx context.Context) ([]StrategyConfig, error) {
	keys, err := s.rdb.Keys(ctx, keyStrategyPrefix+"*").Result()
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return defaultStrategies(), nil
	}
	var strategies []StrategyConfig
	for _, key := range keys {
		data, err := s.rdb.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}
		var cfg StrategyConfig
		if json.Unmarshal(data, &cfg) == nil {
			strategies = append(strategies, cfg)
		}
	}
	if len(strategies) == 0 {
		return defaultStrategies(), nil
	}
	return strategies, nil
}

// GetStrategy returns a single strategy config by ID.
func (s *Store) GetStrategy(ctx context.Context, id string) (*StrategyConfig, error) {
	data, err := s.rdb.Get(ctx, keyStrategyPrefix+id).Bytes()
	if err == redis.Nil {
		// Check if it's a known default.
		for _, d := range defaultStrategies() {
			if d.ID == id {
				return &d, nil
			}
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg StrategyConfig
	return &cfg, json.Unmarshal(data, &cfg)
}

// SaveStrategy persists a strategy config to Redis.
func (s *Store) SaveStrategy(ctx context.Context, cfg StrategyConfig) error {
	cfg.UpdatedMs = time.Now().UnixMilli()
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, keyStrategyPrefix+cfg.ID, data, 0).Err()
}

// GetTotalLockedCapital returns the sum of capital_usd across all enabled strategies.
func (s *Store) GetTotalLockedCapital(ctx context.Context) (float64, error) {
	strategies, err := s.GetStrategies(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, s := range strategies {
		if s.Enabled {
			total += s.CapitalUSD
		}
	}
	return total, nil
}

// AppendStrategyLog writes a log entry to the strategy-specific Redis stream.
func (s *Store) AppendStrategyLog(ctx context.Context, entry StrategyLogEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	stream := "strategy:logs:" + entry.Strategy
	return s.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		MaxLen: 1000, // keep last 1000 entries per strategy
		Approx: true,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// GetStrategyLogs reads recent log entries for a strategy.
func (s *Store) GetStrategyLogs(ctx context.Context, strategyID string, limit int64) ([]StrategyLogEntry, error) {
	stream := "strategy:logs:" + strategyID
	msgs, err := s.rdb.XRevRangeN(ctx, stream, "+", "-", limit).Result()
	if err != nil {
		return nil, err
	}
	var entries []StrategyLogEntry
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var entry StrategyLogEntry
		if json.Unmarshal([]byte(raw), &entry) == nil {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

// ── Gateway handlers ──────────────────────────────────────────────────────

// RegisterStrategyRoutes attaches strategy management endpoints to the server.
func (s *Server) RegisterStrategyRoutes() {
	s.mux.HandleFunc("GET /api/strategies/config", s.auth(s.handleGetStrategyConfigs))
	s.mux.HandleFunc("GET /api/strategies/config/{id}", s.auth(s.handleGetStrategyConfig))
	s.mux.HandleFunc("PUT /api/strategies/config/{id}", s.authRole(auth.RoleTrader, s.handleSaveStrategyConfig))
	s.mux.HandleFunc("POST /api/strategies/{id}/toggle", s.authRole(auth.RoleTrader, s.handleToggleStrategy))
	s.mux.HandleFunc("GET /api/strategies/{id}/logs", s.auth(s.handleGetStrategyLogs))
	s.mux.HandleFunc("GET /api/strategies/capital", s.auth(s.handleGetCapitalSummary))
}

func (s *Server) handleGetStrategyConfigs(w http.ResponseWriter, r *http.Request) {
	strategies, err := s.store.GetStrategies(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, strategies)
}

func (s *Server) handleGetStrategyConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cfg, err := s.store.GetStrategy(r.Context(), id)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		jsonErr(w, "strategy not found: "+id, http.StatusNotFound)
		return
	}
	jsonOK(w, cfg)
}

func (s *Server) handleSaveStrategyConfig(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	var req StrategyConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.ID = id

	// Validate venues.
	for _, v := range req.Venues {
		if !isKnown(v) {
			jsonErr(w, fmt.Sprintf("unknown venue: %s", v), http.StatusBadRequest)
			return
		}
	}

	// Validate stage.
	if req.Stage != "" && !validStages[req.Stage] {
		jsonErr(w, "invalid stage: must be OBSERVE, PAPER, CONSERVATIVE, or FULL", http.StatusBadRequest)
		return
	}

	// Validate capital doesn't exceed available.
	if req.CapitalUSD < 0 {
		jsonErr(w, "capital_usd must be non-negative", http.StatusBadRequest)
		return
	}

	if err := s.store.SaveStrategy(r.Context(), req); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Audit log.
	if s.db != nil {
		if key := auth.FromContext(r.Context()); key != nil {
			_ = s.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
				auth.ClientIP(r), "update_strategy", map[string]interface{}{
					"strategy_id": id, "enabled": req.Enabled, "capital_usd": req.CapitalUSD,
					"venues": req.Venues, "stage": req.Stage,
				})
		}
	}

	jsonOK(w, map[string]interface{}{"status": "updated", "strategy": req})
}

func (s *Server) handleToggleStrategy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()

	cfg, err := s.store.GetStrategy(ctx, id)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg == nil {
		jsonErr(w, "strategy not found: "+id, http.StatusNotFound)
		return
	}

	cfg.Enabled = !cfg.Enabled
	if err := s.store.SaveStrategy(ctx, *cfg); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Log the toggle.
	_ = s.store.AppendStrategyLog(ctx, StrategyLogEntry{
		TsMs:     time.Now().UnixMilli(),
		Level:    "INFO",
		Strategy: id,
		Message:  fmt.Sprintf("Strategy %s by user", map[bool]string{true: "ENABLED", false: "DISABLED"}[cfg.Enabled]),
	})

	if s.db != nil {
		if key := auth.FromContext(r.Context()); key != nil {
			_ = s.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
				auth.ClientIP(r), "toggle_strategy", map[string]interface{}{
					"strategy_id": id, "enabled": cfg.Enabled,
				})
		}
	}

	jsonOK(w, map[string]interface{}{"status": "toggled", "id": id, "enabled": cfg.Enabled})
}

func (s *Server) handleGetStrategyLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	limit, _ := strconv.ParseInt(r.URL.Query().Get("limit"), 10, 64)
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	entries, err := s.store.GetStrategyLogs(r.Context(), id, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []StrategyLogEntry{}
	}
	jsonOK(w, entries)
}

func (s *Server) handleGetCapitalSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	strategies, err := s.store.GetStrategies(ctx)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Read total available equity.
	totalEquity := 100000.0 // default paper balance
	raw := s.store.rdb.Get(ctx, "paper:equity").Val()
	if raw != "" {
		var eq map[string]interface{}
		if json.Unmarshal([]byte(raw), &eq) == nil {
			if v, ok := eq["current_equity_usd"].(float64); ok && v > 0 {
				totalEquity = v
			}
		}
	}

	var allocated float64
	breakdown := make([]map[string]interface{}, 0)
	for _, s := range strategies {
		if s.Enabled {
			allocated += s.CapitalUSD
		}
		breakdown = append(breakdown, map[string]interface{}{
			"id":          s.ID,
			"name":        s.Name,
			"enabled":     s.Enabled,
			"capital_usd": s.CapitalUSD,
		})
	}

	jsonOK(w, map[string]interface{}{
		"total_equity_usd":      totalEquity,
		"allocated_usd":         allocated,
		"unallocated_usd":       totalEquity - allocated,
		"allocation_pct":        allocated / totalEquity * 100,
		"strategy_breakdown":    breakdown,
	})
}
