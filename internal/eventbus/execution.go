package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/execution"
)

// ExecutionBusConfig configures the execution router's stream I/O.
type ExecutionBusConfig struct {
	Addr          string
	Password      string
	UseTLS        bool
	IntentsStream   string // trade:intents:approved
	ConsensusStream string // consensus:updates
	EventsStream    string // execution:events
	FillsStream     string // demo:fills | live:fills
	ConsumerGroup string
	ConsumerName  string
	BlockMs       time.Duration //nolint:staticcheck // field name matches YAML config key
	BatchSize     int64
}

// ExecutionBus handles stream I/O for the execution router.
type ExecutionBus struct {
	sc  *StreamClient
	cfg ExecutionBusConfig
}

// NewExecutionBus creates the bus and ensures consumer groups exist.
func NewExecutionBus(cfg ExecutionBusConfig) (*ExecutionBus, error) {
	sc, err := NewStreamClient(cfg.Addr, cfg.Password, cfg.UseTLS)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	for _, stream := range []string{cfg.IntentsStream, cfg.ConsensusStream} {
		if err := sc.EnsureConsumerGroup(ctx, stream, cfg.ConsumerGroup); err != nil {
			_ = sc.Close()
			return nil, fmt.Errorf("execution bus: %w", err)
		}
	}
	return &ExecutionBus{sc: sc, cfg: cfg}, nil
}

// ReadApprovedIntents reads a batch of approved intents.
func (b *ExecutionBus) ReadApprovedIntents(ctx context.Context) ([]arb.TradeIntent, error) {
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.IntentsStream, b.cfg.ConsumerGroup, b.cfg.ConsumerName,
		b.cfg.BatchSize, b.cfg.BlockMs)
	if err != nil {
		return nil, err
	}
	var intents []arb.TradeIntent
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var intent arb.TradeIntent
		if err := json.Unmarshal([]byte(raw), &intent); err != nil {
			log.Printf("execution bus: unmarshal intent: %v", err)
			continue
		}
		intents = append(intents, intent)
		_ = b.sc.Ack(ctx, b.cfg.IntentsStream, b.cfg.ConsumerGroup, m.ID)
	}
	return intents, nil
}

// ReadConsensusUpdates reads consensus updates to keep the price cache warm.
func (b *ExecutionBus) ReadConsensusUpdates(ctx context.Context) ([]consensus.ConsensusUpdate, error) {
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.ConsensusStream, b.cfg.ConsumerGroup+"-consensus", b.cfg.ConsumerName,
		50, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	var updates []consensus.ConsensusUpdate
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var u consensus.ConsensusUpdate
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			continue
		}
		updates = append(updates, u)
		_ = b.sc.Ack(ctx, b.cfg.ConsensusStream, b.cfg.ConsumerGroup+"-consensus", m.ID)
	}
	return updates, nil
}

// PublishEvent appends an execution event to the events stream.
func (b *ExecutionBus) PublishEvent(ctx context.Context, ev execution.ExecutionEvent) error {
	ev.SchemaVersion = 1
	return b.sc.Publish(ctx, b.cfg.EventsStream, ev)
}

// PublishFill appends a simulated fill to the fills stream.
func (b *ExecutionBus) PublishFill(ctx context.Context, fill *execution.SimulatedFill) error {
	fill.SchemaVersion = 1
	return b.sc.Publish(ctx, b.cfg.FillsStream, fill)
}

// KillSwitchActive returns true when the kill switch is engaged.
func (b *ExecutionBus) KillSwitchActive(ctx context.Context) bool {
	return b.sc.KillSwitchActive(ctx)
}

// SystemMode returns the current risk mode.
func (b *ExecutionBus) SystemMode(ctx context.Context) string {
	mode := b.sc.GetString(ctx, "risk:mode")
	if mode == "" {
		return "RUNNING"
	}
	return mode
}

// WritePosition persists a paper position to a Redis key for the dashboard.
// Implements execution.PositionWriter.
func (b *ExecutionBus) WritePosition(ctx context.Context, tenantID, key string, pos map[string]float64) {
	redisKey := fmt.Sprintf("paper:pos:%s:%s", tenantID, key)
	data, err := json.Marshal(pos)
	if err != nil {
		return
	}
	_ = b.sc.SetString(ctx, redisKey, string(data), 0)
}

// Close releases the Redis connection.
func (b *ExecutionBus) Close() error {
	return b.sc.Close()
}
