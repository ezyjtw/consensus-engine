package execution

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	TradingMode        string     `yaml:"trading_mode"`      // PAPER | LIVE
	SimSlippageBps     float64    `yaml:"sim_slippage_bps"`
	SimLatencyMs       int64      `yaml:"sim_latency_ms"`
	AdverseSelBps      float64    `yaml:"adverse_selection_bps"`
	MaxOrdersPerMinute int        `yaml:"max_orders_per_minute"`
	TenantID           string     `yaml:"tenant_id"`
	Redis              RedisConf  `yaml:"redis"`
}

type RedisConf struct {
	Addr          string `yaml:"addr"`
	Password      string `yaml:"password"`
	UseTLS        bool   `yaml:"use_tls"`
	InputStream   string `yaml:"input_stream"`   // trade:intents:approved
	OutputEvents  string `yaml:"output_events"`  // execution:events
	OutputFills   string `yaml:"output_fills"`   // demo:fills or live:fills
	ConsumerGroup string `yaml:"consumer_group"`
	ConsumerName  string `yaml:"consumer_name"`
	BlockMs       int64  `yaml:"block_ms"`
	BatchSize     int64  `yaml:"batch_size"`
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		c.Redis.Addr = v
	}
	if v := os.Getenv("REDIS_PASSWORD"); v != "" {
		c.Redis.Password = v
	}
	if os.Getenv("REDIS_TLS") == "true" {
		c.Redis.UseTLS = true
	}
	if v := os.Getenv("TRADING_MODE"); v != "" {
		c.TradingMode = v
	}
	if c.TradingMode == "" {
		c.TradingMode = "PAPER"
	}
	if c.SimSlippageBps == 0 {
		c.SimSlippageBps = 4.0
	}
	if c.SimLatencyMs == 0 {
		c.SimLatencyMs = 50
	}
	if c.AdverseSelBps == 0 {
		c.AdverseSelBps = 10.0
	}
	if c.MaxOrdersPerMinute == 0 {
		c.MaxOrdersPerMinute = 120 // 2 orders/sec default
	}
	if c.TenantID == "" {
		c.TenantID = "default"
	}
	return &c, nil
}
