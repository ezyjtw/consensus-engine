package eventbus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/ezyjtw/consensus-engine/internal/arb"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// ArbRedisConfig configures the arb engine's Redis connection and stream names.
type ArbRedisConfig struct {
	Addr               string
	Password           string
	UseTLS             bool
	InputStream        string        // e.g. "consensus:updates"
	MarketQuotesStream string        // e.g. "market:quotes" (for basis tracker)
	OutputIntents      string        // e.g. "trade:intents"
	ConsumerGroup      string
	ConsumerName       string
	BlockMs            time.Duration //nolint:staticcheck // field name matches YAML config key
	BatchSize          int64
}

// ArbBus handles all Redis stream I/O for the arb opportunity engine.
type ArbBus struct {
	rdb *redis.Client
	cfg ArbRedisConfig
}

// NewArbBus creates a Redis client, connects, and creates the consumer group if needed.
func NewArbBus(cfg ArbRedisConfig) (*ArbBus, error) {
	opts, err := buildArbRedisOptions(cfg)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	err = rdb.XGroupCreateMkStream(ctx, cfg.InputStream,
		cfg.ConsumerGroup, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		_ = rdb.Close()
		return nil, fmt.Errorf("creating consumer group on %q: %w", cfg.InputStream, err)
	}
	// Create consumer group for market:quotes if basis tracking is configured.
	if cfg.MarketQuotesStream != "" {
		quotesGroup := cfg.ConsumerGroup + "-basis"
		err = rdb.XGroupCreateMkStream(ctx, cfg.MarketQuotesStream,
			quotesGroup, "$").Err()
		if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
			_ = rdb.Close()
			return nil, fmt.Errorf("creating consumer group on %q: %w", cfg.MarketQuotesStream, err)
		}
	}
	return &ArbBus{rdb: rdb, cfg: cfg}, nil
}

// ReadConsensusUpdates reads a batch of ConsensusUpdate messages from the input stream.
// Returns nil, nil when the block timeout fires with no messages.
func (b *ArbBus) ReadConsensusUpdates(ctx context.Context) ([]consensus.ConsensusUpdate, error) {
	streams, err := b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    b.cfg.ConsumerGroup,
		Consumer: b.cfg.ConsumerName,
		Streams:  []string{b.cfg.InputStream, ">"},
		Count:    b.cfg.BatchSize,
		Block:    b.cfg.BlockMs,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("XReadGroup: %w", err)
	}

	var updates []consensus.ConsensusUpdate
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			raw, ok := msg.Values["data"].(string)
			if !ok {
				log.Printf("arb eventbus: msg %s missing 'data' field, skipping", msg.ID)
				continue
			}
			var u consensus.ConsensusUpdate
			if err := json.Unmarshal([]byte(raw), &u); err != nil {
				log.Printf("arb eventbus: msg %s unmarshal error: %v, skipping", msg.ID, err)
				continue
			}
			updates = append(updates, u)
			if err := b.rdb.XAck(ctx, b.cfg.InputStream,
				b.cfg.ConsumerGroup, msg.ID).Err(); err != nil {
				log.Printf("arb eventbus: XAck msg %s failed: %v", msg.ID, err)
			}
		}
	}
	return updates, nil
}

// ReadMarketQuotes reads raw quotes from the market:quotes stream for basis tracking.
// Uses a separate consumer group ("<group>-basis") to avoid interfering with other consumers.
func (b *ArbBus) ReadMarketQuotes(ctx context.Context) ([]consensus.Quote, error) {
	if b.cfg.MarketQuotesStream == "" {
		return nil, nil
	}
	quotesGroup := b.cfg.ConsumerGroup + "-basis"
	streams, err := b.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    quotesGroup,
		Consumer: b.cfg.ConsumerName,
		Streams:  []string{b.cfg.MarketQuotesStream, ">"},
		Count:    200,
		Block:    100 * time.Millisecond,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("XReadGroup (quotes): %w", err)
	}
	var quotes []consensus.Quote
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			raw, ok := msg.Values["data"].(string)
			if !ok {
				continue
			}
			var q consensus.Quote
			if err := json.Unmarshal([]byte(raw), &q); err != nil {
				continue
			}
			quotes = append(quotes, q)
			_ = b.rdb.XAck(ctx, b.cfg.MarketQuotesStream, quotesGroup, msg.ID).Err()
		}
	}
	return quotes, nil
}

// PublishTradeIntent appends a TradeIntent to the output intents stream.
func (b *ArbBus) PublishTradeIntent(ctx context.Context, intent arb.TradeIntent) error {
	data, err := json.Marshal(intent)
	if err != nil {
		return fmt.Errorf("marshalling intent: %w", err)
	}
	return b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: b.cfg.OutputIntents,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// KillSwitchActive returns true when the dashboard kill switch key is present.
func (b *ArbBus) KillSwitchActive(ctx context.Context) bool {
	val, err := b.rdb.Exists(ctx, "kill:switch").Result()
	if err != nil {
		return false
	}
	return val > 0
}

// Close releases the underlying Redis connection.
func (b *ArbBus) Close() error {
	return b.rdb.Close()
}

func buildArbRedisOptions(cfg ArbRedisConfig) (*redis.Options, error) {
	if strings.HasPrefix(cfg.Addr, "redis://") || strings.HasPrefix(cfg.Addr, "rediss://") {
		opts, err := redis.ParseURL(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %w", err)
		}
		if cfg.Password != "" {
			opts.Password = cfg.Password
		}
		if cfg.UseTLS && opts.TLSConfig == nil {
			opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
		return opts, nil
	}
	opts := &redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
	}
	if cfg.UseTLS {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}
