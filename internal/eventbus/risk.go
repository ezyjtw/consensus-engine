package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/execution"
	"github.com/ezyjtw/consensus-engine/internal/risk"
)

// RiskBusConfig configures the risk daemon's stream I/O.
type RiskBusConfig struct {
	Addr          string
	Password      string
	UseTLS        bool
	EventsStream  string // execution:events
	StatusStream  string // venue_anomalies / venue_status
	AlertsStream  string // risk:alerts
	StateStream   string // risk:state
	ConsumerGroup string
	ConsumerName  string
	BlockMs       time.Duration
	BatchSize     int64
}

// RiskBus handles stream I/O for the risk daemon.
type RiskBus struct {
	sc  *StreamClient
	cfg RiskBusConfig
}

// NewRiskBus creates the bus and ensures consumer groups exist.
func NewRiskBus(cfg RiskBusConfig) (*RiskBus, error) {
	sc, err := NewStreamClient(cfg.Addr, cfg.Password, cfg.UseTLS)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	for _, stream := range []string{cfg.EventsStream, cfg.StatusStream} {
		if err := sc.EnsureConsumerGroup(ctx, stream, cfg.ConsumerGroup); err != nil {
			_ = sc.Close()
			return nil, fmt.Errorf("risk bus: %w", err)
		}
	}
	return &RiskBus{sc: sc, cfg: cfg}, nil
}

// ReadExecutionEvents reads a batch of execution events.
func (b *RiskBus) ReadExecutionEvents(ctx context.Context) ([]execution.ExecutionEvent, error) {
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.EventsStream, b.cfg.ConsumerGroup, b.cfg.ConsumerName,
		b.cfg.BatchSize, b.cfg.BlockMs)
	if err != nil {
		return nil, err
	}
	var events []execution.ExecutionEvent
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var ev execution.ExecutionEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			log.Printf("risk bus: unmarshal event: %v", err)
			continue
		}
		events = append(events, ev)
		_ = b.sc.Ack(ctx, b.cfg.EventsStream, b.cfg.ConsumerGroup, m.ID)
	}
	return events, nil
}

// ReadVenueStatusUpdates reads venue status/anomaly updates.
func (b *RiskBus) ReadVenueStatusUpdates(ctx context.Context) ([]consensus.VenueStatusUpdate, error) {
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.StatusStream, b.cfg.ConsumerGroup, b.cfg.ConsumerName,
		b.cfg.BatchSize, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	var updates []consensus.VenueStatusUpdate
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var u consensus.VenueStatusUpdate
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			continue
		}
		updates = append(updates, u)
		_ = b.sc.Ack(ctx, b.cfg.StatusStream, b.cfg.ConsumerGroup, m.ID)
	}
	return updates, nil
}

// PublishState writes the risk state to both the Redis key and the state stream.
func (b *RiskBus) PublishState(ctx context.Context, state risk.State) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	// Write to key for fast access by other services.
	if err := b.sc.SetString(ctx, "risk:mode", string(state.Mode), 0); err != nil {
		log.Printf("risk bus: set risk:mode: %v", err)
	}
	if err := b.sc.SetString(ctx, "risk:state:json", string(data), 0); err != nil {
		log.Printf("risk bus: set risk:state:json: %v", err)
	}
	// Also append to stream for history.
	return b.sc.Publish(ctx, b.cfg.StateStream, state)
}

// PublishAlert writes an alert to the alerts stream.
func (b *RiskBus) PublishAlert(ctx context.Context, alert risk.Alert) error {
	return b.sc.Publish(ctx, b.cfg.AlertsStream, alert)
}

// GetCommandedMode reads the last operator mode command from Redis.
// Returns "" if no command is pending.
func (b *RiskBus) GetCommandedMode(ctx context.Context) string {
	return b.sc.GetString(ctx, "risk:commanded_mode")
}

// ClearCommandedMode removes the commanded mode key after consuming it.
func (b *RiskBus) ClearCommandedMode(ctx context.Context) {
	b.sc.SetString(ctx, "risk:commanded_mode", "", 1*time.Millisecond) //nolint:errcheck
}

// KillSwitchActive returns true when the kill switch is engaged.
func (b *RiskBus) KillSwitchActive(ctx context.Context) bool {
	return b.sc.KillSwitchActive(ctx)
}

// FlattenOpenPositions reads all paper positions from Redis (paper:pos:{tenant}:*)
// and publishes a close intent for each non-zero position to the intents stream.
// Returns the number of close intents published.
func (b *RiskBus) FlattenOpenPositions(ctx context.Context, tenantID, intentsStream string) (int, error) {
	pattern := "paper:pos:" + tenantID + ":*"
	keys, err := b.sc.Keys(ctx, pattern)
	if err != nil {
		return 0, fmt.Errorf("flatten: scan positions: %w", err)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	vals, err := b.sc.MGet(ctx, keys...)
	if err != nil {
		return 0, fmt.Errorf("flatten: mget positions: %w", err)
	}

	count := 0
	now := time.Now().UnixMilli()
	for i, v := range vals {
		if v == nil {
			continue
		}
		var pos map[string]interface{}
		raw, _ := v.(string)
		if err := json.Unmarshal([]byte(raw), &pos); err != nil {
			continue
		}
		notional, _ := pos["notional"].(float64)
		if notional == 0 {
			continue
		}
		// key format: paper:pos:{tenant}:{venue}:{symbol}:{market}
		parts := splitKey(keys[i], 3) // drop "paper:pos:{tenant}" prefix
		if len(parts) < 3 {
			continue
		}
		venue, symbol, market := parts[0], parts[1], parts[2]
		action := "SELL"
		if notional < 0 {
			action = "BUY"
			notional = -notional
		}

		closeIntent := map[string]interface{}{
			"intent_id":  fmt.Sprintf("flatten-%d-%d", i, now),
			"strategy":   "FLATTEN",
			"symbol":     symbol,
			"ts_ms":      now,
			"expires_ms": now + 30000,
			"source":     "risk_daemon",
			"legs": []map[string]interface{}{{
				"venue":            venue,
				"action":           action,
				"notional_usd":     notional,
				"market":           market,
				"max_slippage_bps": 200, // accept wide slippage to ensure fill
			}},
		}
		if err := b.sc.Publish(ctx, intentsStream, closeIntent); err != nil {
			log.Printf("flatten: publish close intent for %s/%s: %v", venue, symbol, err)
			continue
		}
		count++
		log.Printf("risk-daemon: FLATTEN close intent: venue=%s symbol=%s action=%s notional=%.2f",
			venue, symbol, action, notional)
	}
	return count, nil
}

// OpenPositionCount returns the number of non-zero paper positions for a tenant.
func (b *RiskBus) OpenPositionCount(ctx context.Context, tenantID string) int {
	pattern := "paper:pos:" + tenantID + ":*"
	keys, err := b.sc.Keys(ctx, pattern)
	if err != nil || len(keys) == 0 {
		return 0
	}
	vals, err := b.sc.MGet(ctx, keys...)
	if err != nil {
		return 0
	}
	count := 0
	for _, v := range vals {
		if v == nil {
			continue
		}
		raw, _ := v.(string)
		var pos map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &pos); err != nil {
			continue
		}
		if n, _ := pos["notional"].(float64); n != 0 {
			count++
		}
	}
	return count
}

func splitKey(key string, skip int) []string {
	var parts []string
	cur := ""
	skipped := 0
	for _, c := range key {
		if c == ':' {
			if skipped < skip {
				skipped++
				cur = ""
				continue
			}
			parts = append(parts, cur)
			cur = ""
		} else {
			cur += string(c)
		}
	}
	if cur != "" {
		parts = append(parts, cur)
	}
	return parts
}

// Close releases the Redis connection.
func (b *RiskBus) Close() error {
	return b.sc.Close()
}
