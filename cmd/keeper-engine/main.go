package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/keeper"
)

func main() {
	log.Println("keeper-engine: starting liquidation keeper")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := keeper.KeeperConfig{
		MinProfitUSD:    10,
		MaxPositionUSD:  500000,
		GasEstimateGwei: 30,
		GasLimitLiq:     500000,
		HealthFactorMax: 1.0,
		MaxConcurrent:   3,
		CooldownMs:      30000,
		FlashLoanEnabled: true,
	}

	engine := keeper.NewEngine(cfg)

	log.Printf("keeper-engine: min_profit=$%.0f max_position=$%.0f max_concurrent=%d",
		cfg.MinProfitUSD, cfg.MaxPositionUSD, cfg.MaxConcurrent)

	scanTicker := time.NewTicker(2 * time.Second)
	defer scanTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			stats := engine.Stats()
			log.Printf("keeper-engine: shutting down, executed=%d total_profit=$%.2f",
				stats.LiquidationsExec, stats.TotalProfitUSD)
			return
		case <-scanTicker.C:
			candidates := engine.Scan()
			if len(candidates) > 0 {
				log.Printf("keeper-engine: found %d liquidation candidates", len(candidates))
				for _, c := range candidates {
					log.Printf("  %s/%s health=%.3f net_profit=$%.2f",
						c.Protocol, c.Account, c.HealthFactor, c.NetProfitUSD)
				}
			}
		}
	}
}
