package treasury

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadConfig reads and parses the treasury YAML configuration.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading treasury config %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing treasury config: %w", err)
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
	if v := os.Getenv("TENANT_ID"); v != "" {
		cfg.TenantID = v
	}
	if v := os.Getenv("TRANSFER_POLICY_URL"); v != "" {
		cfg.TransferPolicyURL = v
	}

	// Defaults.
	if cfg.TenantID == "" {
		cfg.TenantID = "default"
	}
	if cfg.PollIntervalSec == 0 {
		cfg.PollIntervalSec = 30
	}
	if cfg.ConvertTo == "" {
		cfg.ConvertTo = "USDC"
	}
	if cfg.SweepIntervalMin == 0 {
		cfg.SweepIntervalMin = 60
	}
	if cfg.ReconcileIntervalMin == 0 {
		cfg.ReconcileIntervalMin = 15
	}
	if cfg.DriftAlertPct == 0 {
		cfg.DriftAlertPct = 2.0
	}

	return &cfg, nil
}
