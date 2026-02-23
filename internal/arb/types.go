package arb

import "github.com/ezyjtw/consensus-engine/internal/consensus"

// ConsensusUpdate and VenueMetrics are consumed directly from the upstream package.
type ConsensusUpdate = consensus.ConsensusUpdate
type VenueMetrics = consensus.VenueMetrics

// TradeLeg describes one side of a two-leg arb or funding intent.
type TradeLeg struct {
	Venue          string  `json:"venue"`
	Action         string  `json:"action"`           // BUY | SELL
	Type           string  `json:"type"`             // MARKET_OR_IOC
	Market         string  `json:"market,omitempty"` // PERP (default) | SPOT
	NotionalUSD    float64 `json:"notional_usd"`
	MaxSlippageBps float64 `json:"max_slippage_bps"`
	PriceLimit     float64 `json:"price_limit"`
}

// ExpectedMetrics holds the pre-trade P&L estimate embedded in each intent.
type ExpectedMetrics struct {
	EdgeBpsGross      float64 `json:"edge_bps_gross"`
	EdgeBpsNet        float64 `json:"edge_bps_net"`
	ProfitUSDNet      float64 `json:"profit_usd_net"`
	FeesUSDEst        float64 `json:"fees_usd_est"`
	SlippageUSDEst    float64 `json:"slippage_usd_est"`
	FundingRate8hBps  float64 `json:"funding_rate_8h_bps,omitempty"`
	AnnualYieldPctNet float64 `json:"annual_yield_pct_net,omitempty"`
}

// IntentConstraints carries execution safety rules for the router.
type IntentConstraints struct {
	MinQuality      string `json:"min_quality"`
	RequireVenueOK  bool   `json:"require_venue_ok"`
	MaxAgeMs        int64  `json:"max_age_ms"`
	HedgePreference string `json:"hedge_preference"`
	CooldownKey     string `json:"cooldown_key"`
}

// IntentDebug holds values useful for diagnosing why an intent was emitted.
type IntentDebug struct {
	ConsensusBandLow  float64 `json:"consensus_band_low"`
	ConsensusBandHigh float64 `json:"consensus_band_high"`
	BuyOn             string  `json:"buy_on"`
	SellOn            string  `json:"sell_on"`
	BuyExec           float64 `json:"buy_exec"`
	SellExec          float64 `json:"sell_exec"`
}

// TradeIntent is the primary output of strategy engines: a multi-leg opportunity
// ready to be validated by the Capital Allocator and executed by the Execution Router.
type TradeIntent struct {
	TenantID    string            `json:"tenant_id"`
	IntentID    string            `json:"intent_id"`
	Strategy    string            `json:"strategy"`
	Symbol      string            `json:"symbol"`
	TsMs        int64             `json:"ts_ms"`
	ExpiresMs   int64             `json:"expires_ms"`
	Legs        []TradeLeg        `json:"legs"`
	Expected    ExpectedMetrics   `json:"expected"`
	Constraints IntentConstraints `json:"constraints"`
	Debug       IntentDebug       `json:"debug"`
}

// RejectionReason is used for observability counters.
type RejectionReason string

const (
	RejectLowQuality       RejectionReason = "low_quality"
	RejectInsufficientEdge RejectionReason = "insufficient_edge"
	RejectCooldown         RejectionReason = "cooldown"
	RejectVenueFiltered    RejectionReason = "venue_filtered"
	RejectNoPairs          RejectionReason = "no_pairs"
)
