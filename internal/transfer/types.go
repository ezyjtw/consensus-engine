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
	Network     string    `json:"network,omitempty"`     // target network
	Region      string    `json:"region,omitempty"`      // requester's region/jurisdiction
	RequestedAt time.Time `json:"requested_at"`
	RequestedBy string    `json:"requested_by"` // actor name from auth context
}

// Decision is the result of a policy check.
type Decision struct {
	RequestID        string    `json:"request_id"`
	Status           Status    `json:"status"`
	DenialCode       string    `json:"denial_code,omitempty"`
	Reason           string    `json:"reason"`
	CheckedAt        time.Time `json:"checked_at"`
	RequiresApproval bool      `json:"requires_approval"`
	ApprovalsNeeded  int       `json:"approvals_needed,omitempty"`  // dual approval: how many sign-offs needed
	ApprovalsHave    int       `json:"approvals_have,omitempty"`    // current approval count
}

// AllowlistEntry is a pre-approved destination address.
type AllowlistEntry struct {
	Label     string `yaml:"label"`      // human-readable name
	Address   string `yaml:"address"`    // exact on-chain address
	Asset     string `yaml:"asset"`      // asset this entry covers; "*" = all
	MaxPerTxUSD float64 `yaml:"max_per_tx_usd"` // 0 = unlimited
	AddedAt   string `yaml:"added_at"`   // ISO date of approval
}

// Approval records a single sign-off on a pending transfer.
type Approval struct {
	RequestID  string    `json:"request_id"`
	ApprovedBy string    `json:"approved_by"` // actor name
	ApprovedAt time.Time `json:"approved_at"`
	Comment    string    `json:"comment,omitempty"`
}

// PendingTransfer is a transfer awaiting approval(s).
type PendingTransfer struct {
	Request   Request    `json:"request"`
	Decision  Decision   `json:"decision"`
	Approvals []Approval `json:"approvals"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"` // auto-deny after this time
}

// VenueRegion maps a venue to its operating region/jurisdiction.
type VenueRegion struct {
	Venue   string   `yaml:"venue"`
	Region  string   `yaml:"region"`           // e.g. "US", "EU", "APAC"
	Blocked []string `yaml:"blocked_regions"`   // regions that cannot transfer to/from this venue
}
