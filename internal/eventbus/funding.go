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

// FundingBusConfig configures the funding engine's stream I/O.
type FundingBusConfig struct {
	Addr           string
	Password       string
	UseTLS         bool
	InputStream    string // market:quotes
	EventsStream   string // execution:events (for position tracking)
	OutputIntents  string // trade:intents
	ConsumerGroup  string
	ConsumerName   string
	BlockMs        time.Duration //nolint:staticcheck // field name matches YAML config key
	BatchSize      int64
}

// FundingBus handles Redis stream I/O for the funding engine.
type FundingBus struct {
	sc  *StreamClient
	cfg FundingBusConfig
}

// NewFundingBus creates a FundingBus, initialising the consumer group.
func NewFundingBus(cfg FundingBusConfig) (*FundingBus, error) {
	sc, err := NewStreamClient(cfg.Addr, cfg.Password, cfg.UseTLS)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := sc.EnsureConsumerGroup(ctx, cfg.InputStream, cfg.ConsumerGroup); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("funding bus: %w", err)
	}
	// Create consumer group for execution events (position tracking).
	if cfg.EventsStream != "" {
		evGroup := cfg.ConsumerGroup + "-positions"
		if err := sc.EnsureConsumerGroup(ctx, cfg.EventsStream, evGroup); err != nil {
			_ = sc.Close()
			return nil, fmt.Errorf("funding bus (events): %w", err)
		}
	}
	return &FundingBus{sc: sc, cfg: cfg}, nil
}

// ReadQuotes reads a batch of market:quotes from the input stream.
func (b *FundingBus) ReadQuotes(ctx context.Context) ([]consensus.Quote, error) {
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.InputStream, b.cfg.ConsumerGroup, b.cfg.ConsumerName,
		b.cfg.BatchSize, b.cfg.BlockMs)
	if err != nil {
		return nil, err
	}
	var quotes []consensus.Quote
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			continue
		}
		var q consensus.Quote
		if err := json.Unmarshal([]byte(raw), &q); err != nil {
			log.Printf("funding bus: unmarshal quote: %v", err)
			continue
		}
		quotes = append(quotes, q)
		_ = b.sc.Ack(ctx, b.cfg.InputStream, b.cfg.ConsumerGroup, m.ID)
	}
	return quotes, nil
}

// ReadExecutionEvents reads execution events for position tracking.
// Uses a separate consumer group ("<group>-positions") to avoid interference.
func (b *FundingBus) ReadExecutionEvents(ctx context.Context) ([]execution.ExecutionEvent, error) {
	if b.cfg.EventsStream == "" {
		return nil, nil
	}
	evGroup := b.cfg.ConsumerGroup + "-positions"
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.EventsStream, evGroup, b.cfg.ConsumerName,
		100, 100*time.Millisecond)
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
			log.Printf("funding bus: unmarshal event: %v", err)
			continue
		}
		events = append(events, ev)
		_ = b.sc.Ack(ctx, b.cfg.EventsStream, evGroup, m.ID)
	}
	return events, nil
}

// PublishIntent appends a TradeIntent to the intents stream.
func (b *FundingBus) PublishIntent(ctx context.Context, intent arb.TradeIntent) error {
	return b.sc.Publish(ctx, b.cfg.OutputIntents, intent)
}

// KillSwitchActive returns true when the kill switch is engaged.
func (b *FundingBus) KillSwitchActive(ctx context.Context) bool {
	return b.sc.KillSwitchActive(ctx)
}

// Close releases the Redis connection.
func (b *FundingBus) Close() error {
	return b.sc.Close()
}
