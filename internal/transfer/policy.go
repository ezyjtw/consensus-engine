package transfer

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Engine enforces transfer policy rules in order:
//  1. Config tamper check
//  2. Allowlist (address must be pre-approved)
//  3. Per-transfer notional cap
//  4. Daily notional velocity
//  5. Transfers-per-day velocity
//  6. New address cooloff (address must be on list for NewAddressCooloffH hours)
//  7. Manual approval gate (all transfers held if ManualApprovalRequired)
type Engine struct {
	mu         sync.Mutex
	cfg        *Config
	configPath string

	// Rolling transfer history for velocity checks.
	history []transferRecord
}

type transferRecord struct {
	toAddress string
	amountUSD float64
	ts        time.Time
}

// New creates a policy Engine.
func New(cfg *Config, configPath string) *Engine {
	return &Engine{cfg: cfg, configPath: configPath}
}

// Check evaluates a transfer request against all policy rules.
// It is safe to call concurrently.
func (e *Engine) Check(req Request) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UTC()
	base := Decision{
		RequestID: req.ID,
		CheckedAt: now,
	}

	// ── 1. Tamper detection ───────────────────────────────────────────────
	if err := e.cfg.TamperCheck(e.configPath); err != nil {
		log.Printf("transfer-policy: TAMPER ALERT: %v", err)
		return deny(base, "CONFIG_TAMPERED",
			fmt.Sprintf("transfer policy file has been modified since load: %v", err))
	}

	// ── 2. Allowlist check ────────────────────────────────────────────────
	entry, ok := e.allowlistEntry(req.ToAddress, req.Asset)
	if !ok {
		return deny(base, "ADDRESS_NOT_ALLOWLISTED",
			fmt.Sprintf("destination %s is not on the pre-approved allowlist", req.ToAddress))
	}

	// ── 3. Per-transfer cap (allowlist entry overrides global if set) ─────
	maxPerTx := e.cfg.MaxPerTransferUSD
	if entry.MaxPerTxUSD > 0 && entry.MaxPerTxUSD < maxPerTx {
		maxPerTx = entry.MaxPerTxUSD
	}
	if maxPerTx > 0 && req.AmountUSD > maxPerTx {
		return deny(base, "EXCEEDS_PER_TX_CAP",
			fmt.Sprintf("amount $%.2f exceeds per-transfer cap $%.2f", req.AmountUSD, maxPerTx))
	}

	// ── 4. New address cooloff ────────────────────────────────────────────
	if e.cfg.NewAddressCooloffH > 0 && entry.AddedAt != "" {
		addedAt, err := time.Parse("2006-01-02", entry.AddedAt)
		if err == nil {
			cooloffExpiry := addedAt.Add(time.Duration(e.cfg.NewAddressCooloffH) * time.Hour)
			if now.Before(cooloffExpiry) {
				return deny(base, "NEW_ADDRESS_COOLOFF",
					fmt.Sprintf("address %s is in new-address cooloff until %s",
						req.ToAddress, cooloffExpiry.Format(time.RFC3339)))
			}
		}
	}

	// ── 5. Velocity checks (rolling 24h) ──────────────────────────────────
	e.pruneHistory(now)

	if e.cfg.MaxTransfersPerDay > 0 && len(e.history) >= e.cfg.MaxTransfersPerDay {
		return deny(base, "DAILY_TRANSFER_COUNT_EXCEEDED",
			fmt.Sprintf("daily transfer count %d exceeds limit %d",
				len(e.history), e.cfg.MaxTransfersPerDay))
	}

	dailyTotal := e.dailyTotal()
	if e.cfg.MaxDailyUSD > 0 && dailyTotal+req.AmountUSD > e.cfg.MaxDailyUSD {
		return deny(base, "DAILY_NOTIONAL_EXCEEDED",
			fmt.Sprintf("daily total $%.2f + $%.2f would exceed limit $%.2f",
				dailyTotal, req.AmountUSD, e.cfg.MaxDailyUSD))
	}

	// ── 6. Manual approval gate ───────────────────────────────────────────
	if e.cfg.ManualApprovalRequired {
		base.Status = StatusPending
		base.RequiresApproval = true
		base.Reason = "manual approval required by policy; all controls passed"
		log.Printf("transfer-policy: PENDING_APPROVAL id=%s amount=$%.2f to=%s",
			req.ID, req.AmountUSD, req.ToAddress)
		return base
	}

	// All checks passed — record and approve.
	e.history = append(e.history, transferRecord{
		toAddress: req.ToAddress,
		amountUSD: req.AmountUSD,
		ts:        now,
	})
	base.Status = StatusApproved
	base.Reason = "all policy checks passed"
	log.Printf("transfer-policy: APPROVED id=%s amount=$%.2f to=%s",
		req.ID, req.AmountUSD, req.ToAddress)
	return base
}

// allowlistEntry returns the matching entry for an address+asset combo.
// Asset "*" in an entry matches any asset.
func (e *Engine) allowlistEntry(addr, asset string) (AllowlistEntry, bool) {
	for _, entry := range e.cfg.Allowlist {
		if strings.EqualFold(entry.Address, addr) &&
			(entry.Asset == "*" || strings.EqualFold(entry.Asset, asset)) {
			return entry, true
		}
	}
	return AllowlistEntry{}, false
}

func (e *Engine) pruneHistory(now time.Time) {
	cutoff := now.Add(-24 * time.Hour)
	i := 0
	for i < len(e.history) && e.history[i].ts.Before(cutoff) {
		i++
	}
	e.history = e.history[i:]
}

func (e *Engine) dailyTotal() float64 {
	var total float64
	for _, r := range e.history {
		total += r.amountUSD
	}
	return total
}

func deny(base Decision, code, reason string) Decision {
	base.Status = StatusDenied
	base.DenialCode = code
	base.Reason = reason
	log.Printf("transfer-policy: DENIED code=%s reason=%s", code, reason)
	return base
}
