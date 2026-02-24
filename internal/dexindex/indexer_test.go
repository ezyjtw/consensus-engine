package dexindex

import (
	"testing"
)

func TestIndexerUpdateAndQuote(t *testing.T) {
	idx := NewIndexer(60000) // 60s stale

	idx.UpdatePool(PoolState{
		PoolAddr: "0xPool1",
		ChainID:  1,
		Protocol: UniswapV2,
		Token0:   "WETH",
		Token1:   "USDC",
		Reserve0: 5_000_000,
		Reserve1: 5_000_000,
		FeeRate:  0.003,
		TVLUSD:   10_000_000,
	})

	quotes := idx.GetQuotes("WETH", "USDC", 10000)
	if len(quotes) != 1 {
		t.Fatalf("expected 1 quote, got %d", len(quotes))
	}

	q := quotes[0]
	if q.AmountOutUSD <= 0 {
		t.Error("expected positive output amount")
	}
	if q.PriceImpact <= 0 {
		t.Error("expected positive price impact")
	}
	if q.PoolAddr != "0xPool1" {
		t.Errorf("expected pool 0xPool1, got %s", q.PoolAddr)
	}
}

func TestIndexerMultiplePools(t *testing.T) {
	idx := NewIndexer(60000)

	// Two pools for same pair, different liquidity
	idx.UpdatePool(PoolState{
		PoolAddr: "0xSmall",
		ChainID:  1,
		Protocol: UniswapV2,
		Token0:   "WETH",
		Token1:   "USDC",
		Reserve0: 100_000,
		Reserve1: 100_000,
		FeeRate:  0.003,
		TVLUSD:   200_000,
	})
	idx.UpdatePool(PoolState{
		PoolAddr: "0xBig",
		ChainID:  1,
		Protocol: UniswapV3,
		Token0:   "WETH",
		Token1:   "USDC",
		Reserve0: 50_000_000,
		Reserve1: 50_000_000,
		FeeRate:  0.0005,
		TVLUSD:   100_000_000,
	})

	quotes := idx.GetQuotes("WETH", "USDC", 50000)
	if len(quotes) != 2 {
		t.Fatalf("expected 2 quotes, got %d", len(quotes))
	}

	// Best quote (first) should be from the bigger pool (less slippage)
	if quotes[0].PoolAddr != "0xBig" {
		t.Errorf("expected best quote from 0xBig, got %s", quotes[0].PoolAddr)
	}
}

func TestBestRoute(t *testing.T) {
	idx := NewIndexer(60000)

	idx.UpdatePool(PoolState{
		PoolAddr: "0xPool1",
		ChainID:  1,
		Protocol: UniswapV2,
		Token0:   "WETH",
		Token1:   "USDC",
		Reserve0: 10_000_000,
		Reserve1: 10_000_000,
		FeeRate:  0.003,
		TVLUSD:   20_000_000,
	})

	route := idx.BestRoute("WETH", "USDC", 5000)
	if route == nil {
		t.Fatal("expected a route")
	}
	if len(route.Hops) != 1 {
		t.Errorf("expected 1 hop, got %d", len(route.Hops))
	}
	if route.NetOutUSD <= 0 {
		t.Error("expected positive net output")
	}
}

func TestTopPoolsByTVL(t *testing.T) {
	idx := NewIndexer(60000)

	idx.UpdatePool(PoolState{PoolAddr: "0xA", TVLUSD: 1_000_000, Reserve0: 1, Reserve1: 1})
	idx.UpdatePool(PoolState{PoolAddr: "0xB", TVLUSD: 50_000_000, Reserve0: 1, Reserve1: 1})
	idx.UpdatePool(PoolState{PoolAddr: "0xC", TVLUSD: 10_000_000, Reserve0: 1, Reserve1: 1})

	top := idx.TopPoolsByTVL(2)
	if len(top) != 2 {
		t.Fatalf("expected 2, got %d", len(top))
	}
	if top[0].PoolAddr != "0xB" {
		t.Errorf("expected 0xB first, got %s", top[0].PoolAddr)
	}
	if top[1].PoolAddr != "0xC" {
		t.Errorf("expected 0xC second, got %s", top[1].PoolAddr)
	}
}
