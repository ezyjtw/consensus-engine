package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// FundingBusConfig configures the funding engine's stream I/O.
type FundingBusConfig struct {
	Addr          string
	Password      string
	UseTLS        bool
	InputStream   string // market:quotes
	OutputIntents string // trade:intents
	ConsumerGroup string
	ConsumerName  string
	BlockMs       time.Duration
	BatchSize     int64
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
