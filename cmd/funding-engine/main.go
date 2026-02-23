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
	"github.com/ezyjtw/consensus-engine/internal/funding"
)

func main() {
	cfgPath := flag.String("config", "configs/policies/funding_engine.yaml",
		"Path to funding engine policy YAML")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", v)
	}

	policy, err := funding.LoadPolicy(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load policy: %v", err)
	}
	log.Printf("funding-engine: symbols=%v venues=%v eval_interval=%ds min_yield_HIGH=%.1f%% min_yield_MED=%.1f%%",
		policy.Symbols, policy.Venues, policy.EvalIntervalS,
		policy.MinAnnualYieldPct["HIGH"], policy.MinAnnualYieldPct["MED"])

	engine := funding.NewEngine(policy)

	bus, err := eventbus.NewFundingBus(eventbus.FundingBusConfig{
		Addr:          policy.Redis.Addr,
		Password:      policy.Redis.Password,
		UseTLS:        policy.Redis.UseTLS,
		InputStream:   policy.Redis.InputStream,
		OutputIntents: policy.Redis.OutputIntents,
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

	evalTicker := time.NewTicker(time.Duration(policy.EvalIntervalS) * time.Second)
	defer evalTicker.Stop()

	statsTicker := time.NewTicker(60 * time.Second)
	defer statsTicker.Stop()

	log.Println("funding-engine: started")

	for {
		select {
		case <-ctx.Done():
			log.Println("funding-engine: shutdown")
			return

		case <-statsTicker.C:
			for k, v := range engine.Emitted {
				if v > 0 {
					log.Printf("stats[emitted]: strategy=%s count=%d", k, v)
				}
			}
			for k, v := range engine.Rejected {
				if v > 0 {
					log.Printf("stats[rejected]: reason=%s count=%d", k, v)
				}
			}

		case <-evalTicker.C:
			if bus.KillSwitchActive(ctx) {
				log.Println("funding-engine: kill switch active — skipping eval")
				continue
			}
			tenantID := "default"
			intents := engine.Evaluate(tenantID)
			for _, intent := range intents {
				if err := bus.PublishIntent(ctx, intent); err != nil {
					log.Printf("funding-engine: publish error: %v", err)
				}
			}

		default:
			// Drain market:quotes to keep funding rate state fresh.
			if bus.KillSwitchActive(ctx) {
				time.Sleep(500 * time.Millisecond)
				continue
			}
			quotes, err := bus.ReadQuotes(ctx)
			if err != nil {
				log.Printf("funding-engine: read error: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			for _, q := range quotes {
				engine.UpdateQuote(q)
			}
		}
	}
}
