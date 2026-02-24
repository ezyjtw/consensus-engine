package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/eventbus"
	"github.com/ezyjtw/consensus-engine/internal/redact"
	"github.com/ezyjtw/consensus-engine/internal/risk"
)

func main() {
	cfgPath := flag.String("config", "configs/policies/risk.yaml",
		"Path to risk policy YAML")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", redact.RedisAddr(v))
	}

	policy, err := risk.LoadPolicy(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load policy: %v", err)
	}
	log.Printf("risk-daemon: tenant=%s max_drawdown=%.1f%% safe_mode_drawdown=%.1f%% max_error_rate=%.1f%%",
		policy.TenantID, policy.MaxDrawdownPct, policy.SafeModeDrawdownPct, policy.MaxErrorRate5mPct)

	daemon := risk.NewDaemon(policy)

	bus, err := eventbus.NewRiskBus(eventbus.RiskBusConfig{
		Addr:          policy.Redis.Addr,
		Password:      policy.Redis.Password,
		UseTLS:        policy.Redis.UseTLS,
		EventsStream:  policy.Redis.EventsStream,
		StatusStream:  policy.Redis.StatusStream,
		AlertsStream:  policy.Redis.AlertsStream,
		StateStream:   policy.Redis.StateStream,
		ConsumerGroup: policy.Redis.ConsumerGroup,
		ConsumerName:  policy.Redis.ConsumerName,
		BlockMs:       time.Duration(policy.Redis.BlockMs) * time.Millisecond,
		BatchSize:     policy.Redis.BatchSize,
	})
	if err != nil {
		log.Fatalf("failed to create bus: %v", err)
	}
	defer bus.Close() //nolint:errcheck

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Set initial mode as RUNNING.
	initialState := daemon.CurrentState()
	if err := bus.PublishState(ctx, initialState); err != nil {
		log.Printf("risk-daemon: publish initial state: %v", err)
	}

	tickTicker := time.NewTicker(5 * time.Second)
	defer tickTicker.Stop()
	stateTicker := time.NewTicker(30 * time.Second)
	defer stateTicker.Stop()
	// FLATTEN polling: re-emit close intents every 10s until positions clear.
	flattenTicker := time.NewTicker(10 * time.Second)
	defer flattenTicker.Stop()

	flattenActive := false

	log.Println("risk-daemon: started in RUNNING mode")

	for {
		select {
		case <-ctx.Done():
			log.Println("risk-daemon: shutdown")
			return

		case <-tickTicker.C:
			// Check for operator-commanded mode changes (from dashboard / API).
			if cmd := bus.GetCommandedMode(ctx); cmd != "" {
				bus.ClearCommandedMode(ctx)
				daemon.SetMode(risk.Mode(cmd), "operator_command")
				if risk.Mode(cmd) == risk.ModeFlatten {
					flattenActive = true
					log.Println("risk-daemon: FLATTEN commanded — beginning position close sequence")
				}
			}
			// Honour kill switch → PAUSED.
			if bus.KillSwitchActive(ctx) {
				if daemon.CurrentMode() == risk.ModeRunning {
					daemon.SetMode(risk.ModePaused, "kill_switch")
				}
			}
			// Periodic risk evaluation (drawdown, error rate, blacklist count).
			alerts := daemon.Tick()
			publishAlerts(ctx, bus, alerts)
			// If Tick() triggered FLATTEN, begin the sequence.
			if daemon.CurrentMode() == risk.ModeFlatten && !flattenActive {
				flattenActive = true
				log.Println("risk-daemon: FLATTEN triggered automatically — beginning position close sequence")
			}

		case <-flattenTicker.C:
			if !flattenActive {
				continue
			}
			// ── FLATTEN sequence ───────────────────────────────────────────────
			// Step 1: Emit close intents for all open paper positions.
			n, err := bus.FlattenOpenPositions(ctx, policy.TenantID, "trade:intents:approved")
			if err != nil {
				log.Printf("risk-daemon: FLATTEN emit error: %v", err)
			}
			if n > 0 {
				log.Printf("risk-daemon: FLATTEN emitted %d close intents", n)
				// Publish an alert for the audit trail.
				publishAlerts(ctx, bus, []risk.Alert{{
					TenantID: policy.TenantID,
					TsMs:     time.Now().UnixMilli(),
					Source:   "flatten_sequence",
					Severity: "CRITICAL",
					Message:  "FLATTEN in progress — close intents emitted for open positions",
				}})
			}
			// Step 2: Check if all positions are closed → transition to HALTED.
			open := bus.OpenPositionCount(ctx, policy.TenantID)
			if open == 0 {
				log.Println("risk-daemon: FLATTEN complete — all positions closed, transitioning to HALTED")
				daemon.SetMode(risk.ModeHalted, "flatten_complete")
				flattenActive = false
				state := daemon.CurrentState()
				if err := bus.PublishState(ctx, state); err != nil {
					log.Printf("risk-daemon: publish HALTED state: %v", err)
				}
				publishAlerts(ctx, bus, []risk.Alert{{
					TenantID: policy.TenantID,
					TsMs:     time.Now().UnixMilli(),
					Source:   "flatten_sequence",
					Severity: "CRITICAL",
					Message:  "FLATTEN complete — system HALTED. Manual restart required.",
				}})
			}

		case <-stateTicker.C:
			state := daemon.CurrentState()
			if err := bus.PublishState(ctx, state); err != nil {
				log.Printf("risk-daemon: publish state: %v", err)
			}
			log.Printf("risk-daemon: mode=%s drawdown=%.2f%% error_rate=%.1f%%",
				state.Mode, state.DrawdownPct, state.ErrorRate5mPct)

		default:
			// Drain execution events.
			events, err := bus.ReadExecutionEvents(ctx)
			if err != nil {
				log.Printf("risk-daemon: read exec events: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			for _, ev := range events {
				isError := ev.EventType == "ORDER_REJECTED" || ev.EventType == "HEDGE_FAILED"
				alerts := daemon.RecordFill(0, isError)
				publishAlerts(ctx, bus, alerts)
			}

			// Drain venue status updates.
			statusUpdates, err := bus.ReadVenueStatusUpdates(ctx)
			if err != nil {
				continue
			}
			for _, su := range statusUpdates {
				if su.Status == consensus.StateBlacklisted {
					alerts := daemon.RecordBlacklist(string(su.Venue), su.TtlMs)
					publishAlerts(ctx, bus, alerts)
				}
			}
		}
	}
}

func publishAlerts(ctx context.Context, bus *eventbus.RiskBus, alerts []risk.Alert) {
	for _, a := range alerts {
		log.Printf("risk-daemon: ALERT severity=%s source=%s msg=%q",
			a.Severity, a.Source, a.Message)
		if err := bus.PublishAlert(ctx, a); err != nil && ctx.Err() == nil {
			log.Printf("risk-daemon: publish alert: %v", err)
		}
	}
}
