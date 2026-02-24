package eventbus

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// StreamClient wraps a Redis client with helpers for stream operations.
// All service-specific buses embed or delegate to a StreamClient.
type StreamClient struct {
	rdb *redis.Client
}

// NewStreamClient creates and returns a connected Redis StreamClient.
func NewStreamClient(addr, password string, useTLS bool) (*StreamClient, error) {
	opts, err := buildOpts(addr, password, useTLS)
	if err != nil {
		return nil, err
	}
	return &StreamClient{rdb: redis.NewClient(opts)}, nil
}

func buildOpts(addr, password string, useTLS bool) (*redis.Options, error) {
	var opts *redis.Options
	if strings.HasPrefix(addr, "redis://") || strings.HasPrefix(addr, "rediss://") {
		var err error
		opts, err = redis.ParseURL(addr)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %w", err)
		}
		if password != "" {
			opts.Password = password
		}
	} else {
		opts = &redis.Options{Addr: addr, Password: password}
	}
	if useTLS && opts.TLSConfig == nil {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return opts, nil
}

// EnsureConsumerGroup creates the stream and consumer group if they don't exist.
func (c *StreamClient) EnsureConsumerGroup(ctx context.Context, stream, group string) error {
	err := c.rdb.XGroupCreateMkStream(ctx, stream, group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return fmt.Errorf("creating consumer group on %q: %w", stream, err)
	}
	return nil
}

// ReadMessages reads a batch of raw messages from a consumer group.
func (c *StreamClient) ReadMessages(ctx context.Context, stream, group, consumer string,
	count int64, block time.Duration) ([]redis.XMessage, error) {
	streams, err := c.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: consumer,
		Streams:  []string{stream, ">"},
		Count:    count,
		Block:    block,
	}).Result()
	if err != nil {
		if err == redis.Nil {
			return nil, nil
		}
		return nil, fmt.Errorf("XReadGroup: %w", err)
	}
	var msgs []redis.XMessage
	for _, s := range streams {
		msgs = append(msgs, s.Messages...)
	}
	return msgs, nil
}

// Publish serialises v as JSON and appends it to stream.
func (c *StreamClient) Publish(ctx context.Context, stream string, v interface{}) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return c.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// Ack acknowledges a message in a consumer group.
func (c *StreamClient) Ack(ctx context.Context, stream, group, id string) error {
	return c.rdb.XAck(ctx, stream, group, id).Err()
}

// KillSwitchActive returns true when the kill:switch Redis key is set.
func (c *StreamClient) KillSwitchActive(ctx context.Context) bool {
	v, err := c.rdb.Exists(ctx, "kill:switch").Result()
	if err != nil {
		return false
	}
	return v > 0
}

// SystemMode returns the current risk daemon mode (RUNNING, PAUSED, SAFE, FLATTEN, HALTED).
// Returns "RUNNING" if the key is absent.
func (c *StreamClient) SystemMode(ctx context.Context) string {
	mode := c.GetString(ctx, "risk:mode")
	if mode == "" {
		return "RUNNING"
	}
	return mode
}

// GetString fetches a Redis string key. Returns "" if not found.
func (c *StreamClient) GetString(ctx context.Context, key string) string {
	v, _ := c.rdb.Get(ctx, key).Result()
	return v
}

// SetString writes a Redis string key with an optional TTL (0 = no expiry).
func (c *StreamClient) SetString(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// HIncrByFloat increments a float field in a Redis hash.
func (c *StreamClient) HIncrByFloat(ctx context.Context, key, field string, incr float64) error {
	return c.rdb.HIncrByFloat(ctx, key, field, incr).Err()
}

// HGetAll returns all fields from a Redis hash.
func (c *StreamClient) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

// Keys returns all Redis keys matching the given glob pattern.
func (c *StreamClient) Keys(ctx context.Context, pattern string) ([]string, error) {
	return c.rdb.Keys(ctx, pattern).Result()
}

// MGet returns the string values for the given keys (nil interface{} for missing keys).
func (c *StreamClient) MGet(ctx context.Context, keys ...string) ([]interface{}, error) {
	return c.rdb.MGet(ctx, keys...).Result()
}

// Ping verifies connectivity.
func (c *StreamClient) Ping(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

// Close shuts down the underlying connection.
func (c *StreamClient) Close() error {
	return c.rdb.Close()
}
