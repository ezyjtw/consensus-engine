package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourorg/arbsuite/internal/allocator"
	"github.com/yourorg/arbsuite/internal/eventbus"
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

		intents, err := bus.ReadIntents(ctx)
		if err != nil {
			log.Printf("capital-allocator: read error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		mode := bus.SystemMode(ctx)
		// Note: consensus quality is embedded in the intent constraints.
		// For V1, we use the quality stored in Constraints.MinQuality as a proxy
		// for the current consensus quality (the strategy engine already validated it).
		for _, intent := range intents {
			quality := intent.Constraints.MinQuality // proxy for current quality
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
