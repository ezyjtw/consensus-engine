package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/execution"
)

// AllocatorBusConfig configures the capital allocator's stream I/O.
type AllocatorBusConfig struct {
	Addr          string
	Password      string
	UseTLS        bool
	InputStream   string // trade:intents
	OutputStream  string // trade:intents:approved
	FillsStream   string // demo:fills or live:fills — for notional release
	OIStream      string // market:open_interest — for OI-gated sizing
	ConsumerGroup string
	ConsumerName  string
	BlockMs       time.Duration //nolint:staticcheck // field name matches YAML config key
	BatchSize     int64
}

// AllocatorBus handles Redis stream I/O for the capital allocator.
type AllocatorBus struct {
	sc  *StreamClient
	cfg AllocatorBusConfig
}

// NewAllocatorBus creates the bus and ensures the consumer group exists.
func NewAllocatorBus(cfg AllocatorBusConfig) (*AllocatorBus, error) {
	sc, err := NewStreamClient(cfg.Addr, cfg.Password, cfg.UseTLS)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	if err := sc.EnsureConsumerGroup(ctx, cfg.InputStream, cfg.ConsumerGroup); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("allocator bus: %w", err)
	}
	if cfg.FillsStream != "" {
		if err := sc.EnsureConsumerGroup(ctx, cfg.FillsStream, cfg.ConsumerGroup); err != nil {
			log.Printf("allocator bus: fills group on %q: %v (ok if stream not yet created)", cfg.FillsStream, err)
		}
	}
	if cfg.OIStream != "" {
		oiGroup := cfg.ConsumerGroup + "-oi"
		if err := sc.EnsureConsumerGroup(ctx, cfg.OIStream, oiGroup); err != nil {
			log.Printf("allocator bus: OI group on %q: %v (ok if stream not yet created)", cfg.OIStream, err)
		}
	}
	return &AllocatorBus{sc: sc, cfg: cfg}, nil
}

// ReadFills drains completed fill events so the engine can release deployed notional.
func (b *AllocatorBus) ReadFills(ctx context.Context) ([]execution.SimulatedFill, error) {
	if b.cfg.FillsStream == "" {
		return nil, nil
	}
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.FillsStream, b.cfg.ConsumerGroup, b.cfg.ConsumerName,
		b.cfg.BatchSize, 0)
	if err != nil {
		return nil, err
	}
	var fills []execution.SimulatedFill
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			_ = b.sc.Ack(ctx, b.cfg.FillsStream, b.cfg.ConsumerGroup, m.ID)
			continue
		}
		var f execution.SimulatedFill
		if err := json.Unmarshal([]byte(raw), &f); err == nil {
			fills = append(fills, f)
		}
		_ = b.sc.Ack(ctx, b.cfg.FillsStream, b.cfg.ConsumerGroup, m.ID)
	}
	return fills, nil
}

// ReadOIUpdates reads open interest updates from the OI stream.
// Returns raw JSON strings for the OI tracker to parse.
func (b *AllocatorBus) ReadOIUpdates(ctx context.Context) ([]string, error) {
	if b.cfg.OIStream == "" {
		return nil, nil
	}
	oiGroup := b.cfg.ConsumerGroup + "-oi"
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.OIStream, oiGroup, b.cfg.ConsumerName,
		b.cfg.BatchSize, 100*time.Millisecond)
	if err != nil {
		return nil, err
	}
	var raws []string
	for _, m := range msgs {
		raw, ok := m.Values["data"].(string)
		if !ok {
			_ = b.sc.Ack(ctx, b.cfg.OIStream, oiGroup, m.ID)
			continue
		}
		raws = append(raws, raw)
		_ = b.sc.Ack(ctx, b.cfg.OIStream, oiGroup, m.ID)
	}
	return raws, nil
}

// ReadIntents reads a batch of raw trade intents from the input stream.
func (b *AllocatorBus) ReadIntents(ctx context.Context) ([]arb.TradeIntent, error) {
	msgs, err := b.sc.ReadMessages(ctx,
		b.cfg.InputStream, b.cfg.ConsumerGroup, b.cfg.ConsumerName,
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
			log.Printf("allocator bus: unmarshal intent: %v", err)
			continue
		}
		intents = append(intents, intent)
		_ = b.sc.Ack(ctx, b.cfg.InputStream, b.cfg.ConsumerGroup, m.ID)
	}
	return intents, nil
}

// PublishApproved appends an approved intent to the output stream.
func (b *AllocatorBus) PublishApproved(ctx context.Context, intent arb.TradeIntent) error {
	return b.sc.Publish(ctx, b.cfg.OutputStream, intent)
}

// SystemMode returns the current system operating mode from Redis.
// Returns "RUNNING" if the key is absent (safe default).
func (b *AllocatorBus) SystemMode(ctx context.Context) string {
	mode := b.sc.GetString(ctx, "risk:mode")
	if mode == "" {
		return "RUNNING"
	}
	return mode
}

// KillSwitchActive returns true when the kill switch is engaged.
func (b *AllocatorBus) KillSwitchActive(ctx context.Context) bool {
	return b.sc.KillSwitchActive(ctx)
}

// Close releases the Redis connection.
func (b *AllocatorBus) Close() error {
	return b.sc.Close()
}
