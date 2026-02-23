package marketdata

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"
	"github.com/ezyjtw/consensus-engine/internal/consensus"
)

// Publisher writes normalised quotes to the Redis output stream.
type Publisher struct {
	rdb    *redis.Client
	stream string
}

// NewPublisher connects to Redis and returns a Publisher.
func NewPublisher(cfg RedisConfig) (*Publisher, error) {
	var opts *redis.Options
	if strings.HasPrefix(cfg.Addr, "redis://") || strings.HasPrefix(cfg.Addr, "rediss://") {
		var err error
		opts, err = redis.ParseURL(cfg.Addr)
		if err != nil {
			return nil, fmt.Errorf("parsing redis URL: %w", err)
		}
		if cfg.Password != "" {
			opts.Password = cfg.Password
		}
	} else {
		opts = &redis.Options{
			Addr:     cfg.Addr,
			Password: cfg.Password,
		}
	}
	if cfg.UseTLS {
		if opts.TLSConfig == nil {
			opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		}
	}
	return &Publisher{rdb: redis.NewClient(opts), stream: cfg.OutputStream}, nil
}

// Publish serialises q and appends it to the market:quotes stream.
func (p *Publisher) Publish(ctx context.Context, q consensus.Quote) error {
	data, err := json.Marshal(q)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return p.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: p.stream,
		Values: map[string]interface{}{"data": string(data)},
	}).Err()
}

// Close shuts down the Redis connection.
func (p *Publisher) Close() error {
	return p.rdb.Close()
}
