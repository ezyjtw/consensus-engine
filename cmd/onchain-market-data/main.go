package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/dexindex"
)

func main() {
	log.Println("onchain-market-data: starting DEX pool state indexer")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	staleMs := int64(30000) // 30s stale threshold
	idx := dexindex.NewIndexer(staleMs)

	log.Printf("onchain-market-data: indexer ready, stale_ms=%d", staleMs)

	// Main loop: poll for pool state updates
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Printf("onchain-market-data: shutting down, tracked %d pools", idx.PoolCount())
			return
		case <-ticker.C:
			log.Printf("onchain-market-data: pools=%d", idx.PoolCount())
		}
	}
}
