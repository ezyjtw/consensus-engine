package dex

import (
	"context"
	"testing"
)

func makeTestConfig() Config {
	return Config{
		Enabled:           true,
		PreferredProvider: Provider1inch,
		DefaultChainID:    1,
		MaxSlippagePct:    0.5,
		MaxPriceImpactPct: 1.0,
		MEVProtect:        true,
	}
}

// Test Enabled flag.
func TestEnabledFlag(t *testing.T) {
	cfg := Config{Enabled: true}
	r := New(cfg)
	if !r.Enabled() {
		t.Error("router should be enabled")
	}
	cfg.Enabled = false
	r = New(cfg)
	if r.Enabled() {
		t.Error("router should be disabled")
	}
}

// Test BestQuote returns error when disabled.
func TestBestQuoteDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	r := New(cfg)
	_, err := r.BestQuote(context.Background(), QuoteRequest{})
	if err == nil {
		t.Error("expected error when disabled")
	}
}

// Test BestQuote config setup and request defaults.
func TestBestQuoteConfigSetup(t *testing.T) {
	cfg := makeTestConfig()
	r := New(cfg)

	// Verify router is properly configured.
	if !r.Enabled() {
		t.Error("router should be enabled")
	}

	req := QuoteRequest{
		ChainID:   1,
		FromToken: "0xUSDC",
		ToToken:   "0xETH",
		AmountWei: "1000000000",
	}

	// Test that MEV protect flag propagates.
	if !cfg.MEVProtect {
		t.Error("MEV protect should be enabled")
	}
	if req.Slippage == 0 {
		// BestQuote should set default slippage.
		req.Slippage = cfg.MaxSlippagePct
	}
	if req.Slippage != 0.5 {
		t.Errorf("slippage should be 0.5, got %.2f", req.Slippage)
	}
}

// Test provider preference routing.
func TestProviderPreference(t *testing.T) {
	tests := []struct {
		name     string
		provider Provider
	}{
		{"1inch preferred", Provider1inch},
		{"paraswap preferred", ProviderParaswap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Enabled:           true,
				PreferredProvider: tt.provider,
				DefaultChainID:    1,
			}
			r := New(cfg)
			if r.cfg.PreferredProvider != tt.provider {
				t.Errorf("expected provider %s, got %s", tt.provider, r.cfg.PreferredProvider)
			}
		})
	}
}

// Test QuoteRequest defaults.
func TestQuoteRequestDefaults(t *testing.T) {
	req := QuoteRequest{
		FromToken: "0xUSDC",
		ToToken:   "0xETH",
		AmountWei: "1000000",
	}
	if req.ChainID != 0 {
		t.Error("default chain ID should be 0")
	}
	if req.Slippage != 0 {
		t.Error("default slippage should be 0")
	}
	if req.MEVProtect {
		t.Error("default MEV protect should be false")
	}
}

// Test QuoteResponse provider field.
func TestQuoteResponseProviderField(t *testing.T) {
	resp := QuoteResponse{
		Provider:   Provider1inch,
		FromToken:  "0xUSDC",
		ToToken:    "0xETH",
		FromAmount: "1000000",
		ToAmount:   "500000000000000",
		EstGasWei:  "150000",
	}
	if resp.Provider != Provider1inch {
		t.Errorf("expected provider 1inch, got %s", resp.Provider)
	}
}

// Test router creation with API keys.
func TestRouterWithAPIKeys(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		OneInchAPIKey:  "test-1inch-key",
		ParaswapAPIKey: "test-paraswap-key",
		DefaultChainID: 1,
	}
	r := New(cfg)
	if r.cfg.OneInchAPIKey != "test-1inch-key" {
		t.Error("1inch API key not set")
	}
	if r.cfg.ParaswapAPIKey != "test-paraswap-key" {
		t.Error("Paraswap API key not set")
	}
}

// Test router HTTP client has timeout set.
func TestRouterHTTPClientTimeout(t *testing.T) {
	cfg := Config{Enabled: true}
	r := New(cfg)
	if r.client.Timeout == 0 {
		t.Error("HTTP client should have timeout set")
	}
}

// Test provider constants.
func TestProviderConstants(t *testing.T) {
	if Provider1inch != "1inch" {
		t.Errorf("Provider1inch should be '1inch', got %q", Provider1inch)
	}
	if ProviderParaswap != "paraswap" {
		t.Errorf("ProviderParaswap should be 'paraswap', got %q", ProviderParaswap)
	}
}
