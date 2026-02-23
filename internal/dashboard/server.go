package dashboard

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
)

// knownExchanges is the authoritative list of supported venues.
var knownExchanges = []string{"binance", "okx", "bybit", "deribit", "htx", "gate"}

// Server wires up all HTTP routes for the dashboard.
type Server struct {
	gw          *Gateway
	store       *Store
	sse         *StreamHandler
	alerts      *AlertWorker
	authToken   string
	mux         *http.ServeMux
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
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// The index page is public so the login screen can be served.
	s.mux.HandleFunc("GET /", s.handleIndex)

	// Everything else requires a valid auth token.
	s.mux.HandleFunc("GET /api/events", s.auth(s.sse.ServeHTTP))

	s.mux.HandleFunc("GET /api/connections", s.auth(s.handleGetConnections))
	s.mux.HandleFunc("PUT /api/connections/{exchange}", s.auth(s.handleSaveConnection))

	s.mux.HandleFunc("GET /api/alerts", s.auth(s.handleGetAlerts))
	s.mux.HandleFunc("PUT /api/alerts", s.auth(s.handleSaveAlerts))
	s.mux.HandleFunc("POST /api/alerts/test", s.auth(s.handleTestWebhook))

	s.mux.HandleFunc("GET /api/kill", s.auth(s.handleGetKill))
	s.mux.HandleFunc("POST /api/kill", s.auth(s.handleActivateKill))
	s.mux.HandleFunc("DELETE /api/kill", s.auth(s.handleDeactivateKill))
}

// auth is a middleware that checks for a valid Bearer token.
// SSE connections may pass the token as ?token=<value> because the browser
// EventSource API does not support custom request headers.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// No auth configured → open access (useful during local dev).
		if s.authToken == "" {
			next(w, r)
			return
		}
		token := ""
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			token = strings.TrimPrefix(h, "Bearer ")
		}
		if token == "" {
			token = r.URL.Query().Get("token")
		}
		if token != s.authToken {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
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
	json.NewDecoder(r.Body).Decode(&req) // reason is optional
	if err := s.store.SetKillSwitch(r.Context(), true, req.Reason); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("KILL SWITCH ACTIVATED — reason: %q", req.Reason)
	jsonOK(w, map[string]string{"status": "activated"})
}

func (s *Server) handleDeactivateKill(w http.ResponseWriter, r *http.Request) {
	if err := s.store.SetKillSwitch(r.Context(), false, ""); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Println("KILL SWITCH DEACTIVATED")
	jsonOK(w, map[string]string{"status": "deactivated"})
}

// --- helpers ---

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func isKnown(exchange string) bool {
	for _, e := range knownExchanges {
		if e == exchange {
			return true
		}
	}
	return false
}
