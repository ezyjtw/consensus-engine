package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/yourorg/arbsuite/internal/marketdata"
)

func main() {
	cfgPath := flag.String("config", "configs/market_data.yaml", "Path to market data config YAML")
	flag.Parse()

	cfg, err := marketdata.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	log.Printf("market-data: tenant=%s output_stream=%s redis=%s",
		cfg.TenantID, cfg.Redis.OutputStream, cfg.Redis.Addr)
	log.Printf("market-data: venues=%d orderbook_depth=%d reconnect_backoffs=%v",
		len(cfg.Venues), cfg.OrderbookDepth, cfg.ReconnectBackoffMs)

	pub, err := marketdata.NewPublisher(cfg.Redis)
	if err != nil {
		log.Fatalf("failed to create publisher: %v", err)
	}
	defer func() {
		if err := pub.Close(); err != nil {
			log.Printf("publisher close error: %v", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup

	for venueName, venueCfg := range cfg.Venues {
		venueName := venueName
		venueCfg := venueCfg

		// Ensure a symbol map exists (fall back to identity map).
		if venueCfg.SymbolMap == nil {
			venueCfg.SymbolMap = make(map[string]string)
			for _, sym := range venueCfg.Symbols {
				venueCfg.SymbolMap[sym] = sym
			}
		}

		log.Printf("market-data: starting adapter venue=%s symbols=%v fee=%.1fbps",
			venueName, venueCfg.Symbols, venueCfg.FeeBpsTaker)

		switch venueName {
		case "binance":
			adapter := marketdata.NewBinanceAdapter(
				venueCfg, pub, cfg.TenantID,
				cfg.ReconnectBackoffMs, cfg.OrderbookDepth,
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				adapter.Run(ctx)
				log.Printf("binance adapter stopped")
			}()

		case "okx":
			adapter := marketdata.NewOKXAdapter(
				venueCfg, pub, cfg.TenantID, cfg.ReconnectBackoffMs,
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				adapter.Run(ctx)
				log.Printf("okx adapter stopped")
			}()

		case "bybit":
			adapter := marketdata.NewBybitAdapter(
				venueCfg, pub, cfg.TenantID,
				cfg.ReconnectBackoffMs, cfg.OrderbookDepth,
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				adapter.Run(ctx)
				log.Printf("bybit adapter stopped")
			}()

		case "deribit":
			adapter := marketdata.NewDeribitAdapter(
				venueCfg, pub, cfg.TenantID,
				cfg.ReconnectBackoffMs, cfg.OrderbookDepth,
			)
			wg.Add(1)
			go func() {
				defer wg.Done()
				adapter.Run(ctx)
				log.Printf("deribit adapter stopped")
			}()

		default:
			log.Printf("market-data: WARNING unknown venue %q — skipping", venueName)
		}
	}

	log.Println("market-data: all adapters running")
	wg.Wait()
	log.Println("market-data: shutdown complete")
}
