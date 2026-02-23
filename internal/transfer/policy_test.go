package transfer

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testConfig(t *testing.T) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "transfer_policy.yaml")
	content := `
manual_approval_required: false
max_per_transfer_usd: 50000
max_daily_usd: 200000
max_transfers_per_day: 10
new_address_cooloff_hours: 48
allowlist:
  - label: "Binance Hot Wallet"
    address: "0xBinanceHot"
    asset: "*"
    max_per_tx_usd: 0
    added_at: "2025-01-01"
  - label: "OKX Cold Wallet"
    address: "0xOKXCold"
    asset: "USDT"
    max_per_tx_usd: 25000
    added_at: "2025-01-01"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg, cfgPath
}

func testRequest(toAddr, asset string, amountUSD float64) Request {
	return Request{
		ID:          "req-1",
		TenantID:    "default",
		ToAddress:   toAddr,
		Asset:       asset,
		AmountUSD:   amountUSD,
		RequestedAt: time.Now().UTC(),
		RequestedBy: "test-user",
	}
}

// Test approved transfer to allowlisted address.
func TestApprovedTransfer(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	req := testRequest("0xBinanceHot", "USDT", 1000)
	decision := engine.Check(req)
	if decision.Status != StatusApproved {
		t.Errorf("expected APPROVED, got %s: %s", decision.Status, decision.Reason)
	}
}

// Test denied for non-allowlisted address.
func TestDeniedNotAllowlisted(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	req := testRequest("0xUnknownWallet", "USDT", 1000)
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("expected DENIED for non-allowlisted address, got %s", decision.Status)
	}
	if decision.DenialCode != "ADDRESS_NOT_ALLOWLISTED" {
		t.Errorf("expected ADDRESS_NOT_ALLOWLISTED, got %s", decision.DenialCode)
	}
}

// Test per-transfer cap (global).
func TestPerTransferCapGlobal(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	req := testRequest("0xBinanceHot", "USDT", 60000) // exceeds global 50k cap
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("expected DENIED for exceeding per-tx cap, got %s", decision.Status)
	}
	if decision.DenialCode != "EXCEEDS_PER_TX_CAP" {
		t.Errorf("expected EXCEEDS_PER_TX_CAP, got %s", decision.DenialCode)
	}
}

// Test per-transfer cap (allowlist entry override).
func TestPerTransferCapEntryOverride(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	// OKX Cold has max_per_tx_usd=25000, lower than global 50000.
	req := testRequest("0xOKXCold", "USDT", 30000)
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("expected DENIED for exceeding entry per-tx cap, got %s", decision.Status)
	}
	if decision.DenialCode != "EXCEEDS_PER_TX_CAP" {
		t.Errorf("expected EXCEEDS_PER_TX_CAP, got %s", decision.DenialCode)
	}
}

// Test asset-specific allowlist (OKX entry only allows USDT).
func TestAssetSpecificAllowlist(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	req := testRequest("0xOKXCold", "ETH", 1000) // OKX only allows USDT
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("expected DENIED for wrong asset, got %s", decision.Status)
	}
	if decision.DenialCode != "ADDRESS_NOT_ALLOWLISTED" {
		t.Errorf("expected ADDRESS_NOT_ALLOWLISTED, got %s", decision.DenialCode)
	}
}

// Test wildcard asset match.
func TestWildcardAssetMatch(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	// Binance has asset="*" so any asset should work.
	req := testRequest("0xBinanceHot", "ETH", 1000)
	decision := engine.Check(req)
	if decision.Status != StatusApproved {
		t.Errorf("expected APPROVED for wildcard asset, got %s: %s", decision.Status, decision.Reason)
	}
}

// Test daily transfer count limit.
func TestDailyTransferCountLimit(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	// MaxTransfersPerDay = 10.
	for i := 0; i < 10; i++ {
		req := testRequest("0xBinanceHot", "USDT", 100)
		req.ID = "req-" + string(rune('A'+i))
		decision := engine.Check(req)
		if decision.Status != StatusApproved {
			t.Fatalf("transfer %d should be approved, got %s: %s", i+1, decision.Status, decision.Reason)
		}
	}

	// 11th transfer should be denied.
	req := testRequest("0xBinanceHot", "USDT", 100)
	req.ID = "req-11"
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("11th transfer should be DENIED, got %s", decision.Status)
	}
	if decision.DenialCode != "DAILY_TRANSFER_COUNT_EXCEEDED" {
		t.Errorf("expected DAILY_TRANSFER_COUNT_EXCEEDED, got %s", decision.DenialCode)
	}
}

// Test daily notional limit.
func TestDailyNotionalLimit(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	// MaxDailyUSD = 200000. Send 5 transfers of 45000 = 225000.
	for i := 0; i < 4; i++ {
		req := testRequest("0xBinanceHot", "USDT", 45000)
		req.ID = "req-daily-" + string(rune('A'+i))
		decision := engine.Check(req)
		if decision.Status != StatusApproved {
			t.Fatalf("transfer %d should be approved (total=%d), got %s",
				i+1, (i+1)*45000, decision.Status)
		}
	}

	// 5th transfer would push total to 225000 > 200000.
	req := testRequest("0xBinanceHot", "USDT", 45000)
	req.ID = "req-daily-5"
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("should be DENIED (daily notional exceeded), got %s", decision.Status)
	}
	if decision.DenialCode != "DAILY_NOTIONAL_EXCEEDED" {
		t.Errorf("expected DAILY_NOTIONAL_EXCEEDED, got %s", decision.DenialCode)
	}
}

// Test tamper detection.
func TestTamperDetection(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	// Modify the config file on disk.
	if err := os.WriteFile(cfgPath, []byte("tampered: true"), 0644); err != nil {
		t.Fatalf("tamper write: %v", err)
	}

	req := testRequest("0xBinanceHot", "USDT", 100)
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("expected DENIED after tamper, got %s", decision.Status)
	}
	if decision.DenialCode != "CONFIG_TAMPERED" {
		t.Errorf("expected CONFIG_TAMPERED, got %s", decision.DenialCode)
	}
}

// Test new address cooloff.
func TestNewAddressCooloff(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "transfer_policy.yaml")
	// Address added today — should be in cooloff.
	today := time.Now().UTC().Format("2006-01-02")
	content := `
manual_approval_required: false
max_per_transfer_usd: 50000
max_daily_usd: 200000
max_transfers_per_day: 10
new_address_cooloff_hours: 48
allowlist:
  - label: "New Wallet"
    address: "0xNewWallet"
    asset: "*"
    added_at: "` + today + `"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	engine := New(cfg, cfgPath)

	req := testRequest("0xNewWallet", "USDT", 100)
	decision := engine.Check(req)
	if decision.Status != StatusDenied {
		t.Errorf("expected DENIED for new address cooloff, got %s: %s", decision.Status, decision.Reason)
	}
	if decision.DenialCode != "NEW_ADDRESS_COOLOFF" {
		t.Errorf("expected NEW_ADDRESS_COOLOFF, got %s", decision.DenialCode)
	}
}

// Test manual approval gate.
func TestManualApprovalGate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "transfer_policy.yaml")
	content := `
manual_approval_required: true
max_per_transfer_usd: 50000
max_daily_usd: 200000
max_transfers_per_day: 10
new_address_cooloff_hours: 0
allowlist:
  - label: "Hot Wallet"
    address: "0xHotWallet"
    asset: "*"
    added_at: "2024-01-01"
`
	if err := os.WriteFile(cfgPath, []byte(content), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	engine := New(cfg, cfgPath)

	req := testRequest("0xHotWallet", "USDT", 100)
	decision := engine.Check(req)
	if decision.Status != StatusPending {
		t.Errorf("expected PENDING_APPROVAL, got %s", decision.Status)
	}
	if !decision.RequiresApproval {
		t.Error("should require approval")
	}
}

// Test case-insensitive address matching.
func TestCaseInsensitiveAddress(t *testing.T) {
	cfg, cfgPath := testConfig(t)
	engine := New(cfg, cfgPath)

	req := testRequest("0xbinancehot", "USDT", 100) // lowercase
	decision := engine.Check(req)
	if decision.Status != StatusApproved {
		t.Errorf("case-insensitive match should approve, got %s: %s", decision.Status, decision.Reason)
	}
}

// Test config hash computation.
func TestConfigHashNonEmpty(t *testing.T) {
	cfg, _ := testConfig(t)
	hash := cfg.Hash()
	if hash == "" {
		t.Error("config hash should not be empty")
	}
	if len(hash) != 64 { // SHA-256 hex = 64 chars
		t.Errorf("config hash length should be 64, got %d", len(hash))
	}
}
