package eventbus

import (
        "context"
        "encoding/json"
        "fmt"
        "time"

        "github.com/redis/go-redis/v9"
        "github.com/yourorg/consensus-engine/internal/consensus"
)

type RedisConfig struct {
        Addr            string
        Password        string
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

func New(cfg RedisConfig) (*Bus, error) {
        rdb := redis.NewClient(&redis.Options{
                Addr:     cfg.Addr,
                Password: cfg.Password,
        })
        ctx := context.Background()
        err := rdb.XGroupCreateMkStream(ctx, cfg.InputStream,
                cfg.ConsumerGroup, "$").Err()
        if err != nil && err.Error() != "BUSYGROUP Consumer Group name already exists" {
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
                                continue
                        }
                        var q consensus.Quote
                        if err := json.Unmarshal([]byte(raw), &q); err != nil {
                                continue
                        }
                        quotes = append(quotes, q)
                        _ = b.rdb.XAck(ctx, b.cfg.InputStream,
                                b.cfg.ConsumerGroup, msg.ID)
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

func (b *Bus) Close() error {
        return b.rdb.Close()
}
