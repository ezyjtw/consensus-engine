package exchange

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
)

// HMACSha256 computes HMAC-SHA256 and returns the hex-encoded result.
// Used by Binance, OKX, and Bybit for request signing.
func HMACSha256(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Sha256Hash computes a plain SHA-256 hash and returns the hex-encoded result.
func Sha256Hash(data string) string {
	h := sha256.Sum256([]byte(data))
	return hex.EncodeToString(h[:])
}
