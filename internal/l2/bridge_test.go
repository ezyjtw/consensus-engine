package l2

import (
	"context"
	"testing"
)

func testConfig() Config {
	return Config{
		Enabled:       true,
		PreferNetwork: NetworkArbitrum,
		MinSavingsUSD: 0.50,
		MaxGasGwei:    100,
	}
}

// Test Estimate returns valid cost for each supported network.
func TestEstimateAllNetworks(t *testing.T) {
	bridge := New(testConfig())
	ctx := context.Background()

	networks := []Network{NetworkArbitrum, NetworkOptimism, NetworkBase}
	for _, net := range networks {
		est, err := bridge.Estimate(ctx, BridgeRequest{Network: net})
		if err != nil {
			t.Errorf("estimate for %s: %v", net, err)
			continue
		}
		if est.TotalCostUSD <= 0 {
			t.Errorf("%s: expected positive cost, got %.4f", net, est.TotalCostUSD)
		}
		if est.EstSettlementMs <= 0 {
			t.Errorf("%s: expected positive settlement time, got %d", net, est.EstSettlementMs)
		}
		if est.Network != net {
			t.Errorf("expected network %s, got %s", net, est.Network)
		}
	}
}

// Test Estimate rejects unsupported network.
func TestEstimateUnsupportedNetwork(t *testing.T) {
	bridge := New(testConfig())
	_, err := bridge.Estimate(context.Background(), BridgeRequest{Network: "polygon"})
	if err == nil {
		t.Error("expected error for unsupported network")
	}
}

// Test Estimate returns error when disabled.
func TestEstimateDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	bridge := New(cfg)
	_, err := bridge.Estimate(context.Background(), BridgeRequest{Network: NetworkArbitrum})
	if err == nil {
		t.Error("expected error when disabled")
	}
}

// Test BestNetwork returns the cheapest option.
func TestBestNetworkSelectsCheapest(t *testing.T) {
	cfg := testConfig()
	cfg.PreferNetwork = "" // no preference — pick cheapest
	bridge := New(cfg)

	net, est, err := bridge.BestNetwork(context.Background(), BridgeRequest{})
	if err != nil {
		t.Fatalf("BestNetwork: %v", err)
	}

	// Base ($0.25) is cheapest in the representative params.
	if net != NetworkBase {
		t.Errorf("expected Base (cheapest), got %s (cost=%.2f)", net, est.TotalCostUSD)
	}
	if est.TotalCostUSD != 0.25 {
		t.Errorf("Base cost should be 0.25, got %.2f", est.TotalCostUSD)
	}
}

// Test BestNetwork prefers configured network when it's the cheapest.
func TestBestNetworkRespectsPreference(t *testing.T) {
	cfg := testConfig()
	cfg.PreferNetwork = NetworkBase
	bridge := New(cfg)

	net, _, err := bridge.BestNetwork(context.Background(), BridgeRequest{})
	if err != nil {
		t.Fatalf("BestNetwork: %v", err)
	}
	if net != NetworkBase {
		t.Errorf("expected preferred network Base, got %s", net)
	}
}

// Test BestNetwork returns error when disabled.
func TestBestNetworkDisabled(t *testing.T) {
	cfg := testConfig()
	cfg.Enabled = false
	bridge := New(cfg)
	_, _, err := bridge.BestNetwork(context.Background(), BridgeRequest{})
	if err == nil {
		t.Error("expected error when disabled")
	}
}

// Test Enabled flag.
func TestEnabledFlag(t *testing.T) {
	cfg := testConfig()
	bridge := New(cfg)
	if !bridge.Enabled() {
		t.Error("bridge should be enabled")
	}

	cfg.Enabled = false
	bridge = New(cfg)
	if bridge.Enabled() {
		t.Error("bridge should be disabled")
	}
}

// Test settlement times vary by network.
func TestSettlementTimesVary(t *testing.T) {
	bridge := New(testConfig())
	ctx := context.Background()

	arbEst, _ := bridge.Estimate(ctx, BridgeRequest{Network: NetworkArbitrum})
	optEst, _ := bridge.Estimate(ctx, BridgeRequest{Network: NetworkOptimism})

	if arbEst.EstSettlementMs == optEst.EstSettlementMs {
		t.Error("settlement times should differ between Arbitrum and Optimism")
	}
	// Arbitrum is faster (10min vs 30min).
	if arbEst.EstSettlementMs >= optEst.EstSettlementMs {
		t.Errorf("Arbitrum (%dms) should settle faster than Optimism (%dms)",
			arbEst.EstSettlementMs, optEst.EstSettlementMs)
	}
}

// Test ChainIDs map completeness.
func TestChainIDsComplete(t *testing.T) {
	expected := map[Network]int{
		NetworkArbitrum: 42161,
		NetworkOptimism: 10,
		NetworkBase:     8453,
	}
	for net, wantID := range expected {
		gotID, ok := ChainIDs[net]
		if !ok {
			t.Errorf("missing chain ID for %s", net)
			continue
		}
		if gotID != wantID {
			t.Errorf("%s chain ID: want %d, got %d", net, wantID, gotID)
		}
	}
}
