package onchain

import (
	"math/big"
	"testing"
	"time"
)

func TestWalletNonceManagement(t *testing.T) {
	w := NewWallet("0xABCD", 5)
	w.SetNonce(Ethereum, 10)

	n1 := w.NextNonce(Ethereum)
	n2 := w.NextNonce(Ethereum)

	if n1 != 10 {
		t.Errorf("first nonce: expected 10, got %d", n1)
	}
	if n2 != 11 {
		t.Errorf("second nonce: expected 11, got %d", n2)
	}
}

func TestWalletPendingTracking(t *testing.T) {
	w := NewWallet("0xABCD", 2)

	tx1 := &PendingTx{Hash: "0x01", ChainID: Ethereum, Nonce: 1}
	tx2 := &PendingTx{Hash: "0x02", ChainID: Ethereum, Nonce: 2}
	tx3 := &PendingTx{Hash: "0x03", ChainID: Ethereum, Nonce: 3}

	if err := w.TrackTx(tx1); err != nil {
		t.Fatal(err)
	}
	if err := w.TrackTx(tx2); err != nil {
		t.Fatal(err)
	}

	if w.PendingCount() != 2 {
		t.Errorf("expected 2 pending, got %d", w.PendingCount())
	}

	// Third should fail — max pending is 2
	if err := w.TrackTx(tx3); err == nil {
		t.Error("expected error when exceeding max pending")
	}

	// Confirm one, then third should work
	w.UpdateTxStatus("0x01", TxConfirmed, 3)
	if w.PendingCount() != 1 {
		t.Errorf("expected 1 pending after confirm, got %d", w.PendingCount())
	}
}

func TestNonceManagerGapFilling(t *testing.T) {
	nm := NewNonceManager()
	w := NewWallet("0xABCD", 10)
	w.SetNonce(Ethereum, 100)
	nm.RegisterWallet(w)

	// Acquire nonce 100
	n1, err := nm.AcquireNonce("0xABCD", Ethereum)
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 100 {
		t.Errorf("expected nonce 100, got %d", n1)
	}

	// Acquire nonce 101
	n2, _ := nm.AcquireNonce("0xABCD", Ethereum)
	if n2 != 101 {
		t.Errorf("expected nonce 101, got %d", n2)
	}

	// Release nonce 100 (tx failed)
	nm.ReleaseNonce(Ethereum, 100)

	// Next acquire should get 100 (gap fill)
	n3, _ := nm.AcquireNonce("0xABCD", Ethereum)
	if n3 != 100 {
		t.Errorf("expected gap-filled nonce 100, got %d", n3)
	}

	// Then back to sequential
	n4, _ := nm.AcquireNonce("0xABCD", Ethereum)
	if n4 != 102 {
		t.Errorf("expected nonce 102, got %d", n4)
	}
}

func TestTxBuilder(t *testing.T) {
	b := NewTxBuilder(300000)
	b.SetChainConfig(TxBuildConfig{ChainID: Ethereum, IsEIP1559: true})

	tx, err := b.BuildSwap(Ethereum, "0xRouter", "0xTokenIn", "0xTokenOut",
		big.NewInt(1000000), big.NewInt(990000), time.Now().Add(5*time.Minute).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if tx.ChainID != Ethereum {
		t.Errorf("expected chain 1, got %d", tx.ChainID)
	}
	if tx.GasLimit != 300000 {
		t.Errorf("expected gas limit 300000, got %d", tx.GasLimit)
	}

	// Test approval
	approveTx, err := b.BuildApproval(Ethereum, "0xToken", "0xSpender", big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	if approveTx.GasLimit != 60000 {
		t.Errorf("expected approval gas 60000, got %d", approveTx.GasLimit)
	}
}

func TestTxSimulator(t *testing.T) {
	sim := NewTxSimulator(100)
	tx := &UnsignedTx{
		ChainID:  Ethereum,
		To:       "0xRouter",
		GasLimit: 200000,
	}
	result := sim.Simulate(tx)
	if !result.Success {
		t.Error("expected simulation success")
	}
	if result.GasUsed != 140000 {
		t.Errorf("expected 140000 gas used (70%%), got %d", result.GasUsed)
	}
}

func TestWalletPurge(t *testing.T) {
	w := NewWallet("0xABCD", 10)
	tx := &PendingTx{Hash: "0x01", ChainID: Ethereum}
	_ = w.TrackTx(tx)
	w.UpdateTxStatus("0x01", TxConfirmed, 5)

	// Purge with zero cutoff — should remove immediately
	removed := w.PurgeConfirmed(0)
	if removed != 1 {
		t.Errorf("expected 1 purged, got %d", removed)
	}
}
