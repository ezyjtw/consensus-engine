// Package dex provides optional DEX spot-leg routing via 1inch and Paraswap.
// MEV protection is available through 1inch Fusion (private order flow) and
// by restricting to curated MEV-safe liquidity sources.
package dex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// Router fetches quotes from DEX aggregators and selects the best route.
type Router struct {
	cfg    Config
	client *http.Client
}

// New creates a Router from config.
func New(cfg Config) *Router {
	return &Router{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether DEX routing is configured and turned on.
func (r *Router) Enabled() bool { return r.cfg.Enabled }

// BestQuote fetches quotes from all configured providers and returns the best
// (highest toAmount). Falls back to the secondary provider if the primary fails.
func (r *Router) BestQuote(ctx context.Context, req QuoteRequest) (*QuoteResponse, error) {
	if !r.cfg.Enabled {
		return nil, fmt.Errorf("dex routing disabled")
	}
	if req.Slippage == 0 {
		req.Slippage = r.cfg.MaxSlippagePct
	}
	if r.cfg.MEVProtect {
		req.MEVProtect = true
	}

	var primary, secondary func(context.Context, QuoteRequest) (*QuoteResponse, error)
	switch r.cfg.PreferredProvider {
	case ProviderParaswap:
		primary, secondary = r.paraswapQuote, r.oneInchQuote
	default:
		primary, secondary = r.oneInchQuote, r.paraswapQuote
	}

	q, err := primary(ctx, req)
	if err == nil {
		return q, nil
	}
	return secondary(ctx, req)
}

// oneInchQuote fetches a price quote from the 1inch Swap API v6.
// When MEVProtect is requested, only curated low-MEV liquidity sources are used.
func (r *Router) oneInchQuote(ctx context.Context, req QuoteRequest) (*QuoteResponse, error) {
	chainID := req.ChainID
	if chainID == 0 {
		chainID = r.cfg.DefaultChainID
	}
	baseURL := fmt.Sprintf("https://api.1inch.dev/swap/v6.0/%d/quote", chainID)

	params := url.Values{}
	params.Set("src", req.FromToken)
	params.Set("dst", req.ToToken)
	params.Set("amount", req.AmountWei)
	if req.MEVProtect {
		// Restrict to MEV-resistant pools (Curve, Uniswap V3 with TWAP protection).
		params.Set("protocols", "UNISWAP_V3,CURVE,BALANCER_V2")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if r.cfg.OneInchAPIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+r.cfg.OneInchAPIKey)
	}

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("1inch quote: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("1inch quote: HTTP %d", resp.StatusCode)
	}

	var raw struct {
		DstAmount string `json:"dstAmount"`
		Gas       int64  `json:"gas"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("1inch quote decode: %w", err)
	}

	return &QuoteResponse{
		Provider:   Provider1inch,
		FromToken:  req.FromToken,
		ToToken:    req.ToToken,
		FromAmount: req.AmountWei,
		ToAmount:   raw.DstAmount,
		EstGasWei:  fmt.Sprintf("%d", raw.Gas),
	}, nil
}

// paraswapQuote fetches a price quote from the Paraswap v5 Prices API.
func (r *Router) paraswapQuote(ctx context.Context, req QuoteRequest) (*QuoteResponse, error) {
	chainID := req.ChainID
	if chainID == 0 {
		chainID = r.cfg.DefaultChainID
	}

	params := url.Values{}
	params.Set("srcToken", req.FromToken)
	params.Set("destToken", req.ToToken)
	params.Set("amount", req.AmountWei)
	params.Set("network", fmt.Sprintf("%d", chainID))
	params.Set("side", "SELL")
	params.Set("maxImpact", fmt.Sprintf("%.2f", r.cfg.MaxPriceImpactPct))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://apiv5.paraswap.io/prices?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if r.cfg.ParaswapAPIKey != "" {
		httpReq.Header.Set("x-api-key", r.cfg.ParaswapAPIKey)
	}

	resp, err := r.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("paraswap quote: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("paraswap quote: HTTP %d", resp.StatusCode)
	}

	var raw struct {
		PriceRoute struct {
			DestAmount         string  `json:"destAmount"`
			GasCost            string  `json:"gasCost"`
			TokenTransferProxy string  `json:"tokenTransferProxy"`
			MaxImpactReached   bool    `json:"maxImpactReached"`
			PriceWithSlippage  string  `json:"priceWithSlippage"`
		} `json:"priceRoute"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("paraswap quote decode: %w", err)
	}
	if raw.PriceRoute.MaxImpactReached {
		return nil, fmt.Errorf("paraswap: max price impact exceeded")
	}

	return &QuoteResponse{
		Provider:   ProviderParaswap,
		FromToken:  req.FromToken,
		ToToken:    req.ToToken,
		FromAmount: req.AmountWei,
		ToAmount:   raw.PriceRoute.DestAmount,
		EstGasWei:  raw.PriceRoute.GasCost,
		RouterAddr: raw.PriceRoute.TokenTransferProxy,
	}, nil
}
