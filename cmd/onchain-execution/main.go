package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/onchain"
)

func main() {
	log.Println("onchain-execution: starting on-chain execution service")

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	walletAddr := os.Getenv("WALLET_ADDRESS")
	if walletAddr == "" {
		walletAddr = "0x0000000000000000000000000000000000000000"
		log.Println("onchain-execution: no WALLET_ADDRESS set, using zero address (read-only mode)")
	}

	wallet := onchain.NewWallet(walletAddr, 10)
	wallet.SetRPC(onchain.Ethereum, os.Getenv("ETH_RPC_URL"))
	wallet.SetRPC(onchain.Arbitrum, os.Getenv("ARB_RPC_URL"))
	wallet.SetRPC(onchain.Base, os.Getenv("BASE_RPC_URL"))

	nm := onchain.NewNonceManager()
	nm.RegisterWallet(wallet)

	txBuilder := onchain.NewTxBuilder(300000)
	txBuilder.SetChainConfig(onchain.TxBuildConfig{ChainID: onchain.Ethereum, IsEIP1559: true})
	txBuilder.SetChainConfig(onchain.TxBuildConfig{ChainID: onchain.Arbitrum, IsEIP1559: true})
	txBuilder.SetChainConfig(onchain.TxBuildConfig{ChainID: onchain.Base, IsEIP1559: true})

	sim := onchain.NewTxSimulator(1000)

	log.Printf("onchain-execution: wallet=%s chains=[ETH,ARB,BASE]", walletAddr)
	_ = sim // used when processing intents

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			pending := wallet.PendingTxs()
			log.Printf("onchain-execution: shutting down, %d pending txs", len(pending))
			return
		case <-ticker.C:
			log.Printf("onchain-execution: pending_txs=%d", wallet.PendingCount())
		}
	}
}
