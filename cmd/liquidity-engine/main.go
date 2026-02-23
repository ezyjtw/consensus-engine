package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ezyjtw/consensus-engine/internal/consensus"
	"github.com/ezyjtw/consensus-engine/internal/eventbus"
	"github.com/ezyjtw/consensus-engine/internal/liquidity"
	"gopkg.in/yaml.v3"
)

type Config struct {
	liquidity.Policy `yaml:",inline"`
	Redis            RedisConfig `yaml:"redis"`
}

type RedisConfig struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	InputStream   string `yaml:"input_stream"`
	OutputIntents string `yaml:"output_intents"`
	ConsumerGroup string `yaml:"consumer_group"`
	ConsumerName  string `yaml:"consumer_name"`
	BlockMs       int64  `yaml:"block_ms"`
	BatchSize     int64  `yaml:"batch_size"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		cfg.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		cfg.Redis.Password = v
	}
	if os.Getenv("REDIS_TLS") == "true" {
		cfg.Redis.UseTLS = true
	}
	if cfg.Redis.ConsumerGroup == "" {
		cfg.Redis.ConsumerGroup = "liquidity-engine"
	}
	if cfg.Redis.ConsumerName == "" {
		cfg.Redis.ConsumerName = "worker-1"
	}
	return &cfg, nil
}

func main() {
	cfgPath := flag.String("config", "configs/policies/liquidity_engine.yaml",
		"Path to liquidity engine config YAML")
	flag.Parse()

	if v := os.Getenv("REDIS_ADDR"); v != "" {
		log.Printf("REDIS_ADDR: %s", v)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("liquidity-engine: load config: %v", err)
	}
	log.Printf("liquidity-engine: spread_blowout_mult=%.1f thin_book_usd=%.0f mark_index_bps=%.1f",
		cfg.SpreadBlowoutMult, cfg.ThinBookThresholdUSD, cfg.MarkIndexDivergeBps)

	engine := liquidity.NewEngine(&cfg.Policy)

	sc, err := eventbus.NewStreamClient(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.UseTLS)
	if err != nil {
		log.Fatalf("liquidity-engine: redis connect: %v", err)
	}
	defer sc.Close()

	ctx, cancel := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := sc.EnsureConsumerGroup(ctx, cfg.Redis.InputStream, cfg.Redis.ConsumerGroup); err != nil {
		log.Printf("liquidity-engine: ensure group: %v", err)
	}

	intentsEmitted := 0
	statsTicker := time.NewTicker(60 * time.Second)
	defer statsTicker.Stop()

	log.Println("liquidity-engine: started")

	for {
		select {
		case <-ctx.Done():
			log.Printf("liquidity-engine: shutdown, emitted=%d intents", intentsEmitted)
			return
		case <-statsTicker.C:
			log.Printf("liquidity-engine: intents_emitted=%d", intentsEmitted)
		default:
		}

		// Check kill switch.
		if sc.KillSwitchActive(ctx) {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// Read latest market quotes.
		msgs, err := sc.ReadMessages(ctx,
			cfg.Redis.InputStream, cfg.Redis.ConsumerGroup, cfg.Redis.ConsumerName,
			cfg.Redis.BatchSize, time.Duration(cfg.Redis.BlockMs)*time.Millisecond)
		if err != nil {
			log.Printf("liquidity-engine: read: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Read current consensus quality for each symbol from Redis.
		for _, m := range msgs {
			raw, ok := m.Values["data"].(string)
			if !ok {
				continue
			}
			var q consensus.Quote
			if err := json.Unmarshal([]byte(raw), &q); err != nil {
				continue
			}

			// Read consensus quality from latest consensus:updates key.
			quality := sc.GetString(ctx, "consensus:quality:"+string(q.Symbol))
			if quality == "" {
				quality = "HIGH" // assume high if key absent (engine not yet running)
			}

			intents := engine.Evaluate(q, quality)
			for _, intent := range intents {
				if err := sc.Publish(ctx, cfg.Redis.OutputIntents, intent); err != nil {
					log.Printf("liquidity-engine: publish intent: %v", err)
					continue
				}
				intentsEmitted++
				log.Printf("liquidity-engine: intent strategy=%s symbol=%s venue=%s",
					intent.Strategy, intent.Symbol, intent.Legs[0].Venue)
			}
		}
	}
}
