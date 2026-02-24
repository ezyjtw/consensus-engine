// Package bridge provides cross-chain bridge monitoring, challenge window
// tracking, and transfer safety verification.
package bridge

import (
	"sort"
	"sync"
	"time"
)

// BridgeType identifies the bridge protocol type.
type BridgeType string

const (
	Optimistic BridgeType = "OPTIMISTIC" // 7-day challenge window
	ZKProof    BridgeType = "ZK_PROOF"   // instant finality after proof
	Canonical  BridgeType = "CANONICAL"  // native bridge (e.g., L1→L2 deposit)
	ThirdParty BridgeType = "THIRD_PARTY" // external bridge (Across, Stargate)
)

// TransferStatus tracks bridge transfer state.
type TransferStatus string

const (
	StatusPending     TransferStatus = "PENDING"
	StatusInChallenge TransferStatus = "IN_CHALLENGE"
	StatusProving     TransferStatus = "PROVING"
	StatusFinalized   TransferStatus = "FINALIZED"
	StatusFailed      TransferStatus = "FAILED"
	StatusCancelled   TransferStatus = "CANCELLED"
)

// BridgeTransfer represents a cross-chain transfer in flight.
type BridgeTransfer struct {
	ID              string         `json:"id"`
	Bridge          string         `json:"bridge"`
	BridgeType      BridgeType     `json:"bridge_type"`
	SourceChain     uint64         `json:"source_chain"`
	DestChain       uint64         `json:"dest_chain"`
	Token           string         `json:"token"`
	AmountUSD       float64        `json:"amount_usd"`
	Status          TransferStatus `json:"status"`
	InitiatedMs     int64          `json:"initiated_ms"`
	ChallengeEndMs  int64          `json:"challenge_end_ms,omitempty"` // optimistic bridges
	EstFinalizeMs   int64          `json:"est_finalize_ms"`
	FinalizedMs     int64          `json:"finalized_ms,omitempty"`
	SourceTxHash    string         `json:"source_tx_hash"`
	DestTxHash      string         `json:"dest_tx_hash,omitempty"`
}

// BridgeConfig configures a bridge integration.
type BridgeConfig struct {
	Name            string     `yaml:"name"`
	Type            BridgeType `yaml:"type"`
	ChallengeMs     int64      `yaml:"challenge_ms"` // challenge window duration
	AvgFinalizeMs   int64      `yaml:"avg_finalize_ms"`
	MaxAmountUSD    float64    `yaml:"max_amount_usd"`
	FeeRateBps      float64    `yaml:"fee_rate_bps"`
	SupportedChains []uint64   `yaml:"supported_chains"`
}

// Monitor tracks bridge transfers and challenge windows.
type Monitor struct {
	mu        sync.RWMutex
	transfers map[string]*BridgeTransfer // id → transfer
	configs   map[string]*BridgeConfig   // bridge name → config
}

// MonitorStats tracks bridge monitoring statistics.
type MonitorStats struct {
	ActiveTransfers   int     `json:"active_transfers"`
	InChallenge       int     `json:"in_challenge"`
	TotalVolUSD       float64 `json:"total_vol_usd"`
	AvgFinalizeMs     int64   `json:"avg_finalize_ms"`
	FailedTransfers   int     `json:"failed_transfers"`
	PendingAmountUSD  float64 `json:"pending_amount_usd"`
}

// ChallengeAlert is emitted for challenge window events.
type ChallengeAlert struct {
	TransferID    string `json:"transfer_id"`
	Bridge        string `json:"bridge"`
	Type          string `json:"type"` // WINDOW_OPEN, WINDOW_CLOSING, WINDOW_EXPIRED
	RemainingMs   int64  `json:"remaining_ms"`
	AmountUSD     float64 `json:"amount_usd"`
	TsMs          int64  `json:"ts_ms"`
}

// NewMonitor creates a bridge monitor.
func NewMonitor() *Monitor {
	return &Monitor{
		transfers: make(map[string]*BridgeTransfer),
		configs:   make(map[string]*BridgeConfig),
	}
}

// SetBridgeConfig registers a bridge configuration.
func (m *Monitor) SetBridgeConfig(cfg BridgeConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.configs[cfg.Name] = &cfg
}

// TrackTransfer starts monitoring a bridge transfer.
func (m *Monitor) TrackTransfer(t BridgeTransfer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if t.Status == "" {
		t.Status = StatusPending
	}
	if t.InitiatedMs == 0 {
		t.InitiatedMs = time.Now().UnixMilli()
	}

	// Set challenge window based on bridge config
	if cfg, ok := m.configs[t.Bridge]; ok {
		if cfg.Type == Optimistic && t.ChallengeEndMs == 0 {
			t.ChallengeEndMs = t.InitiatedMs + cfg.ChallengeMs
			t.Status = StatusInChallenge
		}
		if t.EstFinalizeMs == 0 {
			t.EstFinalizeMs = t.InitiatedMs + cfg.AvgFinalizeMs
		}
	}

	m.transfers[t.ID] = &t
}

// UpdateStatus updates a transfer's status.
func (m *Monitor) UpdateStatus(id string, status TransferStatus, destTxHash string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.transfers[id]; ok {
		t.Status = status
		if destTxHash != "" {
			t.DestTxHash = destTxHash
		}
		if status == StatusFinalized {
			t.FinalizedMs = time.Now().UnixMilli()
		}
	}
}

// ActiveTransfers returns all non-finalized transfers.
func (m *Monitor) ActiveTransfers() []*BridgeTransfer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var active []*BridgeTransfer
	for _, t := range m.transfers {
		if t.Status != StatusFinalized && t.Status != StatusFailed && t.Status != StatusCancelled {
			active = append(active, t)
		}
	}

	sort.Slice(active, func(i, j int) bool {
		return active[i].InitiatedMs > active[j].InitiatedMs
	})
	return active
}

// CheckChallengeWindows evaluates all optimistic bridge transfers for challenge alerts.
func (m *Monitor) CheckChallengeWindows() []ChallengeAlert {
	m.mu.RLock()
	defer m.mu.RUnlock()

	now := time.Now().UnixMilli()
	var alerts []ChallengeAlert

	for _, t := range m.transfers {
		if t.Status != StatusInChallenge || t.ChallengeEndMs == 0 {
			continue
		}

		remaining := t.ChallengeEndMs - now

		if remaining <= 0 {
			alerts = append(alerts, ChallengeAlert{
				TransferID:  t.ID,
				Bridge:      t.Bridge,
				Type:        "WINDOW_EXPIRED",
				RemainingMs: 0,
				AmountUSD:   t.AmountUSD,
				TsMs:        now,
			})
		} else if remaining < 3600000 { // less than 1 hour
			alerts = append(alerts, ChallengeAlert{
				TransferID:  t.ID,
				Bridge:      t.Bridge,
				Type:        "WINDOW_CLOSING",
				RemainingMs: remaining,
				AmountUSD:   t.AmountUSD,
				TsMs:        now,
			})
		}
	}

	return alerts
}

// Stats returns current monitoring statistics.
func (m *Monitor) Stats() MonitorStats {
	m.mu.RLock()
	defer m.mu.RUnlock()

	stats := MonitorStats{}
	var totalFinalizeMs int64
	var finalizedCount int

	for _, t := range m.transfers {
		switch t.Status {
		case StatusPending, StatusInChallenge, StatusProving:
			stats.ActiveTransfers++
			stats.PendingAmountUSD += t.AmountUSD
		case StatusFailed:
			stats.FailedTransfers++
		}
		if t.Status == StatusInChallenge {
			stats.InChallenge++
		}
		stats.TotalVolUSD += t.AmountUSD

		if t.FinalizedMs > 0 {
			totalFinalizeMs += t.FinalizedMs - t.InitiatedMs
			finalizedCount++
		}
	}

	if finalizedCount > 0 {
		stats.AvgFinalizeMs = totalFinalizeMs / int64(finalizedCount)
	}

	return stats
}

// PurgeFinalized removes finalized transfers older than the cutoff.
func (m *Monitor) PurgeFinalized(cutoffMs int64) int {
	m.mu.Lock()
	defer m.mu.Unlock()

	threshold := time.Now().UnixMilli() - cutoffMs
	removed := 0
	for id, t := range m.transfers {
		if t.FinalizedMs > 0 && t.FinalizedMs < threshold {
			delete(m.transfers, id)
			removed++
		}
	}
	return removed
}

// EstimateTransferTime returns estimated finalization time for a bridge.
func (m *Monitor) EstimateTransferTime(bridge string) int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if cfg, ok := m.configs[bridge]; ok {
		return cfg.AvgFinalizeMs
	}
	return 0
}
