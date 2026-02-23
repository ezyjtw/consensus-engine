package marketdata

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// VenueConfig holds WebSocket configuration for a single exchange.
type VenueConfig struct {
	WsURL       string            `yaml:"ws_url"`
	Symbols     []string          `yaml:"symbols"`
	SymbolMap   map[string]string `yaml:"symbol_map"`
	FeeBpsTaker float64           `yaml:"fee_bps_taker"`
}

// RedisConfig holds connection details for the output Redis stream.
type RedisConfig struct {
	Addr         string `yaml:"addr"`
	Password     string `yaml:"password"`
	UseTLS       bool   `yaml:"use_tls"`
	OutputStream string `yaml:"output_stream"`
}

// Config is the top-level market data service configuration.
type Config struct {
	Venues               map[string]VenueConfig `yaml:"venues"`
	ReconnectBackoffMs   []int                  `yaml:"reconnect_backoff_ms"`
	OrderbookDepth       int                    `yaml:"orderbook_depth"`
	FundingPollIntervalS int                    `yaml:"funding_poll_interval_s"`
	Redis                RedisConfig            `yaml:"redis"`
	TenantID             string                 `yaml:"tenant_id"`
}

// LoadConfig reads and parses the YAML config file, applying defaults.
func LoadConfig(path string) (*Config, error) {
	// Allow REDIS_ADDR env var to override config file value.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	// Defaults.
	if len(cfg.ReconnectBackoffMs) == 0 {
		cfg.ReconnectBackoffMs = []int{1000, 2000, 4000, 8000, 16000}
	}
	if cfg.OrderbookDepth == 0 {
		cfg.OrderbookDepth = 10
	}
	if cfg.FundingPollIntervalS == 0 {
		cfg.FundingPollIntervalS = 30
	}
	if cfg.Redis.Addr == "" {
		cfg.Redis.Addr = "localhost:6379"
	}
	if cfg.Redis.OutputStream == "" {
		cfg.Redis.OutputStream = "market:quotes"
	}
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}

	// Env var overrides.
	if addr := os.Getenv("REDIS_ADDR"); addr != "" {
		cfg.Redis.Addr = addr
	}
	if pw := os.Getenv("REDIS_PASSWORD"); pw != "" {
		cfg.Redis.Password = pw
	}
	if os.Getenv("REDIS_TLS") == "true" {
		cfg.Redis.UseTLS = true
	}

	return &cfg, nil
}
