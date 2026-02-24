package main

import (
	"context"
	"encoding/hex"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/redis/go-redis/v9"

	"github.com/ezyjtw/consensus-engine/internal/eventbus"
	"github.com/ezyjtw/consensus-engine/internal/exchange"
	"github.com/ezyjtw/consensus-engine/internal/exchange/binance"
	"github.com/ezyjtw/consensus-engine/internal/exchange/bybit"
	"github.com/ezyjtw/consensus-engine/internal/exchange/coinbase"
	"github.com/ezyjtw/consensus-engine/internal/exchange/deribit"
	"github.com/ezyjtw/consensus-engine/internal/exchange/okx"
	"github.com/ezyjtw/consensus-engine/internal/treasury"
)

func main() {
	cfgPath := flag.String("config", "configs/policies/treasury.yaml",
		"Path to treasury config YAML")
	flag.Parse()

	cfg, err := treasury.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("treasury: venue=%s auto_convert=%v convert_to=%s",
		cfg.TreasuryVenue, cfg.AutoConvert, cfg.ConvertTo)

	// Connect to Redis.
	bus, err := eventbus.NewStreamClient(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.UseTLS)
	if err != nil {
		log.Fatalf("failed to connect to Redis: %v", err)
	}
	defer bus.Close()

	// Set up credential store.
	masterKeyHex := os.Getenv("DASHBOARD_MASTER_KEY")
	if masterKeyHex == "" {
		log.Fatalf("DASHBOARD_MASTER_KEY env var is required")
	}
	masterKey, err := hex.DecodeString(masterKeyHex)
	if err != nil || len(masterKey) != 32 {
		log.Fatalf("DASHBOARD_MASTER_KEY must be 64 hex chars (32 bytes)")
	}

	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Password: cfg.Redis.Password,
	})
	defer rdb.Close()

	credStore := exchange.NewCredentialStore(rdb, masterKey)
	registry := exchange.NewRegistry(credStore)

	// Register venue factories.
	registry.RegisterFactory("coinbase", coinbase.New)
	registry.RegisterFactory("binance", binance.New)
	registry.RegisterFactory("okx", okx.New)
	registry.RegisterFactory("bybit", bybit.New)
	registry.RegisterFactory("deribit", deribit.New)

	engine := treasury.NewEngine(cfg, registry, bus)

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Run all treasury subsystems concurrently.
	go engine.RunDepositWatcher(ctx)
	go engine.RunSweep(ctx)
	go engine.RunReconciliation(ctx)

	log.Printf("treasury: started (%d allocation venues)", len(cfg.Allocation))

	<-ctx.Done()
	log.Printf("treasury: shutdown")
}
