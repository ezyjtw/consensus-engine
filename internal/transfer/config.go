package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config is the full transfer policy configuration.
// The SHA-256 hash of the marshalled YAML is stored at LoadTime and checked
// before every transfer — if the config file changes on disk without a restart
// the engine will deny all transfers and raise an alert.
type Config struct {
	// ManualApprovalRequired: if true, ALL transfers are held as PENDING_APPROVAL
	// regardless of allowlist status. This is the V1 default.
	ManualApprovalRequired bool `yaml:"manual_approval_required"`

	// Allowlist: pre-approved destination addresses. Transfers to addresses NOT
	// on this list are always denied.
	Allowlist []AllowlistEntry `yaml:"allowlist"`

	// Velocity limits.
	MaxPerTransferUSD float64 `yaml:"max_per_transfer_usd"` // hard cap per tx
	MaxDailyUSD       float64 `yaml:"max_daily_usd"`        // rolling 24h cap
	MaxTransfersPerDay int    `yaml:"max_transfers_per_day"`
	NewAddressCooloffH int    `yaml:"new_address_cooloff_hours"` // hours before a new address can receive

	// configHash holds the SHA-256 of the raw YAML at load time.
	// Used for tamper detection — recomputed and compared before each check.
	configHash string
	rawYAML    []byte
}

// LoadConfig reads and parses the transfer policy, computing its hash for
// tamper detection. Returns an error if the file cannot be read or parsed.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading transfer policy %q: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing transfer policy: %w", err)
	}
	sum := sha256.Sum256(data)
	cfg.configHash = hex.EncodeToString(sum[:])
	cfg.rawYAML = data
	return &cfg, nil
}

// TamperCheck re-reads the policy file and verifies the SHA-256 matches the
// hash recorded at load time. Returns an error if the file has been modified.
func (c *Config) TamperCheck(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("re-reading transfer policy for tamper check: %w", err)
	}
	sum := sha256.Sum256(data)
	current := hex.EncodeToString(sum[:])
	if current != c.configHash {
		return fmt.Errorf("transfer policy TAMPERED: on-disk hash %s != loaded hash %s",
			current, c.configHash)
	}
	return nil
}

// Hash returns the SHA-256 hex of the config as loaded.
func (c *Config) Hash() string { return c.configHash }
