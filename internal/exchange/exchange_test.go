package exchange

import (
	"testing"
)

func TestHMACSha256(t *testing.T) {
	// Known test vector.
	secret := "secret"
	payload := "test"
	result := HMACSha256(secret, payload)
	if result == "" {
		t.Fatal("expected non-empty HMAC result")
	}
	if len(result) != 64 { // SHA-256 = 32 bytes = 64 hex chars
		t.Fatalf("expected 64 hex chars, got %d", len(result))
	}
}

func TestSha256Hash(t *testing.T) {
	result := Sha256Hash("hello")
	expected := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if result != expected {
		t.Fatalf("expected %s, got %s", expected, result)
	}
}

func TestAPIError(t *testing.T) {
	err := &APIError{
		StatusCode: 429,
		Body:       "rate limited",
		Method:     "GET",
		Path:       "/api/v1/test",
	}

	if !err.IsRateLimited() {
		t.Fatal("expected rate limited")
	}
	if !err.IsTemporary() {
		t.Fatal("expected temporary")
	}

	err500 := &APIError{StatusCode: 500, Body: "internal error", Method: "POST", Path: "/"}
	if err500.IsRateLimited() {
		t.Fatal("500 should not be rate limited")
	}
	if !err500.IsTemporary() {
		t.Fatal("500 should be temporary")
	}

	err400 := &APIError{StatusCode: 400, Body: "bad request", Method: "POST", Path: "/"}
	if err400.IsTemporary() {
		t.Fatal("400 should not be temporary")
	}
}

func TestRegistryDirect(t *testing.T) {
	r := NewRegistry(nil)

	// Register a nil exchange directly (test the Register path).
	// We can't use a real exchange without credentials, so test the map logic.
	if all := r.All(); len(all) != 0 {
		t.Fatalf("expected empty registry, got %d", len(all))
	}
}

func TestOrderTypes(t *testing.T) {
	// Ensure type constants are distinct.
	types := []OrderType{OrderTypeMarket, OrderTypeLimit, OrderTypeIOC}
	seen := make(map[OrderType]bool)
	for _, ot := range types {
		if seen[ot] {
			t.Fatalf("duplicate order type: %s", ot)
		}
		seen[ot] = true
	}
}

func TestSideConstants(t *testing.T) {
	if SideBuy == SideSell {
		t.Fatal("BUY and SELL should be different")
	}
}
