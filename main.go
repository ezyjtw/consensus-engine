package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/yourorg/consensus-engine/internal/consensus"
)

var policy *Policy

type Policy = consensus.Policy

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}

	cfgPath := os.Getenv("CONSENSUS_CONFIG")
	if cfgPath == "" {
		cfgPath = "configs/policies/consensus_policy.yaml"
	}

	var loadErr error
	policy, loadErr = consensus.LoadPolicy(cfgPath)

	http.HandleFunc("/", handleRoot)
	http.HandleFunc("/health", handleHealth)
	http.HandleFunc("/config", handleConfig)

	addr := fmt.Sprintf("0.0.0.0:%s", port)
	log.Printf("consensus-engine HTTP server starting on %s", addr)
	if loadErr != nil {
		log.Printf("warning: policy not loaded: %v", loadErr)
	} else {
		log.Printf("policy loaded from %s", cfgPath)
	}
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	status := "running"
	policyLoaded := policy != nil
	resp := map[string]interface{}{
		"service":       "consensus-engine",
		"status":        status,
		"description":   "Multi-venue crypto price consensus service",
		"policy_loaded": policyLoaded,
	}
	if policyLoaded {
		resp["symbols"] = policy.Symbols
		resp["core_venues"] = policy.CoreVenues
	}
	json.NewEncoder(w).Encode(resp)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	if policy == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]string{"error": "policy not loaded"})
		return
	}
	resp := map[string]interface{}{
		"size_notional_usd":   policy.SizeNotionalUSD,
		"stale_ms":            policy.StaleMs,
		"outlier_bps_warn":    policy.OutlierBpsWarn,
		"outlier_bps_blacklist": policy.OutlierBpsBlacklist,
		"min_core_quorum":     policy.MinCoreQuorum,
		"core_venues":         policy.CoreVenues,
		"optional_venues":     policy.OptionalVenues,
		"symbols":             policy.Symbols,
		"slippage_buffer_bps": policy.SlippageBufferBps,
		"depth_penalty_bps":   policy.DepthPenaltyBps,
	}
	json.NewEncoder(w).Encode(resp)
}
