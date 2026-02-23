package consensus

type Venue string
type Symbol string

type VenueState string

const (
	StateOK          VenueState = "OK"
	StateWarn        VenueState = "WARN"
	StateBlacklisted VenueState = "BLACKLISTED"
)

type Orderbook struct {
	Bids [][2]float64 `json:"bids"`
	Asks [][2]float64 `json:"asks"`
}

type FeedHealth struct {
	WsConnected bool  `json:"ws_connected"`
	LastMsgTsMs int64 `json:"last_msg_ts_ms"`
}

type Quote struct {
	TenantID     string     `json:"tenant_id"`
	Venue        Venue      `json:"venue"`
	Symbol       Symbol     `json:"symbol"`
	TsMs         int64      `json:"ts_ms"`
	BestBid      float64    `json:"best_bid"`
	BestAsk      float64    `json:"best_ask"`
	Mark         float64    `json:"mark,omitempty"`
	Index        float64    `json:"index,omitempty"`
	BidDepth1Pct float64    `json:"bid_depth_1pct,omitempty"`
	AskDepth1Pct float64    `json:"ask_depth_1pct,omitempty"`
	Orderbook    *Orderbook `json:"orderbook,omitempty"`
	FeeBpsTaker  float64    `json:"fee_bps_taker"`
	FundingRate  float64    `json:"funding_rate,omitempty"`
	FeedHealth   FeedHealth `json:"feed_health"`
}

type VenueStatus struct {
	State              VenueState
	OutlierSinceMs     int64
	HardOutlierSinceMs int64
	WarnSinceMs        int64
	BlacklistUntilMs   int64
	RecoverySinceMs    int64
	Reason             string
}

type VenueMetrics struct {
	Venue         Venue      `json:"venue"`
	Status        VenueState `json:"status"`
	Trust         float64    `json:"trust"`
	Mid           float64    `json:"mid"`
	BuyExec       float64    `json:"buy_exec"`
	SellExec      float64    `json:"sell_exec"`
	EffectiveBuy  float64    `json:"effective_buy"`
	EffectiveSell float64    `json:"effective_sell"`
	DeviationBps  float64    `json:"deviation_bps"`
	Flags         []string   `json:"flags,omitempty"`
}

type Consensus struct {
	Mid      float64 `json:"mid"`
	BuyExec  float64 `json:"buy_exec"`
	SellExec float64 `json:"sell_exec"`
	BandLow  float64 `json:"band_low"`
	BandHigh float64 `json:"band_high"`
	Quality  string  `json:"quality"`
}

type ConsensusUpdate struct {
	TenantID        string         `json:"tenant_id"`
	Symbol          Symbol         `json:"symbol"`
	TsMs            int64          `json:"ts_ms"`
	SizeNotionalUSD float64        `json:"size_notional_usd"`
	Consensus       Consensus      `json:"consensus"`
	Venues          []VenueMetrics `json:"venues"`
}

type VenueAnomaly struct {
	TenantID          string  `json:"tenant_id"`
	Symbol            Symbol  `json:"symbol"`
	Venue             Venue   `json:"venue"`
	TsMs              int64   `json:"ts_ms"`
	AnomalyType       string  `json:"anomaly_type"`
	Severity          string  `json:"severity"`
	DeviationBps      float64 `json:"deviation_bps"`
	ConsensusMid      float64 `json:"consensus_mid"`
	VenueMid          float64 `json:"venue_mid"`
	WindowMs          int64   `json:"window_ms"`
	RecommendedAction string  `json:"recommended_action"`
}

type VenueStatusUpdate struct {
	TenantID string     `json:"tenant_id"`
	Venue    Venue      `json:"venue"`
	Symbol   Symbol     `json:"symbol"`
	TsMs     int64      `json:"ts_ms"`
	Status   VenueState `json:"status"`
	TtlMs    int64      `json:"ttl_ms,omitempty"`
	Reason   string     `json:"reason"`
}
