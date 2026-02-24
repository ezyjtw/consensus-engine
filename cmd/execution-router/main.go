package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/eventbus"
	"github.com/ezyjtw/consensus-engine/internal/execution"
	"github.com/ezyjtw/consensus-engine/internal/redact"
)

func main() {
	cfgPath := flag.String("config", "configs/execution_router.yaml",
		"Path to execution router config YAML")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", redact.RedisAddr(v))
	}

	cfg, err := execution.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	log.Printf("execution-router: mode=%s sim_slippage=%.1fbps sim_latency=%dms",
		cfg.TradingMode, cfg.SimSlippageBps, cfg.SimLatencyMs)

	// Determine fills stream based on trading mode.
	fillsStream := "demo:fills"
	if cfg.TradingMode == "LIVE" {
		fillsStream = "live:fills"
	}

	bus, err := eventbus.NewExecutionBus(eventbus.ExecutionBusConfig{
		Addr:            cfg.Redis.Addr,
		Password:        cfg.Redis.Password,
		UseTLS:          cfg.Redis.UseTLS,
		IntentsStream:   cfg.Redis.InputStream,
		ConsensusStream: "consensus:updates",
		EventsStream:    cfg.Redis.OutputEvents,
		FillsStream:     fillsStream,
		ConsumerGroup:   cfg.Redis.ConsumerGroup,
		ConsumerName:    cfg.Redis.ConsumerName,
		BlockMs:         time.Duration(cfg.Redis.BlockMs) * time.Millisecond,
		BatchSize:       cfg.Redis.BatchSize,
	})
	if err != nil {
		log.Fatalf("failed to create bus: %v", err)
	}
	defer bus.Close() //nolint:errcheck

	priceCache := execution.NewPriceCache()
	executor := execution.NewPaperExecutor(cfg, priceCache)

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Background goroutine: keep consensus price cache warm.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			updates, err := bus.ReadConsensusUpdates(ctx)
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			for _, u := range updates {
				priceCache.Update(u)
			}
		}
	}()

	log.Printf("execution-router: started in %s mode", cfg.TradingMode)

	statsTicker := time.NewTicker(30 * time.Second)
	defer statsTicker.Stop()

	filled, expired, errors := 0, 0, 0

	for {
		select {
		case <-ctx.Done():
			log.Printf("execution-router: shutdown (filled=%d expired=%d errors=%d)",
				filled, expired, errors)
			return
		case <-statsTicker.C:
			log.Printf("execution-router: filled=%d expired=%d errors=%d mode=%s",
				filled, expired, errors, cfg.TradingMode)
		default:
		}

		mode := bus.SystemMode(ctx)
		if mode == "FLATTEN" || mode == "HALTED" {
			log.Printf("execution-router: system mode=%s — paused", mode)
			time.Sleep(1 * time.Second)
			continue
		}
		if bus.KillSwitchActive(ctx) {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		intents, err := bus.ReadApprovedIntents(ctx)
		if err != nil {
			log.Printf("execution-router: read error: %v", err)
			errors++
			time.Sleep(100 * time.Millisecond)
			continue
		}

		for _, intent := range intents {
			events, fill := executor.Execute(ctx, intent)
			if fill != nil && fill.IntentExpired {
				expired++
				continue
			}
			if fill == nil {
				continue // no consensus price available yet
			}

			for _, ev := range events {
				if pubErr := bus.PublishEvent(ctx, ev); pubErr != nil {
					log.Printf("execution-router: publish event: %v", pubErr)
					errors++
				}
			}
			if err := bus.PublishFill(ctx, fill); err != nil {
				log.Printf("execution-router: publish fill: %v", err)
				errors++
			}
			filled++
			log.Printf("execution-router: filled intent=%s strategy=%s symbol=%s pnl=$%.2f",
				intent.IntentID, intent.Strategy, intent.Symbol, fill.NetPnLUSD)
		}
	}
}
