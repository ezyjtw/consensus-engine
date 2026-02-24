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
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

type RedisConfig struct {
	Addr            string
	Password        string
	UseTLS          bool
	InputStream     string
	OutputConsensus string
	OutputAnomalies string
	OutputStatus    string
	ConsumerGroup   string
	ConsumerName    string
	BlockMs         time.Duration //nolint:staticcheck // field name matches YAML config key
	BatchSize       int64
}

type Bus struct {
	rdb *redis.Client
	cfg RedisConfig
}

func buildRedisOptions(cfg RedisConfig) (*redis.Options, error) {
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

func New(cfg RedisConfig) (*Bus, error) {
	opts, err := buildRedisOptions(cfg)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opts)
	ctx := context.Background()
	err = rdb.XGroupCreateMkStream(ctx, cfg.InputStream,
		cfg.ConsumerGroup, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		_ = rdb.Close()
		return nil, fmt.Errorf("creating consumer group: %w", err)
	}
	return &Bus{rdb: rdb, cfg: cfg}, nil
}

func (b *Bus) ReadMarketUpdates(ctx context.Context) ([]consensus.Quote, error) {
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
	var quotes []consensus.Quote
	for _, stream := range streams {
		for _, msg := range stream.Messages {
			raw, ok := msg.Values["data"].(string)
			if !ok {
				log.Printf("eventbus: msg %s missing 'data' field, skipping", msg.ID)
				continue
			}
			var q consensus.Quote
			if err := json.Unmarshal([]byte(raw), &q); err != nil {
				log.Printf("eventbus: msg %s unmarshal error: %v, skipping", msg.ID, err)
				continue
			}
			quotes = append(quotes, q)
			if err := b.rdb.XAck(ctx, b.cfg.InputStream,
				b.cfg.ConsumerGroup, msg.ID).Err(); err != nil {
				log.Printf("eventbus: XAck msg %s failed: %v", msg.ID, err)
			}
		}
	}
	return quotes, nil
}

func (b *Bus) PublishConsensus(ctx context.Context,
	u consensus.ConsensusUpdate) error {
	u.SchemaVersion = 1
	data, err := json.Marshal(u)
	if err != nil {
		return err
	}
	return b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: b.cfg.OutputConsensus,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

func (b *Bus) PublishAnomaly(ctx context.Context,
	a consensus.VenueAnomaly) error {
	a.SchemaVersion = 1
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	return b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: b.cfg.OutputAnomalies,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// PublishStatusUpdate appends a venue state transition to the status stream and
// caches the latest state in a Redis key so the consensus engine can recover it
// after a restart without querying Postgres.
func (b *Bus) PublishStatusUpdate(ctx context.Context,
	s consensus.VenueStatusUpdate) error {
	s.SchemaVersion = 1
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	// Append to stream (consumed by ledger for Postgres history).
	if err := b.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: b.cfg.OutputStatus,
		Values: map[string]interface{}{"data": string(data)},
	}).Err(); err != nil {
		return err
	}
	// Cache latest state per venue+symbol for fast restart recovery.
	cacheKey := "consensus:venue_state:" + s.TenantID + ":" + string(s.Venue) + ":" + string(s.Symbol)
	b.rdb.Set(ctx, cacheKey, string(data), 0) //nolint:errcheck
	return nil
}

// LoadVenueState returns the last-known VenueStatusUpdate for a venue+symbol from
// the Redis cache written by PublishStatusUpdate. Returns nil if not found.
func (b *Bus) LoadVenueState(ctx context.Context, tenantID string,
	venue consensus.Venue, symbol consensus.Symbol) *consensus.VenueStatusUpdate {
	key := "consensus:venue_state:" + tenantID + ":" + string(venue) + ":" + string(symbol)
	raw, err := b.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil
	}
	var su consensus.VenueStatusUpdate
	if err := json.Unmarshal([]byte(raw), &su); err != nil {
		return nil
	}
	return &su
}

// KillSwitchActive returns true when the dashboard kill switch is active.
// The consensus engine checks this before publishing outputs.
func (b *Bus) KillSwitchActive(ctx context.Context) bool {
	val, err := b.rdb.Exists(ctx, "kill:switch").Result()
	if err != nil {
		return false
	}
	return val > 0
}

// SystemMode returns the current risk daemon mode (RUNNING, PAUSED, SAFE, FLATTEN, HALTED).
// Returns "RUNNING" if the key is absent.
func (b *Bus) SystemMode(ctx context.Context) string {
	mode, _ := b.rdb.Get(ctx, "risk:mode").Result()
	if mode == "" {
		return "RUNNING"
	}
	return mode
}

func (b *Bus) Close() error {
	return b.rdb.Close()
}
