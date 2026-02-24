package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/treasuryyield"
)

func main() {
	log.Println("treasury-yield-router: starting yield optimiser")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := treasuryyield.RouterConfig{
		MaxRiskScore:       40,
		MaxSingleAllocPct:  30,
		MaxLockDays:        7,
		MinAPYPct:          1.0,
		ReservesPct:        20,
		RebalanceThreshPct: 5,
	}

	router := treasuryyield.NewRouter(cfg)

	log.Printf("treasury-yield-router: max_risk=%0.f max_alloc=%0.f%% reserves=%0.f%%",
		cfg.MaxRiskScore, cfg.MaxSingleAllocPct, cfg.ReservesPct)

	rebalanceTicker := time.NewTicker(5 * time.Minute)
	defer rebalanceTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("treasury-yield-router: shutting down, sources=%d",
				router.SourceCount())
			return
		case <-rebalanceTicker.C:
			log.Printf("treasury-yield-router: sources=%d", router.SourceCount())
		}
	}
}
