package bridge

import (
	"testing"
	"time"
)

func TestTrackAndActiveTransfers(t *testing.T) {
	m := NewMonitor()

	m.SetBridgeConfig(BridgeConfig{
		Name:          "optimism",
		Type:          Optimistic,
		ChallengeMs:   7 * 24 * 3600 * 1000, // 7 days
		AvgFinalizeMs: 7*24*3600*1000 + 600000,
	})

	m.TrackTransfer(BridgeTransfer{
		ID:          "tx-001",
		Bridge:      "optimism",
		BridgeType:  Optimistic,
		SourceChain: 1,
		DestChain:   10,
		Token:       "USDC",
		AmountUSD:   50000,
	})

	active := m.ActiveTransfers()
	if len(active) != 1 {
		t.Fatalf("expected 1 active transfer, got %d", len(active))
	}

	tx := active[0]
	if tx.Status != StatusInChallenge {
		t.Errorf("expected IN_CHALLENGE status, got %s", tx.Status)
	}
	if tx.ChallengeEndMs == 0 {
		t.Error("expected challenge end to be set")
	}
}

func TestUpdateStatusFinalized(t *testing.T) {
	m := NewMonitor()

	m.TrackTransfer(BridgeTransfer{
		ID:          "tx-002",
		Bridge:      "stargate",
		BridgeType:  ThirdParty,
		Token:       "ETH",
		AmountUSD:   10000,
	})

	m.UpdateStatus("tx-002", StatusFinalized, "0xDestHash")

	active := m.ActiveTransfers()
	if len(active) != 0 {
		t.Errorf("expected 0 active after finalize, got %d", len(active))
	}

	stats := m.Stats()
	if stats.ActiveTransfers != 0 {
		t.Errorf("expected 0 active in stats, got %d", stats.ActiveTransfers)
	}
}

func TestChallengeWindowAlerts(t *testing.T) {
	m := NewMonitor()

	now := time.Now().UnixMilli()

	// Transfer with challenge window expiring soon (30 minutes left)
	m.TrackTransfer(BridgeTransfer{
		ID:             "tx-closing",
		Bridge:         "optimism",
		BridgeType:     Optimistic,
		Status:         StatusInChallenge,
		AmountUSD:      100000,
		InitiatedMs:    now - 7*24*3600*1000,
		ChallengeEndMs: now + 1800000, // 30 min from now
	})

	// Transfer with challenge window already expired
	m.TrackTransfer(BridgeTransfer{
		ID:             "tx-expired",
		Bridge:         "arbitrum",
		BridgeType:     Optimistic,
		Status:         StatusInChallenge,
		AmountUSD:      50000,
		InitiatedMs:    now - 8*24*3600*1000,
		ChallengeEndMs: now - 3600000, // 1 hour ago
	})

	alerts := m.CheckChallengeWindows()
	if len(alerts) != 2 {
		t.Fatalf("expected 2 alerts, got %d", len(alerts))
	}

	closingFound, expiredFound := false, false
	for _, a := range alerts {
		switch a.Type {
		case "WINDOW_CLOSING":
			closingFound = true
			if a.TransferID != "tx-closing" {
				t.Errorf("expected tx-closing, got %s", a.TransferID)
			}
		case "WINDOW_EXPIRED":
			expiredFound = true
			if a.TransferID != "tx-expired" {
				t.Errorf("expected tx-expired, got %s", a.TransferID)
			}
		}
	}
	if !closingFound {
		t.Error("missing WINDOW_CLOSING alert")
	}
	if !expiredFound {
		t.Error("missing WINDOW_EXPIRED alert")
	}
}

func TestStatsAndPurge(t *testing.T) {
	m := NewMonitor()

	m.TrackTransfer(BridgeTransfer{
		ID:        "tx-1",
		Bridge:    "across",
		AmountUSD: 20000,
	})
	m.TrackTransfer(BridgeTransfer{
		ID:        "tx-2",
		Bridge:    "stargate",
		AmountUSD: 30000,
	})

	m.UpdateStatus("tx-1", StatusFinalized, "0x1")

	stats := m.Stats()
	if stats.ActiveTransfers != 1 {
		t.Errorf("expected 1 active, got %d", stats.ActiveTransfers)
	}
	if stats.TotalVolUSD != 50000 {
		t.Errorf("expected 50000 total vol, got %f", stats.TotalVolUSD)
	}

	// Purge finalized: FinalizedMs was just set to ~now, so we need a cutoff
	// that makes threshold = now + buffer (i.e. finalized time is before threshold).
	// PurgeFinalized checks FinalizedMs < threshold where threshold = now - cutoff.
	// Using cutoff = -1000 makes threshold = now + 1s which is after FinalizedMs.
	removed := m.PurgeFinalized(-1000)
	if removed != 1 {
		t.Errorf("expected 1 purged, got %d", removed)
	}
}
