package dashboard

// metrics.go — Prometheus-compatible text-format metrics endpoint.
// Uses only the standard library and the existing Redis client; no new deps.
//
// Scrape with: curl http://localhost:8080/metrics
// Configure Prometheus: scrape_configs: - job_name: arbsuite, static_configs: - targets: [...]

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleMetrics serves a Prometheus text exposition of live operational metrics.
// The endpoint is public (no auth) so Prometheus can scrape without a token.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.gw == nil {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintln(w, "# gateway not initialised")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	rdb := s.gw.rdb
	var buf []byte
	g := func(help, name string, val float64, labels ...string) {
		buf = append(buf, fmt.Sprintf("# HELP %s %s\n# TYPE %s gauge\n", name, help, name)...)
		if len(labels) == 0 {
			buf = append(buf, fmt.Sprintf("%s %.6g\n", name, val)...)
		} else {
			buf = append(buf, fmt.Sprintf("%s{%s} %.6g\n", name, labels[0], val)...)
		}
	}

	// ── system mode ──────────────────────────────────────────────────────
	modeNum := map[string]float64{
		"RUNNING": 0, "PAUSED": 1, "SAFE": 2, "FLATTEN": 3, "HALTED": 4,
	}
	mode := rdb.Get(ctx, "risk:mode").Val()
	if mode == "" {
		mode = "RUNNING"
	}
	g("Current system mode (0=RUNNING 1=PAUSED 2=SAFE 3=FLATTEN 4=HALTED)",
		"arbsuite_system_mode", modeNum[mode])

	killVal := 0.0
	if rdb.Exists(ctx, "kill:switch").Val() > 0 {
		killVal = 1
	}
	g("1 if the kill switch is active", "arbsuite_kill_switch_active", killVal)

	// ── risk daemon state ─────────────────────────────────────────────────
	riskRaw := rdb.Get(ctx, "risk:state:json").Val()
	if riskRaw != "" {
		var rs map[string]interface{}
		if json.Unmarshal([]byte(riskRaw), &rs) == nil {
			if v, ok := rs["drawdown_pct"].(float64); ok {
				g("Current drawdown from peak equity (%)",
					"arbsuite_drawdown_pct", v)
			}
			if v, ok := rs["current_equity_usd"].(float64); ok {
				g("Current portfolio equity in USD",
					"arbsuite_equity_usd", v)
			}
			if venues, ok := rs["blacklisted_venues"].([]interface{}); ok {
				g("Number of currently blacklisted venues",
					"arbsuite_blacklisted_venues_count", float64(len(venues)))
			}
			if v, ok := rs["error_rate_5m_pct"].(float64); ok {
				g("Rolling 5-minute execution error rate (%)",
					"arbsuite_execution_error_rate_pct", v)
			}
		}
	}

	// ── market data staleness per venue ────────────────────────────────────
	msgs, err := rdb.XRevRangeN(ctx, "market:quotes", "+", "-", 100).Result()
	if err == nil {
		venueLastTs := make(map[string]int64)
		for _, m := range msgs {
			raw, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			var q map[string]interface{}
			if json.Unmarshal([]byte(raw), &q) != nil {
				continue
			}
			venue, _ := q["venue"].(string)
			tsMs, _ := q["ts_ms"].(float64)
			if venue != "" && int64(tsMs) > venueLastTs[venue] {
				venueLastTs[venue] = int64(tsMs)
			}
		}
		if len(venueLastTs) > 0 {
			now := time.Now().UnixMilli()
			buf = append(buf, fmt.Sprintf(
				"# HELP arbsuite_market_data_staleness_ms Milliseconds since last quote per venue\n"+
					"# TYPE arbsuite_market_data_staleness_ms gauge\n")...)
			for venue, ts := range venueLastTs {
				buf = append(buf, fmt.Sprintf(
					`arbsuite_market_data_staleness_ms{venue=%q} %d`+"\n",
					venue, now-ts)...)
			}
		}
	}

	// ── funding rates per venue+symbol ─────────────────────────────────────
	msgs2, err2 := rdb.XRevRangeN(ctx, "market:quotes", "+", "-", 200).Result()
	if err2 == nil {
		type fKey struct{ venue, symbol string }
		seen := make(map[fKey]bool)
		var frLines []byte
		for _, m := range msgs2 {
			raw, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			var q map[string]interface{}
			if json.Unmarshal([]byte(raw), &q) != nil {
				continue
			}
			venue, _ := q["venue"].(string)
			symbol, _ := q["symbol"].(string)
			k := fKey{venue, symbol}
			if seen[k] {
				continue
			}
			seen[k] = true
			fr, _ := q["funding_rate"].(float64)
			frLines = append(frLines, fmt.Sprintf(
				`arbsuite_funding_rate_8h_bps{venue=%q,symbol=%q} %.6f`+"\n",
				venue, symbol, fr*10000)...)
		}
		if len(frLines) > 0 {
			buf = append(buf, fmt.Sprintf(
				"# HELP arbsuite_funding_rate_8h_bps Perpetual funding rate per 8h period in bps\n"+
					"# TYPE arbsuite_funding_rate_8h_bps gauge\n")...)
			buf = append(buf, frLines...)
		}
	}

	// ── consensus quality per symbol ───────────────────────────────────────
	cKeys, _ := rdb.Keys(ctx, "consensus:latest:*").Result()
	if len(cKeys) > 0 {
		qualityNum := map[string]float64{"HIGH": 2, "MED": 1, "LOW": 0}
		buf = append(buf, fmt.Sprintf(
			"# HELP arbsuite_consensus_quality Consensus quality score per symbol (2=HIGH 1=MED 0=LOW)\n"+
				"# TYPE arbsuite_consensus_quality gauge\n")...)
		for _, k := range cKeys {
			raw := rdb.Get(ctx, k).Val()
			var c map[string]interface{}
			if json.Unmarshal([]byte(raw), &c) != nil {
				continue
			}
			symbol, _ := c["symbol"].(string)
			quality, _ := c["quality"].(string)
			if symbol != "" {
				buf = append(buf, fmt.Sprintf(
					`arbsuite_consensus_quality{symbol=%q} %.0f`+"\n",
					symbol, qualityNum[quality])...)
			}
		}
	}

	// ── intents emitted / approved (from Redis counters written by service loops) ──
	if v, err := rdb.Get(ctx, "metrics:intents:emitted").Int64(); err == nil {
		g("Total trade intents emitted by strategy engines",
			"arbsuite_intents_emitted_total", float64(v))
	}
	if v, err := rdb.Get(ctx, "metrics:intents:approved").Int64(); err == nil {
		g("Total trade intents approved by capital allocator",
			"arbsuite_intents_approved_total", float64(v))
	}
	if v, err := rdb.Get(ctx, "metrics:intents:rejected").Int64(); err == nil {
		g("Total trade intents rejected by capital allocator",
			"arbsuite_intents_rejected_total", float64(v))
	}
	if v, err := rdb.Get(ctx, "metrics:fills:total").Int64(); err == nil {
		g("Total simulated fill events",
			"arbsuite_fills_total", float64(v))
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write(buf)
}
