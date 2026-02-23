// Package transfer enforces institutional-grade transfer safety policies.
// Every withdrawal or cross-venue collateral move must pass through this engine
// before an execution request is submitted to any exchange API.
package transfer

import "time"

// Status describes the outcome of a policy check.
type Status string

const (
	StatusApproved Status = "APPROVED"
	StatusDenied   Status = "DENIED"
	StatusPending  Status = "PENDING_APPROVAL" // requires manual sign-off
)

// Request is a proposed transfer to be validated.
type Request struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	FromVenue   string    `json:"from_venue"`
	ToVenue     string    `json:"to_venue"`
	ToAddress   string    `json:"to_address"`   // on-chain destination
	Asset       string    `json:"asset"`        // USDT, USDC, ETH, etc.
	AmountUSD   float64   `json:"amount_usd"`
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by"` // actor name from auth context
}

// Decision is the result of a policy check.
type Decision struct {
	RequestID   string    `json:"request_id"`
	Status      Status    `json:"status"`
	DenialCode  string    `json:"denial_code,omitempty"`
	Reason      string    `json:"reason"`
	CheckedAt   time.Time `json:"checked_at"`
	RequiresApproval bool `json:"requires_approval"`
}

// AllowlistEntry is a pre-approved destination address.
type AllowlistEntry struct {
	Label     string `yaml:"label"`      // human-readable name
	Address   string `yaml:"address"`    // exact on-chain address
	Asset     string `yaml:"asset"`      // asset this entry covers; "*" = all
	MaxPerTxUSD float64 `yaml:"max_per_tx_usd"` // 0 = unlimited
	AddedAt   string `yaml:"added_at"`   // ISO date of approval
}
