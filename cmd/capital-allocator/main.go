package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/allocator"
	"github.com/ezyjtw/consensus-engine/internal/eventbus"
)

func main() {
	cfgPath := flag.String("config", "configs/policies/allocator.yaml",
		"Path to capital allocator policy YAML")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", v)
	}

	policy, err := allocator.LoadPolicy(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load policy: %v", err)
	}
	log.Printf("capital-allocator: strategies=%v venues=%v",
		keys(policy.PerStrategyMaxUSD), keys(policy.PerVenueMaxUSD))

	engine := allocator.NewEngine(policy)

	bus, err := eventbus.NewAllocatorBus(eventbus.AllocatorBusConfig{
		Addr:          policy.Redis.Addr,
		Password:      policy.Redis.Password,
		UseTLS:        policy.Redis.UseTLS,
		InputStream:   policy.Redis.InputStream,
		OutputStream:  policy.Redis.OutputStream,
		FillsStream:   policy.Redis.FillsStream,
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

	statsTicker := time.NewTicker(30 * time.Second)
	defer statsTicker.Stop()

	log.Println("capital-allocator: started")

	for {
		select {
		case <-ctx.Done():
			log.Println("capital-allocator: shutdown")
			return
		case <-statsTicker.C:
			log.Printf("capital-allocator: approved=%d rejected=%v",
				engine.Approved, engine.Rejected)
		default:
		}

		if bus.KillSwitchActive(ctx) {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// ── Release notional for completed fills ──────────────────────────
		// This MUST run before evaluating new intents so caps are up-to-date.
		fills, err := bus.ReadFills(ctx)
		if err != nil {
			log.Printf("capital-allocator: read fills error: %v", err)
		}
		for _, f := range fills {
			if f.IntentExpired {
				continue // expired intents never deployed capital
			}
			// Release the notional across all legs using the primary venue.
			// For two-leg arb both legs are counted, so release full fill notional.
			totalNotional := 0.0
			for _, leg := range f.Legs {
				totalNotional += leg.FilledNotionalUSD
				engine.ReleaseNotional(f.Strategy, leg.Venue, leg.FilledNotionalUSD)
			}
			if totalNotional > 0 {
				log.Printf("capital-allocator: released %.2f USD notional for strategy=%s",
					totalNotional, f.Strategy)
			}
		}

		// ── Evaluate new intents ──────────────────────────────────────────
		intents, err := bus.ReadIntents(ctx)
		if err != nil {
			log.Printf("capital-allocator: read error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		mode := bus.SystemMode(ctx)
		for _, intent := range intents {
			quality := intent.Constraints.MinQuality
			outcome := engine.Evaluate(intent, mode, quality)
			if !outcome.Approved {
				continue
			}
			if err := bus.PublishApproved(ctx, outcome.Intent); err != nil {
				log.Printf("capital-allocator: publish error: %v", err)
			}
		}
	}
}

func keys(m map[string]float64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
