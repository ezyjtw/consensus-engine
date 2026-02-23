package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yourorg/arbsuite/internal/arb"
	"github.com/yourorg/arbsuite/internal/eventbus"
)

func main() {
	cfgPath := flag.String("config",
		"configs/policies/arb_engine.yaml", "Path to arb engine policy YAML")
	flag.Parse()

	// Log env vars that override config (mirrors consensus-engine convention).
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		log.Printf("REDIS_ADDR detected: %s", addr)
	} else {
		log.Println("REDIS_ADDR not set, using default from policy file")
	}
	if os.Getenv("REDIS_PASSWORD") != "" {
		log.Println("REDIS_PASSWORD detected")
	}
	if tls := os.Getenv("REDIS_TLS"); tls != "" {
		log.Printf("REDIS_TLS detected: %q (must be exactly \"true\" to enable TLS)", tls)
	}

	policy, err := arb.LoadPolicy(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load policy: %v", err)
	}

	log.Printf("Active arb policy: min_quality=%s min_edge_HIGH=%.1f min_edge_MED=%.1f "+
		"cooldown=%dms size_ladder=%v max_slippage=%.1fbps allow_warn=%v ignore_flagged=%v",
		policy.MinConsensusQuality,
		policy.MinEdgeBpsNet["HIGH"], policy.MinEdgeBpsNet["MED"],
		policy.CooldownMs,
		policy.SizeLadderUSD,
		policy.MaxSlippageBps,
		policy.AllowWarnVenues,
		policy.IgnoreFlaggedVenues,
	)
	log.Printf("Redis: addr=%s tls=%v input=%s output=%s",
		policy.Redis.Addr, policy.Redis.UseTLS,
		policy.Redis.InputStream, policy.Redis.OutputIntents,
	)

	engine := arb.NewEngine(policy)

	bus, err := eventbus.NewArbBus(eventbus.ArbRedisConfig{
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
		log.Fatalf("failed to create event bus: %v", err)
	}
	defer func() {
		if err := bus.Close(); err != nil {
			log.Printf("error closing bus: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Println("arb-opportunity-engine started")

	statsTicker := time.NewTicker(30 * time.Second)
	defer statsTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("shutting down")
			return
		case <-statsTicker.C:
			logStats(engine)
		default:
		}

		if bus.KillSwitchActive(ctx) {
			log.Println("kill switch active — pausing arb engine")
			time.Sleep(500 * time.Millisecond)
			continue
		}

		updates, err := bus.ReadConsensusUpdates(ctx)
		if err != nil {
			log.Printf("read error: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		for _, update := range updates {
			intents := engine.Process(update)
			for _, intent := range intents {
				if err := bus.PublishTradeIntent(ctx, intent); err != nil {
					log.Printf("publish error (symbol=%s id=%s): %v",
						intent.Symbol, intent.IntentID, err)
					continue
				}
				log.Printf("intent: symbol=%s buy_on=%s sell_on=%s size=$%.0f "+
					"net_edge=%.2fbps profit=$%.2f ttl=%dms id=%s",
					intent.Symbol,
					intent.Legs[0].Venue,
					intent.Legs[1].Venue,
					intent.Legs[0].NotionalUSD,
					intent.Expected.EdgeBpsNet,
					intent.Expected.ProfitUSDNet,
					intent.ExpiresMs-intent.TsMs,
					intent.IntentID,
				)
			}
		}
	}
}

func logStats(e *arb.Engine) {
	for sym, count := range e.Emitted {
		if count > 0 {
			log.Printf("stats[emitted]: symbol=%s count=%d", sym, count)
		}
	}
	for reason, count := range e.Rejected {
		if count > 0 {
			log.Printf("stats[rejected]: reason=%s count=%d", reason, count)
		}
	}
}
