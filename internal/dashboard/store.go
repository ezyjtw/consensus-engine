package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	keyConnPrefix   = "dashboard:conn:"
	keyAlertConfig  = "dashboard:alert:config"
	keyKillSwitch   = "kill:switch"
	keyFundingStages = "config:funding:stages"
)

// ConnectionConfig holds exchange API credentials (stored encrypted).
type ConnectionConfig struct {
	Exchange   string `json:"exchange"`
	APIKey     string `json:"api_key"`
	APISecret  string `json:"api_secret"`
	Passphrase string `json:"passphrase,omitempty"`
	Configured bool   `json:"configured"`
}

// AlertConfig holds alert thresholds and webhook delivery settings.
type AlertConfig struct {
	WebhookURL         string  `json:"webhook_url"`
	OnQualityLow       bool    `json:"on_quality_low"`
	OnVenueBlacklisted bool    `json:"on_venue_blacklisted"`
	OnAnomalyHigh      bool    `json:"on_anomaly_high"`
	OnAnomalyMedium    bool    `json:"on_anomaly_medium"`
	DeviationBpsThresh float64 `json:"deviation_bps_thresh"`
}

// KillSwitchState represents the current kill switch status.
type KillSwitchState struct {
	Active      bool   `json:"active"`
	Reason      string `json:"reason,omitempty"`
	ActivatedAt int64  `json:"activated_at_ms,omitempty"`
}

// Store is a Redis-backed config store for the dashboard.
type Store struct {
	rdb       *redis.Client
	masterKey []byte
}

func NewStore(rdb *redis.Client, masterKey []byte) *Store {
	return &Store{rdb: rdb, masterKey: masterKey}
}

// SaveConnection encrypts and stores exchange API credentials.
func (s *Store) SaveConnection(ctx context.Context, exchange, apiKey, apiSecret, passphrase string) error {
	encKey, err := Encrypt(s.masterKey, apiKey)
	if err != nil {
		return fmt.Errorf("encrypt api_key: %w", err)
	}
	encSecret, err := Encrypt(s.masterKey, apiSecret)
	if err != nil {
		return fmt.Errorf("encrypt api_secret: %w", err)
	}
	encPass := ""
	if passphrase != "" {
		encPass, err = Encrypt(s.masterKey, passphrase)
		if err != nil {
			return fmt.Errorf("encrypt passphrase: %w", err)
		}
	}
	cfg := ConnectionConfig{
		Exchange:   exchange,
		APIKey:     encKey,
		APISecret:  encSecret,
		Passphrase: encPass,
		Configured: apiKey != "",
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, keyConnPrefix+exchange, data, 0).Err()
}

// GetConnections returns connection configs for the given exchanges with secrets masked.
func (s *Store) GetConnections(ctx context.Context, exchanges []string) ([]ConnectionConfig, error) {
	configs := make([]ConnectionConfig, 0, len(exchanges))
	for _, ex := range exchanges {
		data, err := s.rdb.Get(ctx, keyConnPrefix+ex).Bytes()
		if err == redis.Nil {
			configs = append(configs, ConnectionConfig{Exchange: ex, Configured: false})
			continue
		}
		if err != nil {
			return nil, err
		}
		var cfg ConnectionConfig
		if err := json.Unmarshal(data, &cfg); err != nil {
			return nil, err
		}
		// Mask secrets before returning to the client.
		if cfg.APIKey != "" {
			cfg.APIKey = "••••••••"
		}
		cfg.APISecret = ""
		cfg.Passphrase = ""
		configs = append(configs, cfg)
	}
	return configs, nil
}

// SaveAlertConfig persists alert configuration.
func (s *Store) SaveAlertConfig(ctx context.Context, cfg AlertConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, keyAlertConfig, data, 0).Err()
}

// GetAlertConfig retrieves alert configuration.
func (s *Store) GetAlertConfig(ctx context.Context) (AlertConfig, error) {
	data, err := s.rdb.Get(ctx, keyAlertConfig).Bytes()
	if err == redis.Nil {
		return AlertConfig{}, nil
	}
	if err != nil {
		return AlertConfig{}, err
	}
	var cfg AlertConfig
	return cfg, json.Unmarshal(data, &cfg)
}

// SetKillSwitch activates or deactivates the kill switch.
func (s *Store) SetKillSwitch(ctx context.Context, active bool, reason string) error {
	if !active {
		return s.rdb.Del(ctx, keyKillSwitch).Err()
	}
	state := KillSwitchState{
		Active:      true,
		Reason:      reason,
		ActivatedAt: time.Now().UnixMilli(),
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, keyKillSwitch, data, 0).Err()
}

// GetKillSwitch returns the current kill switch state.
func (s *Store) GetKillSwitch(ctx context.Context) (KillSwitchState, error) {
	data, err := s.rdb.Get(ctx, keyKillSwitch).Bytes()
	if err == redis.Nil {
		return KillSwitchState{Active: false}, nil
	}
	if err != nil {
		return KillSwitchState{}, err
	}
	var state KillSwitchState
	return state, json.Unmarshal(data, &state)
}

// FundingStageEntry represents a single symbol's stage override stored in Redis.
type FundingStageEntry struct {
	Symbol    string `json:"symbol"`
	Stage     string `json:"stage"`
	UpdatedMs int64  `json:"updated_ms"`
}

// GetFundingStages returns all dynamic stage overrides from Redis.
func (s *Store) GetFundingStages(ctx context.Context) ([]FundingStageEntry, error) {
	data, err := s.rdb.Get(ctx, keyFundingStages).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []FundingStageEntry
	return entries, json.Unmarshal(data, &entries)
}

// SetFundingStage updates a single symbol's stage in the stored list.
// If the symbol doesn't exist in the list, it's appended.
func (s *Store) SetFundingStage(ctx context.Context, symbol, stage string) error {
	entries, err := s.GetFundingStages(ctx)
	if err != nil {
		return err
	}
	now := time.Now().UnixMilli()
	found := false
	for i := range entries {
		if entries[i].Symbol == symbol {
			entries[i].Stage = stage
			entries[i].UpdatedMs = now
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, FundingStageEntry{
			Symbol:    symbol,
			Stage:     stage,
			UpdatedMs: now,
		})
	}
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	return s.rdb.Set(ctx, keyFundingStages, data, 0).Err()
}
