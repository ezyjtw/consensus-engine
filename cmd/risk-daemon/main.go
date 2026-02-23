package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourorg/arbsuite/internal/consensus"
	"github.com/yourorg/arbsuite/internal/eventbus"
	"github.com/yourorg/arbsuite/internal/risk"
)

func main() {
	cfgPath := flag.String("config", "configs/policies/risk.yaml",
		"Path to risk policy YAML")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", v)
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
	defer bus.Close()

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
			}
			// Also honour kill switch → PAUSED.
			if bus.KillSwitchActive(ctx) {
				if daemon.CurrentMode() == risk.ModeRunning {
					daemon.SetMode(risk.ModePaused, "kill_switch")
				}
			}
			// Periodic risk evaluation.
			alerts := daemon.Tick()
			publishAlerts(ctx, bus, alerts)

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
				alerts := daemon.RecordFill(0, isError) // PnL tracked via fills stream
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
