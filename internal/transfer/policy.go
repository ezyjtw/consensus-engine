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
//  2. Region/jurisdiction constraints
//  3. Allowlist (address must be pre-approved)
//  4. Per-transfer notional cap
//  5. New address cooloff (address must be on list for NewAddressCooloffH hours)
//  6. Daily notional/count velocity
//  7. Dual approval gate (large transfers require 2 sign-offs)
//  8. Manual approval gate (all transfers held if ManualApprovalRequired)
type Engine struct {
	mu         sync.Mutex
	cfg        *Config
	configPath string

	// Rolling transfer history for velocity checks.
	history []transferRecord

	// Pending transfers awaiting approval(s).
	pending map[string]*PendingTransfer // requestID → pending
}

type transferRecord struct {
	toAddress string
	amountUSD float64
	ts        time.Time
}

// New creates a policy Engine.
func New(cfg *Config, configPath string) *Engine {
	return &Engine{
		cfg:        cfg,
		configPath: configPath,
		pending:    make(map[string]*PendingTransfer),
	}
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

	// ── 2. Region/jurisdiction constraints ────────────────────────────────
	if reason := e.checkRegionConstraints(req); reason != "" {
		return deny(base, "REGION_BLOCKED", reason)
	}

	// ── 3. Allowlist check ────────────────────────────────────────────────
	entry, ok := e.allowlistEntry(req.ToAddress, req.Asset)
	if !ok {
		return deny(base, "ADDRESS_NOT_ALLOWLISTED",
			fmt.Sprintf("destination %s is not on the pre-approved allowlist", req.ToAddress))
	}

	// ── 4. Per-transfer cap (allowlist entry overrides global if set) ─────
	maxPerTx := e.cfg.MaxPerTransferUSD
	if entry.MaxPerTxUSD > 0 && entry.MaxPerTxUSD < maxPerTx {
		maxPerTx = entry.MaxPerTxUSD
	}
	if maxPerTx > 0 && req.AmountUSD > maxPerTx {
		return deny(base, "EXCEEDS_PER_TX_CAP",
			fmt.Sprintf("amount $%.2f exceeds per-transfer cap $%.2f", req.AmountUSD, maxPerTx))
	}

	// ── 5. New address cooloff ────────────────────────────────────────────
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

	// ── 6. Velocity checks (rolling 24h) ──────────────────────────────────
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

	// ── 7. Dual approval gate (large transfers) ──────────────────────────
	if e.cfg.DualApprovalThresholdUSD > 0 && req.AmountUSD >= e.cfg.DualApprovalThresholdUSD {
		expiryH := e.cfg.ApprovalExpiryH
		if expiryH == 0 {
			expiryH = 24
		}
		base.Status = StatusPending
		base.RequiresApproval = true
		base.ApprovalsNeeded = 2
		base.ApprovalsHave = 0
		base.Reason = fmt.Sprintf("dual approval required: $%.2f >= threshold $%.2f",
			req.AmountUSD, e.cfg.DualApprovalThresholdUSD)

		e.pending[req.ID] = &PendingTransfer{
			Request:   req,
			Decision:  base,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(expiryH) * time.Hour),
		}

		log.Printf("transfer-policy: DUAL_APPROVAL id=%s amount=$%.2f to=%s (needs 2 sign-offs)",
			req.ID, req.AmountUSD, req.ToAddress)
		return base
	}

	// ── 8. Manual approval gate ───────────────────────────────────────────
	if e.cfg.ManualApprovalRequired {
		expiryH := e.cfg.ApprovalExpiryH
		if expiryH == 0 {
			expiryH = 24
		}
		base.Status = StatusPending
		base.RequiresApproval = true
		base.ApprovalsNeeded = 1
		base.ApprovalsHave = 0
		base.Reason = "manual approval required by policy; all controls passed"

		e.pending[req.ID] = &PendingTransfer{
			Request:   req,
			Decision:  base,
			CreatedAt: now,
			ExpiresAt: now.Add(time.Duration(expiryH) * time.Hour),
		}

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

// Approve adds an approval to a pending transfer. If sufficient approvals
// are collected, the transfer is approved and removed from pending.
// Returns the updated decision and true if the transfer is now fully approved.
func (e *Engine) Approve(requestID, approvedBy, comment string) (Decision, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UTC()

	pt, exists := e.pending[requestID]
	if !exists {
		return Decision{
			RequestID:  requestID,
			Status:     StatusDenied,
			DenialCode: "NOT_FOUND",
			Reason:     "no pending transfer with this ID",
			CheckedAt:  now,
		}, false
	}

	// Check expiry.
	if now.After(pt.ExpiresAt) {
		delete(e.pending, requestID)
		return Decision{
			RequestID:  requestID,
			Status:     StatusDenied,
			DenialCode: "EXPIRED",
			Reason:     fmt.Sprintf("approval expired at %s", pt.ExpiresAt.Format(time.RFC3339)),
			CheckedAt:  now,
		}, false
	}

	// Check for duplicate approver (same person can't approve twice).
	for _, a := range pt.Approvals {
		if a.ApprovedBy == approvedBy {
			return Decision{
				RequestID:  requestID,
				Status:     StatusPending,
				DenialCode: "DUPLICATE_APPROVER",
				Reason:     fmt.Sprintf("operator %q has already approved this transfer", approvedBy),
				CheckedAt:  now,
			}, false
		}
	}

	// Requester cannot self-approve.
	if pt.Request.RequestedBy == approvedBy {
		return Decision{
			RequestID:  requestID,
			Status:     StatusPending,
			DenialCode: "SELF_APPROVAL",
			Reason:     "the requestor cannot approve their own transfer",
			CheckedAt:  now,
		}, false
	}

	// Record approval.
	pt.Approvals = append(pt.Approvals, Approval{
		RequestID:  requestID,
		ApprovedBy: approvedBy,
		ApprovedAt: now,
		Comment:    comment,
	})

	pt.Decision.ApprovalsHave = len(pt.Approvals)

	log.Printf("transfer-policy: APPROVAL id=%s by=%s (%d/%d)",
		requestID, approvedBy, len(pt.Approvals), pt.Decision.ApprovalsNeeded)

	// Check if we have enough approvals.
	if len(pt.Approvals) >= pt.Decision.ApprovalsNeeded {
		// Transfer approved — record in history.
		e.history = append(e.history, transferRecord{
			toAddress: pt.Request.ToAddress,
			amountUSD: pt.Request.AmountUSD,
			ts:        now,
		})
		delete(e.pending, requestID)

		decision := Decision{
			RequestID:       requestID,
			Status:          StatusApproved,
			Reason:          fmt.Sprintf("approved by %d operators", len(pt.Approvals)),
			CheckedAt:       now,
			ApprovalsNeeded: pt.Decision.ApprovalsNeeded,
			ApprovalsHave:   len(pt.Approvals),
		}

		log.Printf("transfer-policy: FULLY APPROVED id=%s amount=$%.2f",
			requestID, pt.Request.AmountUSD)
		return decision, true
	}

	// Still pending.
	return pt.Decision, false
}

// DenyPending explicitly denies a pending transfer.
func (e *Engine) DenyPending(requestID, deniedBy, reason string) Decision {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UTC()
	pt, exists := e.pending[requestID]
	if !exists {
		return Decision{
			RequestID:  requestID,
			Status:     StatusDenied,
			DenialCode: "NOT_FOUND",
			Reason:     "no pending transfer with this ID",
			CheckedAt:  now,
		}
	}

	delete(e.pending, requestID)
	log.Printf("transfer-policy: DENIED by %s id=%s reason=%s amount=$%.2f",
		deniedBy, requestID, reason, pt.Request.AmountUSD)

	return Decision{
		RequestID:  requestID,
		Status:     StatusDenied,
		DenialCode: "OPERATOR_DENIED",
		Reason:     fmt.Sprintf("denied by %s: %s", deniedBy, reason),
		CheckedAt:  now,
	}
}

// ListPending returns all pending transfers, pruning expired ones.
func (e *Engine) ListPending() []PendingTransfer {
	e.mu.Lock()
	defer e.mu.Unlock()

	now := time.Now().UTC()
	var result []PendingTransfer

	for id, pt := range e.pending {
		if now.After(pt.ExpiresAt) {
			delete(e.pending, id)
			continue
		}
		result = append(result, *pt)
	}
	return result
}

// ── Region constraint enforcement ─────────────────────────────────────────

// checkRegionConstraints validates that the transfer doesn't violate
// venue-to-region restrictions. Returns empty string if OK.
func (e *Engine) checkRegionConstraints(req Request) string {
	if len(e.cfg.VenueRegions) == 0 {
		return "" // no region constraints configured
	}

	// Find regions for source and destination venues.
	var fromRegion, toRegion *VenueRegion
	for i := range e.cfg.VenueRegions {
		vr := &e.cfg.VenueRegions[i]
		if strings.EqualFold(vr.Venue, req.FromVenue) {
			fromRegion = vr
		}
		if strings.EqualFold(vr.Venue, req.ToVenue) {
			toRegion = vr
		}
	}

	// Check if source venue blocks the requester's region.
	if fromRegion != nil && req.Region != "" {
		for _, blocked := range fromRegion.Blocked {
			if strings.EqualFold(blocked, req.Region) {
				return fmt.Sprintf("venue %s blocks transfers from region %s",
					req.FromVenue, req.Region)
			}
		}
	}

	// Check if destination venue blocks the requester's region.
	if toRegion != nil && req.Region != "" {
		for _, blocked := range toRegion.Blocked {
			if strings.EqualFold(blocked, req.Region) {
				return fmt.Sprintf("venue %s blocks transfers to region %s",
					req.ToVenue, req.Region)
			}
		}
	}

	// Check cross-region: source venue region blocked by destination venue.
	if fromRegion != nil && toRegion != nil {
		for _, blocked := range toRegion.Blocked {
			if strings.EqualFold(blocked, fromRegion.Region) {
				return fmt.Sprintf("transfers from %s (region %s) to %s blocked",
					req.FromVenue, fromRegion.Region, req.ToVenue)
			}
		}
	}

	return ""
}

// ── Internal helpers ──────────────────────────────────────────────────────

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
