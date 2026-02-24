package execution

import (
	"testing"
)

func TestMapSymbol(t *testing.T) {
	tests := []struct {
		venue, symbol, expected string
	}{
		{"binance", "BTC-PERP", "BTCUSDT"},
		{"okx", "BTC-PERP", "BTC-USDT-SWAP"},
		{"bybit", "ETH-PERP", "ETHUSDT"},
		{"deribit", "BTC-PERP", "BTC-PERPETUAL"},
		{"unknown", "BTC-PERP", "BTC-PERP"},
		{"binance", "BTC", "BTCUSDT"},    // no -PERP suffix
	}

	for _, tt := range tests {
		got := mapSymbol(tt.venue, tt.symbol)
		if got != tt.expected {
			t.Errorf("mapSymbol(%q, %q) = %q, want %q",
				tt.venue, tt.symbol, got, tt.expected)
		}
	}
}
