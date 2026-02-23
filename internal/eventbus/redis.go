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
        "github.com/yourorg/arbsuite/internal/consensus"
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
        BlockMs         time.Duration
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
        data, err := json.Marshal(a)
        if err != nil {
                return err
        }
        return b.rdb.XAdd(ctx, &redis.XAddArgs{
                Stream: b.cfg.OutputAnomalies,
                Values: map[string]interface{}{"data": string(data)},
        }).Err()
}

func (b *Bus) PublishStatusUpdate(ctx context.Context,
        s consensus.VenueStatusUpdate) error {
        data, err := json.Marshal(s)
        if err != nil {
                return err
        }
        return b.rdb.XAdd(ctx, &redis.XAddArgs{
                Stream: b.cfg.OutputStatus,
                Values: map[string]interface{}{"data": string(data)},
        }).Err()
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

func (b *Bus) Close() error {
        return b.rdb.Close()
}
