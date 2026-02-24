package exchange

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/redis/go-redis/v9"
)

const keyConnPrefix = "dashboard:conn:"

// Credentials holds decrypted API credentials for one exchange.
type Credentials struct {
	Exchange   string
	APIKey     string
	APISecret  string
	Passphrase string // OKX, Deribit
}

// CredentialStore retrieves and decrypts exchange API credentials from Redis.
// Credentials are cached after first retrieval.
type CredentialStore struct {
	rdb       *redis.Client
	masterKey []byte
	mu        sync.RWMutex
	cache     map[string]*Credentials
}

// NewCredentialStore creates a store that reads encrypted credentials from Redis.
func NewCredentialStore(rdb *redis.Client, masterKey []byte) *CredentialStore {
	return &CredentialStore{
		rdb:       rdb,
		masterKey: masterKey,
		cache:     make(map[string]*Credentials),
	}
}

// Get returns decrypted credentials for the named exchange.
// Results are cached; call Refresh to clear the cache.
func (cs *CredentialStore) Get(ctx context.Context, exchange string) (*Credentials, error) {
	cs.mu.RLock()
	if cred, ok := cs.cache[exchange]; ok {
		cs.mu.RUnlock()
		return cred, nil
	}
	cs.mu.RUnlock()

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Double-check after acquiring write lock.
	if cred, ok := cs.cache[exchange]; ok {
		return cred, nil
	}

	data, err := cs.rdb.Get(ctx, keyConnPrefix+exchange).Bytes()
	if err == redis.Nil {
		return nil, fmt.Errorf("no credentials configured for %s", exchange)
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials for %s: %w", exchange, err)
	}

	var stored struct {
		Exchange   string `json:"exchange"`
		APIKey     string `json:"api_key"`
		APISecret  string `json:"api_secret"`
		Passphrase string `json:"passphrase"`
		Configured bool   `json:"configured"`
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		return nil, fmt.Errorf("parsing credentials for %s: %w", exchange, err)
	}
	if !stored.Configured {
		return nil, fmt.Errorf("credentials for %s are not configured", exchange)
	}

	apiKey, err := decryptAESGCM(cs.masterKey, stored.APIKey)
	if err != nil {
		return nil, fmt.Errorf("decrypting api_key for %s: %w", exchange, err)
	}
	apiSecret, err := decryptAESGCM(cs.masterKey, stored.APISecret)
	if err != nil {
		return nil, fmt.Errorf("decrypting api_secret for %s: %w", exchange, err)
	}
	passphrase := ""
	if stored.Passphrase != "" {
		passphrase, err = decryptAESGCM(cs.masterKey, stored.Passphrase)
		if err != nil {
			return nil, fmt.Errorf("decrypting passphrase for %s: %w", exchange, err)
		}
	}

	cred := &Credentials{
		Exchange:   exchange,
		APIKey:     apiKey,
		APISecret:  apiSecret,
		Passphrase: passphrase,
	}
	cs.cache[exchange] = cred
	return cred, nil
}

// Refresh clears the credential cache so the next Get re-reads from Redis.
func (cs *CredentialStore) Refresh() {
	cs.mu.Lock()
	cs.cache = make(map[string]*Credentials)
	cs.mu.Unlock()
}

// decryptAESGCM decrypts a hex-encoded nonce+ciphertext using AES-256-GCM.
// Compatible with the dashboard Encrypt function.
func decryptAESGCM(key []byte, cipherHex string) (string, error) {
	data, err := hex.DecodeString(cipherHex)
	if err != nil {
		return "", fmt.Errorf("invalid hex: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(data) < gcm.NonceSize() {
		return "", fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertext := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}
