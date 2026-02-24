package triangular

import (
	"testing"
	"time"
)

func TestTriangularForwardOpportunity(t *testing.T) {
	cfg := Config{
		Triangles: []Triangle{
			{Name: "BTC_ETH_USDT", PairAB: "BTC/USDT", PairBC: "ETH/BTC", PairCA: "ETH/USDT", Venue: "binance"},
		},
		MinEdgeBps:     1.0,
		FeeBpsTaker:    1.0, // 1 bps per leg
		MaxNotionalUSD: 10000,
		CooldownMs:     100,
		StaleMs:        5000,
	}

	e := NewEngine(cfg)
	now := time.Now().UnixMilli()

	// Create a forward arbitrage: buy BTC cheap, convert to ETH, sell ETH for more USDT
	e.UpdateQuote(PairQuote{Symbol: "BTC/USDT", Venue: "binance", Bid: 100000, Ask: 99990, BidQty: 1, AskQty: 1, TsMs: now})
	e.UpdateQuote(PairQuote{Symbol: "ETH/BTC", Venue: "binance", Bid: 0.035, Ask: 0.03499, BidQty: 10, AskQty: 10, TsMs: now})
	e.UpdateQuote(PairQuote{Symbol: "ETH/USDT", Venue: "binance", Bid: 3510, Ask: 3500, BidQty: 10, AskQty: 10, TsMs: now})

	// Forward rate: (1/99990) * (1/0.03499) * 3510
	// = 0.00001001 * 28.579 * 3510 ≈ 1.00396 → ~39.6 bps gross - 3 bps fees = ~36.6 bps net
	opps := e.Scan()
	found := false
	for _, o := range opps {
		if o.Direction == "FORWARD" {
			found = true
			if o.NetEdgeBps < 1.0 {
				t.Errorf("expected positive net edge, got %.2f bps", o.NetEdgeBps)
			}
			if o.NotionalUSD <= 0 {
				t.Error("expected positive notional")
			}
		}
	}
	if !found {
		t.Error("expected FORWARD triangular opportunity")
	}
}

func TestTriangularNoOpportunity(t *testing.T) {
	cfg := Config{
		Triangles: []Triangle{
			{Name: "BTC_ETH_USDT", PairAB: "BTC/USDT", PairBC: "ETH/BTC", PairCA: "ETH/USDT", Venue: "binance"},
		},
		MinEdgeBps:     5.0,
		FeeBpsTaker:    2.0,
		MaxNotionalUSD: 10000,
		CooldownMs:     100,
		StaleMs:        5000,
	}

	e := NewEngine(cfg)
	now := time.Now().UnixMilli()

	// Perfectly balanced prices: no arb
	e.UpdateQuote(PairQuote{Symbol: "BTC/USDT", Venue: "binance", Bid: 100000, Ask: 100000, BidQty: 1, AskQty: 1, TsMs: now})
	e.UpdateQuote(PairQuote{Symbol: "ETH/BTC", Venue: "binance", Bid: 0.035, Ask: 0.035, BidQty: 10, AskQty: 10, TsMs: now})
	e.UpdateQuote(PairQuote{Symbol: "ETH/USDT", Venue: "binance", Bid: 3500, Ask: 3500, BidQty: 10, AskQty: 10, TsMs: now})

	opps := e.Scan()
	if len(opps) > 0 {
		t.Errorf("expected no opportunities with balanced prices, got %d", len(opps))
	}
}

func TestTriangularCooldown(t *testing.T) {
	cfg := Config{
		Triangles: []Triangle{
			{Name: "TEST", PairAB: "A/B", PairBC: "C/A", PairCA: "C/B", Venue: "test"},
		},
		MinEdgeBps:     0.1,
		FeeBpsTaker:    0.0,
		MaxNotionalUSD: 10000,
		CooldownMs:     60000,
		StaleMs:        5000,
	}

	e := NewEngine(cfg)
	now := time.Now().UnixMilli()

	e.UpdateQuote(PairQuote{Symbol: "A/B", Venue: "test", Bid: 1.001, Ask: 0.999, BidQty: 100, AskQty: 100, TsMs: now})
	e.UpdateQuote(PairQuote{Symbol: "C/A", Venue: "test", Bid: 1.001, Ask: 0.999, BidQty: 100, AskQty: 100, TsMs: now})
	e.UpdateQuote(PairQuote{Symbol: "C/B", Venue: "test", Bid: 1.001, Ask: 0.999, BidQty: 100, AskQty: 100, TsMs: now})

	opps1 := e.Scan()
	opps2 := e.Scan() // should be cooldown-blocked

	if len(opps1) == 0 {
		t.Error("expected opportunity on first scan")
	}
	if len(opps2) > 0 {
		t.Error("expected cooldown to block second scan")
	}
}
