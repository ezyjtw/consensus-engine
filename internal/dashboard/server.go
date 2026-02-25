package dashboard

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/ezyjtw/consensus-engine/internal/auth"
	"github.com/ezyjtw/consensus-engine/internal/ledger"
)

// knownExchanges is the authoritative list of supported venues.
var knownExchanges = []string{"binance", "okx", "bybit", "deribit", "htx", "gate"}

// Server wires up all HTTP routes for the dashboard.
type Server struct {
	gw        *Gateway
	store     *Store
	sse       *StreamHandler
	alerts    *AlertWorker
	authToken string // legacy single-token auth; role = admin
	db        *ledger.DB
	mux       *http.ServeMux
}

func NewServer(store *Store, sse *StreamHandler, alerts *AlertWorker, authToken string) *Server {
	s := &Server{
		store:     store,
		sse:       sse,
		alerts:    alerts,
		authToken: authToken,
		mux:       http.NewServeMux(),
	}
	s.routes()
	s.RegisterStrategyRoutes()
	return s
}

// SetDB wires the Postgres DB for RBAC API-key validation.
func (s *Server) SetDB(db *ledger.DB) { s.db = db }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// The index page is public so the login screen can be served.
	s.mux.HandleFunc("GET /", s.handleIndex)

	// Prometheus-compatible metrics scrape endpoint — public, no auth required.
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)

	// Everything else requires a valid auth token.
	s.mux.HandleFunc("GET /api/events", s.auth(s.sse.ServeHTTP))

	s.mux.HandleFunc("GET /api/connections", s.auth(s.handleGetConnections))
	s.mux.HandleFunc("PUT /api/connections/{exchange}", s.authRole(auth.RoleAdmin, s.handleSaveConnection))

	s.mux.HandleFunc("GET /api/alerts", s.auth(s.handleGetAlerts))
	s.mux.HandleFunc("PUT /api/alerts", s.authRole(auth.RoleTrader, s.handleSaveAlerts))
	s.mux.HandleFunc("POST /api/alerts/test", s.authRole(auth.RoleTrader, s.handleTestWebhook))

	s.mux.HandleFunc("GET /api/kill", s.auth(s.handleGetKill))
	s.mux.HandleFunc("POST /api/kill", s.authRole(auth.RoleTrader, s.handleActivateKill))
	s.mux.HandleFunc("DELETE /api/kill", s.authRole(auth.RoleTrader, s.handleDeactivateKill))

	// Funding stage management (admin-only writes, any auth reads).
	s.mux.HandleFunc("GET /api/config/stages", s.auth(s.handleGetStages))
	s.mux.HandleFunc("PUT /api/config/stages/{symbol}", s.authRole(auth.RoleAdmin, s.handleSetStage))
}

// ── Auth middleware ────────────────────────────────────────────────────────

// auth validates the request (legacy token or DB API key) and stores the
// resulting *auth.APIKey in the context. Returns 401 if auth fails.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.resolveKey(r)
		if key == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(auth.WithAPIKey(r.Context(), key)))
	}
}

// authRole wraps auth and additionally enforces a minimum role.
func (s *Server) authRole(minRole auth.Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.resolveKey(r)
		if key == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if err := auth.RequireRole(key, minRole); err != nil {
			jsonErr(w, err.Error(), http.StatusForbidden)
			return
		}
		next(w, r.WithContext(auth.WithAPIKey(r.Context(), key)))
	}
}

// resolveKey authenticates via legacy token (→ admin) or DB API key.
// Returns nil if authentication fails.
func (s *Server) resolveKey(r *http.Request) *auth.APIKey {
	token := auth.ExtractBearer(r)
	if token == "" {
		// No auth configured in dev mode. main.go already validates that
		// authToken=="" is only allowed when ENV is dev/empty, so we can
		// safely grant admin access here regardless of whether Postgres is
		// connected (Postgres may be used for historical data, not auth).
		if s.authToken == "" {
			return &auth.APIKey{Role: auth.RoleAdmin, TenantID: "default", Name: "dev"}
		}
		return nil
	}

	// Legacy single-token mode.
	if s.authToken != "" && token == s.authToken {
		return &auth.APIKey{Role: auth.RoleAdmin, TenantID: "default", Name: "legacy"}
	}

	// DB-backed API key lookup.
	if s.db != nil {
		keyHash := auth.HashKey(token)
		apiKey, err := s.db.ValidateAPIKey(r.Context(), keyHash)
		if err == nil && apiKey != nil {
			return apiKey
		}
	}

	return nil
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

// --- Connections ---

func (s *Server) handleGetConnections(w http.ResponseWriter, r *http.Request) {
	configs, err := s.store.GetConnections(r.Context(), knownExchanges)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, configs)
}

func (s *Server) handleSaveConnection(w http.ResponseWriter, r *http.Request) {
	exchange := r.PathValue("exchange")
	if !isKnown(exchange) {
		jsonErr(w, "unknown exchange: "+exchange, http.StatusBadRequest)
		return
	}
	var req struct {
		APIKey     string `json:"api_key"`
		APISecret  string `json:"api_secret"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := s.store.SaveConnection(r.Context(), exchange, req.APIKey, req.APISecret, req.Passphrase); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Audit log.
	if s.db != nil {
		if key := auth.FromContext(r.Context()); key != nil {
			_ = s.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
				auth.ClientIP(r), "save_connection:"+exchange, map[string]string{"exchange": exchange})
		}
	}
	log.Printf("connection saved for %s", exchange)
	jsonOK(w, map[string]string{"status": "saved"})
}

// --- Alerts ---

func (s *Server) handleGetAlerts(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetAlertConfig(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, cfg)
}

func (s *Server) handleSaveAlerts(w http.ResponseWriter, r *http.Request) {
	var cfg AlertConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if err := s.store.SaveAlertConfig(r.Context(), cfg); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "saved"})
}

func (s *Server) handleTestWebhook(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetAlertConfig(r.Context())
	if err != nil || cfg.WebhookURL == "" {
		jsonErr(w, "no webhook URL configured", http.StatusBadRequest)
		return
	}
	if err := s.alerts.TestWebhook(r.Context(), cfg.WebhookURL); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"status": "sent"})
}

// --- Kill switch ---

func (s *Server) handleGetKill(w http.ResponseWriter, r *http.Request) {
	state, err := s.store.GetKillSwitch(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, state)
}

func (s *Server) handleActivateKill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // reason is optional
	if err := s.store.SetKillSwitch(r.Context(), true, req.Reason); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Audit log.
	if s.db != nil {
		if key := auth.FromContext(r.Context()); key != nil {
			_ = s.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
				auth.ClientIP(r), "kill_switch_activated", map[string]string{"reason": req.Reason})
		}
	}
	log.Printf("KILL SWITCH ACTIVATED — reason: %q", req.Reason)
	jsonOK(w, map[string]string{"status": "activated"})
}

func (s *Server) handleDeactivateKill(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetKillSwitch(r.Context(), false, ""); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.db != nil {
		if key := auth.FromContext(r.Context()); key != nil {
			_ = s.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
				auth.ClientIP(r), "kill_switch_deactivated", nil)
		}
	}
	log.Println("KILL SWITCH DEACTIVATED")
	jsonOK(w, map[string]string{"status": "deactivated"})
}

// --- Funding stages ---

// validStages are the allowed funding stage values.
var validStages = map[string]bool{
	"OBSERVE": true, "PAPER": true, "CONSERVATIVE": true, "FULL": true,
}

func (s *Server) handleGetStages(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.GetFundingStages(r.Context())
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []FundingStageEntry{}
	}
	jsonOK(w, entries)
}

func (s *Server) handleSetStage(w http.ResponseWriter, r *http.Request) {
	symbol := r.PathValue("symbol")
	if symbol == "" {
		jsonErr(w, "symbol is required", http.StatusBadRequest)
		return
	}
	var req struct {
		Stage string `json:"stage"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if !validStages[req.Stage] {
		jsonErr(w, "invalid stage: must be OBSERVE, PAPER, CONSERVATIVE, or FULL", http.StatusBadRequest)
		return
	}
	if err := s.store.SetFundingStage(r.Context(), symbol, req.Stage); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if s.db != nil {
		if key := auth.FromContext(r.Context()); key != nil {
			_ = s.db.AuditLogRich(r.Context(), key.TenantID, key.Name, string(key.Role),
				auth.ClientIP(r), "set_funding_stage", map[string]string{
					"symbol": symbol, "stage": req.Stage,
				})
		}
	}
	log.Printf("funding stage updated: %s → %s", symbol, req.Stage)
	jsonOK(w, map[string]string{"status": "updated", "symbol": symbol, "stage": req.Stage})
}

// --- helpers ---

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func isKnown(exchange string) bool {
	for _, e := range knownExchanges {
		if e == exchange {
			return true
		}
	}
	return false
}

// ensure strings is used (auth.ExtractBearer uses it).
var _ = strings.HasPrefix
