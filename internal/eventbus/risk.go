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

// Close releases the Redis connection.
func (b *RiskBus) Close() error {
	return b.sc.Close()
}
