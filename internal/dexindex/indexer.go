// Package dexindex provides DEX pool state normalisation, quote aggregation,
// and liquidity indexing across AMMs and order-book DEXs.
package dexindex

import (
	"math"
	"sort"
	"sync"
	"time"
)

// PoolType identifies the AMM type.
type PoolType string

const (
	UniswapV2   PoolType = "UNISWAP_V2"
	UniswapV3   PoolType = "UNISWAP_V3"
	CurveStable PoolType = "CURVE_STABLE"
	Balancer    PoolType = "BALANCER"
)

// PoolState represents a normalised DEX pool snapshot.
type PoolState struct {
	PoolAddr    string   `json:"pool_addr"`
	ChainID     uint64   `json:"chain_id"`
	Protocol    PoolType `json:"protocol"`
	Token0      string   `json:"token0"`
	Token1      string   `json:"token1"`
	Reserve0    float64  `json:"reserve0"`
	Reserve1    float64  `json:"reserve1"`
	FeeRate     float64  `json:"fee_rate"` // 0..1
	TVLUSD      float64  `json:"tvl_usd"`
	Volume24hUSD float64 `json:"volume_24h_usd"`
	Price       float64  `json:"price"` // token0 / token1
	TickLower   int      `json:"tick_lower,omitempty"` // V3 concentrated
	TickUpper   int      `json:"tick_upper,omitempty"`
	Liquidity   float64  `json:"liquidity,omitempty"` // V3 active liquidity
	LastUpdateMs int64   `json:"last_update_ms"`
}

// DEXQuote is a normalised quote from a DEX pool.
type DEXQuote struct {
	PoolAddr     string  `json:"pool_addr"`
	ChainID      uint64  `json:"chain_id"`
	Protocol     PoolType `json:"protocol"`
	TokenIn      string  `json:"token_in"`
	TokenOut     string  `json:"token_out"`
	AmountInUSD  float64 `json:"amount_in_usd"`
	AmountOutUSD float64 `json:"amount_out_usd"`
	PriceImpact  float64 `json:"price_impact"` // bps
	EffectiveRate float64 `json:"effective_rate"`
	GasCostUSD   float64 `json:"gas_cost_usd"`
	NetOutUSD    float64 `json:"net_out_usd"` // amountOut - gasCost
	ValidUntilMs int64   `json:"valid_until_ms"`
}

// Route describes a multi-hop swap path.
type Route struct {
	Hops         []RouteHop `json:"hops"`
	TotalInUSD   float64    `json:"total_in_usd"`
	TotalOutUSD  float64    `json:"total_out_usd"`
	TotalGasUSD  float64    `json:"total_gas_usd"`
	NetOutUSD    float64    `json:"net_out_usd"`
	PriceImpact  float64    `json:"price_impact"` // bps
}

// RouteHop is one step in a multi-hop route.
type RouteHop struct {
	PoolAddr string   `json:"pool_addr"`
	Protocol PoolType `json:"protocol"`
	TokenIn  string   `json:"token_in"`
	TokenOut string   `json:"token_out"`
	AmountIn float64  `json:"amount_in"`
	AmountOut float64 `json:"amount_out"`
}

// Indexer tracks DEX pool states and provides quote aggregation.
type Indexer struct {
	mu         sync.RWMutex
	pools      map[string]*PoolState // poolAddr → state
	byPair     map[string][]string   // "token0:token1" → pool addrs
	staleMs    int64
	gasPrices  map[uint64]float64 // chainID → gas price in gwei
}

// NewIndexer creates a DEX pool indexer.
func NewIndexer(staleMs int64) *Indexer {
	return &Indexer{
		pools:     make(map[string]*PoolState),
		byPair:    make(map[string][]string),
		staleMs:   staleMs,
		gasPrices: make(map[uint64]float64),
	}
}

// UpdatePool records or updates a pool state.
func (idx *Indexer) UpdatePool(state PoolState) {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	state.LastUpdateMs = time.Now().UnixMilli()
	idx.pools[state.PoolAddr] = &state

	// Index by pair (both directions)
	key1 := state.Token0 + ":" + state.Token1
	key2 := state.Token1 + ":" + state.Token0

	idx.addPairIndex(key1, state.PoolAddr)
	idx.addPairIndex(key2, state.PoolAddr)
}

func (idx *Indexer) addPairIndex(key, poolAddr string) {
	for _, addr := range idx.byPair[key] {
		if addr == poolAddr {
			return
		}
	}
	idx.byPair[key] = append(idx.byPair[key], poolAddr)
}

// SetGasPrice updates the gas price for a chain.
func (idx *Indexer) SetGasPrice(chainID uint64, gasPriceGwei float64) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.gasPrices[chainID] = gasPriceGwei
}

// GetQuotes returns quotes from all pools for a token pair.
func (idx *Indexer) GetQuotes(tokenIn, tokenOut string, amountInUSD float64) []DEXQuote {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	now := time.Now().UnixMilli()
	key := tokenIn + ":" + tokenOut
	poolAddrs := idx.byPair[key]

	var quotes []DEXQuote
	for _, addr := range poolAddrs {
		pool, ok := idx.pools[addr]
		if !ok || now-pool.LastUpdateMs > idx.staleMs {
			continue
		}

		quote := idx.simulateSwap(pool, tokenIn, tokenOut, amountInUSD)
		if quote != nil {
			quotes = append(quotes, *quote)
		}
	}

	// Sort by net output descending (best quote first)
	sort.Slice(quotes, func(i, j int) bool {
		return quotes[i].NetOutUSD > quotes[j].NetOutUSD
	})

	return quotes
}

// BestRoute finds the optimal single-hop or multi-hop route.
func (idx *Indexer) BestRoute(tokenIn, tokenOut string, amountInUSD float64) *Route {
	// Direct quotes
	direct := idx.GetQuotes(tokenIn, tokenOut, amountInUSD)
	if len(direct) == 0 {
		return nil
	}

	best := direct[0]
	return &Route{
		Hops: []RouteHop{{
			PoolAddr: best.PoolAddr,
			Protocol: best.Protocol,
			TokenIn:  tokenIn,
			TokenOut: tokenOut,
			AmountIn: amountInUSD,
			AmountOut: best.AmountOutUSD,
		}},
		TotalInUSD:  amountInUSD,
		TotalOutUSD: best.AmountOutUSD,
		TotalGasUSD: best.GasCostUSD,
		NetOutUSD:   best.NetOutUSD,
		PriceImpact: best.PriceImpact,
	}
}

// PoolCount returns the number of tracked pools.
func (idx *Indexer) PoolCount() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.pools)
}

// TopPoolsByTVL returns the top N pools by TVL.
func (idx *Indexer) TopPoolsByTVL(n int) []*PoolState {
	idx.mu.RLock()
	defer idx.mu.RUnlock()

	var pools []*PoolState
	for _, p := range idx.pools {
		pools = append(pools, p)
	}
	sort.Slice(pools, func(i, j int) bool {
		return pools[i].TVLUSD > pools[j].TVLUSD
	})
	if len(pools) > n {
		pools = pools[:n]
	}
	return pools
}

func (idx *Indexer) simulateSwap(pool *PoolState, tokenIn, tokenOut string, amountInUSD float64) *DEXQuote {
	if pool.Reserve0 <= 0 || pool.Reserve1 <= 0 {
		return nil
	}

	var reserveIn, reserveOut float64
	if tokenIn == pool.Token0 {
		reserveIn = pool.Reserve0
		reserveOut = pool.Reserve1
	} else {
		reserveIn = pool.Reserve1
		reserveOut = pool.Reserve0
	}

	// Constant product AMM: amountOut = reserveOut * amountIn / (reserveIn + amountIn) * (1 - fee)
	amountOutUSD := reserveOut * amountInUSD * (1 - pool.FeeRate) / (reserveIn + amountInUSD)
	priceImpact := (1 - amountOutUSD/amountInUSD) * 10000 // bps

	// Gas cost estimate
	gasUsed := 150000.0 // typical swap gas
	if pool.Protocol == UniswapV3 {
		gasUsed = 200000
	} else if pool.Protocol == CurveStable {
		gasUsed = 300000
	}

	gasPriceGwei := idx.gasPrices[pool.ChainID]
	if gasPriceGwei == 0 {
		gasPriceGwei = 30 // default
	}
	ethPriceUSD := 2000.0 // placeholder
	gasCostUSD := gasUsed * gasPriceGwei * 1e-9 * ethPriceUSD

	effectiveRate := amountOutUSD / amountInUSD
	if math.IsNaN(effectiveRate) || math.IsInf(effectiveRate, 0) {
		return nil
	}

	return &DEXQuote{
		PoolAddr:      pool.PoolAddr,
		ChainID:       pool.ChainID,
		Protocol:      pool.Protocol,
		TokenIn:       tokenIn,
		TokenOut:      tokenOut,
		AmountInUSD:   amountInUSD,
		AmountOutUSD:  amountOutUSD,
		PriceImpact:   priceImpact,
		EffectiveRate: effectiveRate,
		GasCostUSD:    gasCostUSD,
		NetOutUSD:     amountOutUSD - gasCostUSD,
		ValidUntilMs:  time.Now().UnixMilli() + 2000,
	}
}
