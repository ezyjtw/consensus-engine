package store

import (
	"testing"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// spec §10 test 3: stale feed excluded from live quotes
func TestStaleFilter(t *testing.T) {
	staleMs := int64(750)
	s := New(staleMs)

	nowMs := time.Now().UnixMilli()

	fresh := consensus.Quote{
		TenantID: "t1",
		Symbol:   "BTC-PERP",
		Venue:    "binance",
		FeedHealth: consensus.FeedHealth{
			WsConnected: true,
			LastMsgTsMs: nowMs - 100, // 100ms ago — fresh
		},
	}
	stale := consensus.Quote{
		TenantID: "t1",
		Symbol:   "BTC-PERP",
		Venue:    "okx",
		FeedHealth: consensus.FeedHealth{
			WsConnected: false,
			LastMsgTsMs: nowMs - 2000, // 2s ago — stale
		},
	}

	s.UpsertQuote(fresh)
	s.UpsertQuote(stale)

	live := s.LiveQuotes("t1", "BTC-PERP")

	if _, ok := live["binance"]; !ok {
		t.Error("fresh quote (binance) must appear in LiveQuotes")
	}
	if _, ok := live["okx"]; ok {
		t.Error("stale quote (okx) must be excluded from LiveQuotes")
	}
}

// quotes for a different tenant/symbol must not appear
func TestIsolation(t *testing.T) {
	s := New(5000)
	nowMs := time.Now().UnixMilli()

	q := consensus.Quote{
		TenantID: "tenant-A",
		Symbol:   "ETH-PERP",
		Venue:    "binance",
		FeedHealth: consensus.FeedHealth{LastMsgTsMs: nowMs},
	}
	s.UpsertQuote(q)

	if live := s.LiveQuotes("tenant-B", "ETH-PERP"); len(live) != 0 {
		t.Errorf("cross-tenant isolation failed: got %d quotes for tenant-B", len(live))
	}
	if live := s.LiveQuotes("tenant-A", "BTC-PERP"); len(live) != 0 {
		t.Errorf("cross-symbol isolation failed: got %d quotes for BTC-PERP", len(live))
	}
}

// GetStatus returns StateOK default for unknown venue
func TestGetStatusDefault(t *testing.T) {
	s := New(5000)
	st := s.GetStatus("t1", "BTC-PERP", "unknown")
	if st.State != consensus.StateOK {
		t.Errorf("default status should be StateOK, got %s", st.State)
	}
}
