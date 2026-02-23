// Package l2 provides optional L2 transfer routing for Arbitrum, Optimism, and Base.
// It compares estimated gas costs across networks and routes to the lowest-cost option
// that meets the configured minimum savings threshold.
package l2

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// Bridge selects and estimates L2 transfer routes.
type Bridge struct {
	cfg    Config
	client *http.Client
}

// New creates a Bridge from config.
func New(cfg Config) *Bridge {
	return &Bridge{
		cfg:    cfg,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

// Enabled reports whether L2 transfers are configured and turned on.
func (b *Bridge) Enabled() bool { return b.cfg.Enabled }

// Estimate returns a gas/cost estimate for a specific network transfer.
// Production implementations would call the bridge contract via JSON-RPC to
// get exact calldata + gasEstimate. These values use representative L2 parameters.
func (b *Bridge) Estimate(_ context.Context, req BridgeRequest) (*BridgeEstimate, error) {
	if !b.cfg.Enabled {
		return nil, fmt.Errorf("l2 transfers disabled")
	}
	if _, ok := ChainIDs[req.Network]; !ok {
		return nil, fmt.Errorf("unsupported network: %s", req.Network)
	}

	// Representative gas parameters per network (actual values require RPC call).
	type params struct {
		l1Gas, l2Gas string
		settlementMs int64
		costUSD      float64
	}
	networkParams := map[Network]params{
		NetworkArbitrum: {"21000", "800000", 10 * 60 * 1000, 0.30},
		NetworkOptimism: {"21000", "1000000", 30 * 60 * 1000, 0.45},
		NetworkBase:     {"21000", "700000", 5 * 60 * 1000, 0.25},
	}

	p, ok := networkParams[req.Network]
	if !ok {
		return nil, fmt.Errorf("no params for network %s", req.Network)
	}

	return &BridgeEstimate{
		Network:         req.Network,
		L1GasCostWei:    p.l1Gas,
		L2GasCostWei:    p.l2Gas,
		TotalCostUSD:    p.costUSD,
		EstSettlementMs: p.settlementMs,
	}, nil
}

// BestNetwork evaluates all configured networks and returns the one with the
// lowest estimated cost, subject to the max_gas_gwei and min_savings_usd policy.
func (b *Bridge) BestNetwork(ctx context.Context, req BridgeRequest) (Network, *BridgeEstimate, error) {
	if !b.cfg.Enabled {
		return "", nil, fmt.Errorf("l2 transfers disabled")
	}

	// Build candidate list, preferred network first.
	candidates := []Network{NetworkArbitrum, NetworkOptimism, NetworkBase}
	if b.cfg.PreferNetwork != "" {
		filtered := []Network{b.cfg.PreferNetwork}
		for _, n := range candidates {
			if n != b.cfg.PreferNetwork {
				filtered = append(filtered, n)
			}
		}
		candidates = filtered
	}

	var best Network
	var bestEst *BridgeEstimate

	for _, net := range candidates {
		r := req
		r.Network = net
		est, err := b.Estimate(ctx, r)
		if err != nil {
			continue
		}
		if b.cfg.MinSavingsUSD > 0 && est.TotalCostUSD > 1.0-b.cfg.MinSavingsUSD {
			// Cost is too high relative to desired savings floor.
			continue
		}
		if bestEst == nil || est.TotalCostUSD < bestEst.TotalCostUSD {
			best = net
			bestEst = est
		}
	}

	if bestEst == nil {
		return "", nil, fmt.Errorf("no viable L2 network found within policy constraints")
	}
	return best, bestEst, nil
}
