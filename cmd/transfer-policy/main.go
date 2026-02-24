// transfer-policy is a lightweight HTTP service that validates transfer requests
// against the configured policy (allowlist, velocity limits, tamper detection).
// It exposes a single POST /check endpoint consumed by the execution router
// before any real withdrawal is submitted to an exchange API.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/transfer"
)

func main() {
	cfgPath := flag.String("config", "configs/policies/transfer_policy.yaml",
		"Path to transfer policy YAML")
	port := flag.String("port", "8085", "HTTP listen port")
	flag.Parse()

	cfg, err := transfer.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("transfer-policy: load config: %v", err)
	}
	log.Printf("transfer-policy: loaded policy (hash=%s manual_approval=%v allowlist=%d entries)",
		cfg.Hash(), cfg.ManualApprovalRequired, len(cfg.Allowlist))

	engine := transfer.New(cfg, *cfgPath)

	mux := http.NewServeMux()

	// POST /check — validate a transfer request.
	mux.HandleFunc("POST /check", func(w http.ResponseWriter, r *http.Request) {
		var req transfer.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
			return
		}
		if req.RequestedAt.IsZero() {
			req.RequestedAt = time.Now().UTC()
		}
		decision := engine.Check(req)
		w.Header().Set("Content-Type", "application/json")
		if decision.Status == transfer.StatusDenied {
			w.WriteHeader(http.StatusForbidden)
		}
		_ = json.NewEncoder(w).Encode(decision)
	})

	// GET /pending — list pending transfers awaiting approval.
	mux.HandleFunc("GET /pending", func(w http.ResponseWriter, r *http.Request) {
		pending := engine.ListPending()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pending)
	})

	// POST /approve — approve a pending transfer.
	mux.HandleFunc("POST /approve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RequestID  string `json:"request_id"`
			ApprovedBy string `json:"approved_by"`
			Comment    string `json:"comment"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
			return
		}
		if req.RequestID == "" || req.ApprovedBy == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "request_id and approved_by required"})
			return
		}
		decision, fullyApproved := engine.Approve(req.RequestID, req.ApprovedBy, req.Comment)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"decision":       decision,
			"fully_approved": fullyApproved,
		})
	})

	// POST /deny — deny a pending transfer.
	mux.HandleFunc("POST /deny", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			RequestID string `json:"request_id"`
			DeniedBy  string `json:"denied_by"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid JSON"})
			return
		}
		decision := engine.DenyPending(req.RequestID, req.DeniedBy, req.Reason)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(decision)
	})

	// GET /health — liveness probe.
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"status":             "ok",
			"config_hash":        cfg.Hash(),
			"manual_approval":    cfg.ManualApprovalRequired,
			"allowlist_entries":  len(cfg.Allowlist),
			"ts_ms":              time.Now().UnixMilli(),
		})
	})

	srv := &http.Server{
		Addr:         ":" + *port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		log.Printf("transfer-policy: listening on :%s", *port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("transfer-policy: http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("transfer-policy: shutting down…")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		fmt.Printf("transfer-policy: shutdown error: %v\n", err)
	}
}
